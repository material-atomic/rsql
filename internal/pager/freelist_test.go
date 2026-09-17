package pager

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/material-atomic/rsql/internal/vfs"
)

// fill writes a page of `mark` and returns its id.
func fill(t *testing.T, pager *Pager, mark byte) uint64 {
	t.Helper()
	id, err := pager.Allocate()
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	page := pager.NewPage(id, KindLeaf)
	page.Payload()[0] = mark
	if err := pager.Write(page); err != nil {
		t.Fatalf("write: %v", err)
	}
	return id
}

func TestAPageFreedByTheRunningTransactionWaitsForItsCommit(t *testing.T) {
	disk := vfs.NewSim(30, vfs.Faults{})
	pager, err := Create(disk, 0)
	if err != nil {
		t.Fatal(err)
	}

	first, second := fill(t, pager, 'a'), fill(t, pager, 'b')
	if err := pager.Commit(first); err != nil {
		t.Fatal(err)
	}
	grown := pager.Meta().PageCount

	// The last committed state still points at this page, and a crash now would
	// come back to that state, so it may not be handed out.
	pager.Free(second)
	again, err := pager.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if again == second {
		t.Fatal("a page freed by the transaction in progress was handed straight back out")
	}
	if pager.Meta().PageCount <= grown {
		t.Error("the file should have grown instead of reusing anything")
	}

	if err := pager.Commit(first); err != nil {
		t.Fatal(err)
	}
	if got, err := pager.Allocate(); err != nil || got != second {
		t.Errorf("after the commit the page should come back: got %d, want %d (%v)", got, second, err)
	}
}

func TestAPageThisTransactionAllocatedComesStraightBack(t *testing.T) {
	disk := vfs.NewSim(31, vfs.Faults{})
	pager, err := Create(disk, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Nothing durable and no reader has ever seen it, so there is nothing to
	// wait for.
	id := fill(t, pager, 'a')
	pager.Free(id)

	again, err := pager.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Errorf("allocated %d; the page this transaction just gave up was %d", again, id)
	}
}

func TestAHeldSnapshotKeepsItsPages(t *testing.T) {
	disk := vfs.NewSim(32, vfs.Faults{})
	pager, err := Create(disk, 0)
	if err != nil {
		t.Fatal(err)
	}

	root, doomed := fill(t, pager, 'a'), fill(t, pager, 'b')
	if err := pager.Commit(root); err != nil {
		t.Fatal(err)
	}

	// A reader of the state that still contains the page.
	snapshot := pager.Snapshot()

	pager.Free(doomed)
	if err := pager.Commit(root); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		got, err := pager.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		if got == doomed {
			t.Fatalf("a page a held snapshot can still see was handed out")
		}
	}
	if err := pager.Commit(root); err != nil {
		t.Fatal(err)
	}

	// Let go, and it becomes ordinary free space.
	snapshot.Release()

	handed := map[uint64]bool{}
	for i := 0; i < 4; i++ {
		got, err := pager.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		handed[got] = true
	}
	if !handed[doomed] {
		t.Errorf("after the snapshot was released the page never came back: got %v, want %d", handed, doomed)
	}
}

func TestReleasingASnapshotTwiceIsHarmless(t *testing.T) {
	disk := vfs.NewSim(33, vfs.Faults{})
	pager, err := Create(disk, 0)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := pager.Snapshot()
	snapshot.Release()
	snapshot.Release()

	if held := pager.oldestHeld(); held != 0 {
		t.Errorf("a released snapshot is still held at %d", held)
	}
}

