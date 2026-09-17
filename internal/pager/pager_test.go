package pager

import (
	"bytes"
	"errors"
	"testing"

	"github.com/material-atomic/rsql/internal/vfs"
)

func fresh(t *testing.T, faults vfs.Faults, seed int64) (*vfs.SimDisk, *Pager) {
	t.Helper()
	disk := vfs.NewSim(seed, faults)
	pager, err := Create(disk, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return disk, pager
}

// writePage puts one page of `fill` into the file and returns its id.
func writePage(t *testing.T, pager *Pager, fill byte) uint64 {
	t.Helper()
	id, err := pager.Allocate()
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	page := pager.NewPage(id, KindLeaf)
	for i := range page.Payload() {
		page.Payload()[i] = fill
	}
	if err := pager.Write(page); err != nil {
		t.Fatalf("write: %v", err)
	}
	return id
}

func TestAFreshFileHasTwoMetaPagesAndOpens(t *testing.T) {
	disk, pager := fresh(t, vfs.Faults{}, 1)

	if meta := pager.Meta(); meta.TxID != 1 || meta.PageCount != 2 {
		t.Fatalf("a fresh file: %+v", meta)
	}

	reopened, err := Open(disk, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if reopened.Meta() != pager.Meta() {
		t.Errorf("reopened as %+v, was %+v", reopened.Meta(), pager.Meta())
	}
}

func TestACommittedPageIsThereAfterACrash(t *testing.T) {
	disk, pager := fresh(t, vfs.Faults{}, 2)

	id := writePage(t, pager, 'a')
	if err := pager.Commit(id); err != nil {
		t.Fatalf("commit: %v", err)
	}

	after, err := Open(disk.Crash(), 0)
	if err != nil {
		t.Fatalf("open after crash: %v", err)
	}
	if after.Meta().Root != id || after.Meta().TxID != 2 {
		t.Fatalf("after a crash: %+v", after.Meta())
	}

	page, err := after.Read(id)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if page.Kind != KindLeaf || page.Payload()[0] != 'a' {
		t.Error("the committed page is not what was written")
	}
}

// The property the whole design rests on: a crash leaves a whole transaction,
// the previous one or the new one, and never a mixture.
func TestACrashMidCommitLeavesThePreviousTransactionWhole(t *testing.T) {
	for cut := 1; cut <= 6; cut++ {
		disk := vfs.NewSim(int64(cut), vfs.Faults{})
		pager, err := Create(disk, 0)
		if err != nil {
			t.Fatal(err)
		}

		first := writePage(t, pager, 'a')
		if err := pager.Commit(first); err != nil {
			t.Fatal(err)
		}

		// A second transaction, with the power cut somewhere inside it.
		disk2 := vfs.NewSim(int64(cut), vfs.Faults{PowerCutAfter: cut})
		disk2.Restore(disk.Durable())
		second, err := Open(disk2, 0)
		if err != nil {
			t.Fatal(err)
		}

		id := writePage2(second, 'b')
		_ = second.Commit(id)

		after, err := Open(disk2.Crash(), 0)
		if err != nil {
			t.Fatalf("cut %d: the file must still open: %v", cut, err)
		}

		meta := after.Meta()
		if meta.TxID != 2 && meta.TxID != 3 {
			t.Fatalf("cut %d: txid %d is neither transaction", cut, meta.TxID)
		}

		// Whichever transaction survived, its root must be readable.
		page, err := after.Read(meta.Root)
		if err != nil {
			t.Fatalf("cut %d: the surviving root does not read: %v", cut, err)
		}
		want := byte('a')
		if meta.TxID == 3 {
			want = 'b'
		}
		if page.Payload()[0] != want {
			t.Errorf("cut %d: txid %d points at a page of %q", cut, meta.TxID, page.Payload()[0])
		}
	}
}

// writePage2 is writePage without a *testing.T, for loops that handle errors themselves.
func writePage2(pager *Pager, fill byte) uint64 {
	id, err := pager.Allocate()
	if err != nil {
		return 0
	}
	page := pager.NewPage(id, KindLeaf)
	for i := range page.Payload() {
		page.Payload()[i] = fill
	}
	_ = pager.Write(page)
	return id
}

func TestAPageChangedOnDiskIsRefused(t *testing.T) {
	disk, pager := fresh(t, vfs.Faults{}, 3)
	id := writePage(t, pager, 'z')
	if err := pager.Commit(id); err != nil {
		t.Fatal(err)
	}

	// One byte, in the middle of the page: what a bad cable or a bad sector does.
	image := disk.Durable()
	image[int(id)*PageBytes+HeaderBytes+17] ^= 0x40
	damaged := vfs.NewSim(3, vfs.Faults{})
	damaged.Restore(image)

	reopened, err := Open(damaged, 0)
	if err != nil {
		t.Fatalf("the meta pages are untouched, so it must open: %v", err)
	}
	if _, err := reopened.Read(id); !errors.Is(err, ErrChecksum) {
		t.Errorf("want ErrChecksum, got %v", err)
	}
}

func TestAPageThatIsTheWrongPageIsRefused(t *testing.T) {
	disk, pager := fresh(t, vfs.Faults{}, 4)
	first := writePage(t, pager, '1')
	second := writePage(t, pager, '2')
	if err := pager.Commit(first); err != nil {
		t.Fatal(err)
	}

	// Page 3's bytes, whole and checksummed, sitting where page 2 belongs:
	// a stale pointer, or a filesystem that moved a block.
	image := disk.Durable()
	copy(image[int(first)*PageBytes:(int(first)+1)*PageBytes], image[int(second)*PageBytes:(int(second)+1)*PageBytes])
	moved := vfs.NewSim(4, vfs.Faults{})
	moved.Restore(image)

	reopened, err := Open(moved, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Read(first); !errors.Is(err, ErrChecksum) {
		t.Errorf("a whole page in the wrong place must be refused, got %v", err)
	}
}

func TestOneDamagedMetaPageIsSurvivable(t *testing.T) {
	disk, pager := fresh(t, vfs.Faults{}, 5)
	first := writePage(t, pager, 'a')
	if err := pager.Commit(first); err != nil { // writes meta 0
		t.Fatal(err)
	}
	second := writePage(t, pager, 'b')
	if err := pager.Commit(second); err != nil { // writes meta 1
		t.Fatal(err)
	}

	// The newer meta page is damaged: the older one is a whole transaction.
	image := disk.Durable()
	image[PageBytes+HeaderBytes+3] ^= 0xff
	damaged := vfs.NewSim(5, vfs.Faults{})
	damaged.Restore(image)

	after, err := Open(damaged, 0)
	if err != nil {
		t.Fatalf("one good meta page is enough: %v", err)
	}
	if after.Meta().Root != first {
		t.Errorf("fell back to root %d, want the older transaction's %d", after.Meta().Root, first)
	}
}

// The commit protocol rests on a meta page landing in one piece, and a disk
// promises that within one sector and nowhere else. Nothing in the code stops
// the layout from growing past that, so it is checked here.
func TestAMetaWriteNeverCrossesASector(t *testing.T) {
	if MetaBytes > vfs.SectorBytes {
		t.Fatalf("a meta page is %d bytes, wider than the %d a disk writes atomically", MetaBytes, vfs.SectorBytes)
	}
	if PageBytes%vfs.SectorBytes != 0 {
		t.Fatalf("pages of %d bytes do not sit on %d-byte sector boundaries", PageBytes, vfs.SectorBytes)
	}
	// Both meta pages start at a page boundary, so a meta write that fits in
	// MetaBytes lies inside the first sector of its page.
	if offPageCount+8 > MetaBytes {
		t.Fatalf("the meta layout runs to %d bytes, past the %d that are written", offPageCount+8, MetaBytes)
	}
}

// A restart must find the transaction that last completed, whichever of the two
// meta pages it happened to land in.
func TestOpeningTakesTheNewerTransaction(t *testing.T) {
	disk, pager := fresh(t, vfs.Faults{}, 11)

	for _, fill := range []byte{'a', 'b', 'c'} {
		id := writePage(t, pager, fill)
		if err := pager.Commit(id); err != nil {
			t.Fatal(err)
		}

		restarted := vfs.NewSim(11, vfs.Faults{})
		restarted.Restore(disk.Durable())
		after, err := Open(restarted, 0)
		if err != nil {
			t.Fatalf("%q: open: %v", fill, err)
		}
		if after.Meta() != pager.Meta() {
			t.Fatalf("%q: a restart sees %+v, the last commit left %+v", fill, after.Meta(), pager.Meta())
		}

		page, err := after.Read(after.Meta().Root)
		if err != nil {
			t.Fatalf("%q: read the root: %v", fill, err)
		}
		if page.Payload()[0] != fill {
			t.Errorf("%q: the root reads as %q", fill, page.Payload()[0])
		}
	}
}

func TestNeitherMetaPageReadableIsAnError(t *testing.T) {
	disk, pager := fresh(t, vfs.Faults{}, 6)
	id := writePage(t, pager, 'a')
	if err := pager.Commit(id); err != nil {
		t.Fatal(err)
	}

	// Damage inside the payload of both, leaving the magic intact: this is our
	// file, and it is our file that is broken.
	image := disk.Durable()
	image[offRoot+3] ^= 0xff
	image[PageBytes+offRoot+3] ^= 0xff
	broken := vfs.NewSim(6, vfs.Faults{})
	broken.Restore(image)

	if _, err := Open(broken, 0); !errors.Is(err, ErrNoMeta) {
		t.Errorf("want ErrNoMeta, got %v", err)
	}
}

func TestAFileThatIsNotOursIsNotOpened(t *testing.T) {
	disk := vfs.NewSim(7, vfs.Faults{})
	disk.Restore(bytes.Repeat([]byte("this is a jpeg, honestly"), 1000))

	if _, err := Open(disk, 0); !errors.Is(err, ErrNotRsql) {
		t.Errorf("want ErrNotRsql, got %v", err)
	}
}

func TestASyncThatLiedCostsTheTransactionButNotTheFile(t *testing.T) {
	disk := vfs.NewSim(8, vfs.Faults{})
	pager, err := Create(disk, 0)
	if err != nil {
		t.Fatal(err)
	}
	first := writePage(t, pager, 'a')
	if err := pager.Commit(first); err != nil {
		t.Fatal(err)
	}

	// From here on every sync lies, so nothing more becomes durable.
	lying := vfs.NewSim(8, vfs.Faults{LyingSync: 1})
	lying.Restore(disk.Durable())
	second, err := Open(lying, 0)
	if err != nil {
		t.Fatal(err)
	}
	id := writePage2(second, 'b')
	if err := second.Commit(id); err != nil {
		t.Fatal(err)
	}

	after, err := Open(lying.Crash(), 0)
	if err != nil {
		t.Fatalf("a lying sync must not break the file: %v", err)
	}
	if after.Meta().Root != first {
		t.Errorf("root is %d; the second transaction was never durable, so it must be %d", after.Meta().Root, first)
	}
}

func TestQuotaIsEnforcedWhereThePagesAreHandedOut(t *testing.T) {
	disk := vfs.NewSim(9, vfs.Faults{})
	pager, err := Create(disk, 4) // two meta pages plus two of data
	if err != nil {
		t.Fatal(err)
	}

	writePage(t, pager, 'a')
	writePage(t, pager, 'b')

	if _, err := pager.Allocate(); !errors.Is(err, ErrQuota) {
		t.Errorf("want ErrQuota, got %v", err)
	}
}

func TestMetaPagesAreNotWrittenByHand(t *testing.T) {
	_, pager := fresh(t, vfs.Faults{}, 10)

	if err := pager.Write(pager.NewPage(0, KindLeaf)); !errors.Is(err, ErrReadOnlyPage) {
		t.Errorf("want ErrReadOnlyPage, got %v", err)
	}
	if _, err := pager.Read(99); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("want ErrOutOfRange, got %v", err)
	}
}

// The guarantee, stated twice: once for a disk that keeps the one promise a
// disk makes, and once for a disk that does not.
//
// A power cut can lose any write that was not synced, land them in another
// order, or land one of them half done. Through all of that a restart must find
// a file it opens and a root it reads — the single invariant everything above
// the pager is allowed to assume.
func TestWithHonestSyncsACommittedRootAlwaysReads(t *testing.T) {
	for seed := int64(0); seed < 60; seed++ {
		crashing, root := crashDuringWork(t, seed, vfs.Faults{
			TornWrite: 0.5, ReorderWrites: true, PowerCutAfter: int(seed%7) + 1,
		})

		after, err := Open(crashing, 0)
		if err != nil {
			t.Fatalf("seed %d: the file must open after any crash: %v\n%v", seed, err, crashing.Trace)
		}

		page, err := after.Read(after.Meta().Root)
		if err != nil {
			t.Fatalf("seed %d: the surviving root must read: %v\n%v", seed, err, crashing.Trace)
		}
		if page.Kind != KindLeaf || !isOneOfOurs(page.Payload()[0]) {
			t.Fatalf("seed %d: the root is kind %d, payload %q", seed, page.Kind, page.Payload()[0])
		}
		if after.Meta().Root == root && page.Payload()[0] != 'a' {
			t.Fatalf("seed %d: the first transaction's root reads as %q", seed, page.Payload()[0])
		}
	}
}

// A sync that lies breaks the one promise the commit protocol is built on, so
// the transaction it was covering can be lost — no engine survives that, and
// pretending otherwise would be the lie. What must still hold is that the file
// opens and says so cleanly: an error naming the damage, never a page of
// whatever happened to be on the disk.
func TestALyingSyncCostsDataButNeverLeavesAnUnreadableFile(t *testing.T) {
	lost := 0

	for seed := int64(0); seed < 60; seed++ {
		crashing, _ := crashDuringWork(t, seed, vfs.Faults{
			TornWrite: 0.5, LyingSync: 0.3, ReorderWrites: true, PowerCutAfter: int(seed%7) + 1,
		})

		after, err := Open(crashing, 0)
		if err != nil {
			t.Fatalf("seed %d: the file must open even when a sync lied: %v\n%v", seed, err, crashing.Trace)
		}

		page, err := after.Read(after.Meta().Root)
		switch {
		case err == nil:
			if page.Kind != KindLeaf || !isOneOfOurs(page.Payload()[0]) {
				t.Fatalf("seed %d: read a page that is not one of ours: kind %d, %q\n%v",
					seed, page.Kind, page.Payload()[0], crashing.Trace)
			}
		case errors.Is(err, ErrChecksum), errors.Is(err, ErrOutOfRange), errors.Is(err, ErrTruncated):
			lost++
		default:
			t.Fatalf("seed %d: the damage must be named: %v\n%v", seed, err, crashing.Trace)
		}
	}

	if lost == 0 {
		t.Error("over 60 seeds a lying sync never cost a transaction, so this proves nothing")
	}
}

// crashDuringWork commits one page on a sound disk, then does more work on a
// disk with the given faults and cuts the power. It hands back the disk a
// restart would find, and the root of the transaction that was already durable.
func crashDuringWork(t *testing.T, seed int64, faults vfs.Faults) (*vfs.SimDisk, uint64) {
	t.Helper()

	disk := vfs.NewSim(seed, vfs.Faults{})
	pager, err := Create(disk, 0)
	if err != nil {
		t.Fatal(err)
	}
	root := writePage2(pager, 'a')
	if err := pager.Commit(root); err != nil {
		t.Fatal(err)
	}

	crashing := vfs.NewSim(seed, faults)
	crashing.Restore(disk.Durable())

	if working, err := Open(crashing, 0); err == nil {
		for i := 0; i < 3; i++ {
			id := writePage2(working, byte('b'+i))
			_ = working.Commit(id)
		}
	}

	return crashing.Crash(), root
}

func isOneOfOurs(fill byte) bool { return fill >= 'a' && fill <= 'd' }