func TestTheFreeListSurvivesARestart(t *testing.T) {
	disk := vfs.NewSim(34, vfs.Faults{})
	pager, err := Create(disk, 0)
	if err != nil {
		t.Fatal(err)
	}

	ids := make([]uint64, 0, 600)
	for i := 0; i < 600; i++ {
		ids = append(ids, fill(t, pager, byte(i)))
	}
	if err := pager.Commit(ids[0]); err != nil {
		t.Fatal(err)
	}

	// Enough freed pages to need more than one list page.
	for _, id := range ids[1:] {
		pager.Free(id)
	}
	if err := pager.Commit(ids[0]); err != nil {
		t.Fatal(err)
	}
	wanted := pager.FreePages()
	if wanted != 599 {
		t.Fatalf("the list holds %d pages, want the 599 that were freed", wanted)
	}
	size := pager.Meta().PageCount

	restart := vfs.NewSim(34, vfs.Faults{})
	restart.Restore(disk.Durable())
	reopened, err := Open(restart, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := reopened.FreePages(); got != wanted {
		t.Errorf("the reopened list holds %d pages, want %d", got, wanted)
	}

	// And it is used rather than remembered: allocating does not grow the file.
	for i := 0; i < 500; i++ {
		if _, err := reopened.Allocate(); err != nil {
			t.Fatal(err)
		}
	}
	if grown := reopened.Meta().PageCount; grown != size {
		t.Errorf("the file grew from %d to %d pages while %d were free", size, grown, wanted)
	}
}

// damage rewrites one page of a durable image and reseals it, so that what a
// restart reads is damage the checksum agrees with — which is the only kind
// the free list has to defend itself against.
func damage(image []byte, id uint64, change func(kind *uint8, payload []byte)) {
	page := image[int(id)*PageBytes : (int(id)+1)*PageBytes]
	kind := page[offKind]
	change(&kind, page[HeaderBytes:])
	seal(page, id, kind)
}

func TestAFreeListThatDoesNotReadBackIsRefused(t *testing.T) {
	breakages := map[string]func(kind *uint8, payload []byte){
		"a list page that is not a list page": func(kind *uint8, payload []byte) {
			*kind = KindLeaf
		},
		"more entries than a page holds": func(kind *uint8, payload []byte) {
			binary.BigEndian.PutUint32(payload[freeCount:], uint32(freePerPage+1))
		},
		"it runs off the end of the file": func(kind *uint8, payload []byte) {
			binary.BigEndian.PutUint64(payload[freeNext:], 1<<40)
		},
		"it holds a page the file does not have": func(kind *uint8, payload []byte) {
			binary.BigEndian.PutUint64(payload[freeHeader+8:], 1<<40)
		},
		"it points back at a meta page": func(kind *uint8, payload []byte) {
			binary.BigEndian.PutUint64(payload[freeNext:], 1)
		},
	}

	for name, break_ := range breakages {
		t.Run(name, func(t *testing.T) {
			disk := vfs.NewSim(35, vfs.Faults{})
			pager, err := Create(disk, 0)
			if err != nil {
				t.Fatal(err)
			}
			ids := []uint64{fill(t, pager, 'a'), fill(t, pager, 'b'), fill(t, pager, 'c')}
			if err := pager.Commit(ids[0]); err != nil {
				t.Fatal(err)
			}
			pager.Free(ids[1])
			pager.Free(ids[2])
			if err := pager.Commit(ids[0]); err != nil { // writes the list
				t.Fatal(err)
			}
			if err := pager.Commit(ids[0]); err != nil { // and again, so it is the durable one
				t.Fatal(err)
			}

			head := pager.Meta().Freelist
			if head == 0 {
				t.Fatal("this test needs a list on disk")
			}

			image := disk.Durable()
			damage(image, head, break_)
			restart := vfs.NewSim(35, vfs.Faults{})
			restart.Restore(image)

			if _, err := Open(restart, 0); !errors.Is(err, ErrFreelist) {
				t.Errorf("want ErrFreelist, got %v", err)
			}
		})
	}
}

func TestAPageIsNotHandedOutTwiceBecauseItWasFreedTwice(t *testing.T) {
	disk := vfs.NewSim(36, vfs.Faults{})
	pager, err := Create(disk, 0)
	if err != nil {
		t.Fatal(err)
	}

	root, doomed := fill(t, pager, 'a'), fill(t, pager, 'b')
	if err := pager.Commit(root); err != nil {
		t.Fatal(err)
	}

	// Two paths through the tree can arrive at the same dead page; the list has
	// to record it once, or two callers are handed the same page.
	pager.Free(doomed)
	pager.Free(doomed)
	if err := pager.Commit(root); err != nil {
		t.Fatal(err)
	}

	first, err := pager.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	second, err := pager.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Errorf("page %d was handed out twice", first)
	}
}
