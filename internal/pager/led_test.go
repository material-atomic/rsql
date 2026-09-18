package pager

import (
	"bytes"
	"errors"
	"testing"

	"github.com/material-atomic/rsql/internal/vfs"
)

// filled writes a page of one repeated byte and returns its id.
func filled(t *testing.T, pages *Pager, with byte) uint64 {
	t.Helper()

	id, err := pages.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	page := pages.NewPage(id, KindLeaf)
	for i := range page.Payload() {
		page.Payload()[i] = with
	}
	if err := pages.Write(page); err != nil {
		t.Fatal(err)
	}
	return id
}

func holds(t *testing.T, pages *Pager, id uint64, with byte) {
	t.Helper()

	page, err := pages.Read(id)
	if err != nil {
		t.Fatalf("page %d: %v", id, err)
	}
	wanted := bytes.Repeat([]byte{with}, len(page.Payload()))
	if !bytes.Equal(page.Payload(), wanted) {
		t.Errorf("page %d holds %x…, want %x…", id, page.Payload()[:8], wanted[:8])
	}
}

// TestALedStoreIsWhereItsLeaderSaysItIs: a led file keeps pages and no opinion
// about which transaction they belong to. Opening it means being told.
func TestALedStoreIsWhereItsLeaderSaysItIs(t *testing.T) {
	disk := vfs.NewSim(1, vfs.Faults{})
	pages, err := CreateLed(disk, Options{})
	if err != nil {
		t.Fatal(err)
	}

	first := filled(t, pages, 0xAA)
	at, err := pages.Flush(first)
	if err != nil {
		t.Fatal(err)
	}
	if at.Root != first {
		t.Errorf("flushed at root %d, want %d", at.Root, first)
	}

	second := filled(t, pages, 0xBB)
	later, err := pages.Flush(second)
	if err != nil {
		t.Fatal(err)
	}

	// Opened at the first transaction, this file is the first transaction —
	// even though the second is sitting in it, written and synced.
	restart := vfs.NewSim(1, vfs.Faults{})
	restart.Restore(disk.Durable())
	back, err := OpenLed(restart, at, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if back.Meta().Root != first {
		t.Errorf("opened at root %d, want %d", back.Meta().Root, first)
	}
	holds(t, back, first, 0xAA)

	// And opened at the second, it is the second. Nothing in the file decided
	// this; the leader did.
	restart = vfs.NewSim(1, vfs.Faults{})
	restart.Restore(disk.Durable())
	forward, err := OpenLed(restart, later, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if forward.Meta().Root != second {
		t.Errorf("opened at root %d, want %d", forward.Meta().Root, second)
	}
	holds(t, forward, second, 0xBB)
}

// TestALedStoreWillNotCommitForItself is the whole point stated as a rule.
// Two commit points is a recovery path, and there is no recovery path here.
func TestALedStoreWillNotCommitForItself(t *testing.T) {
	led, err := CreateLed(vfs.NewSim(2, vfs.Faults{}), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := led.Commit(0); !errors.Is(err, ErrLed) {
		t.Errorf("a led store committed: %v", err)
	}

	leader, err := Create(vfs.NewSim(3, vfs.Faults{}), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leader.Flush(0); !errors.Is(err, ErrNotLed) {
		t.Errorf("a leader flushed as though led: %v", err)
	}
	if err := leader.Abandon(Led{}); !errors.Is(err, ErrNotLed) {
		t.Errorf("a leader was abandoned as though led: %v", err)
	}
}

// TestPagesWrittenPastWhereTheLeaderSaysAreNotThere: a crash between a led
// store's sync and its leader's commit leaves pages in the file that nothing
// points at. That is the ordinary mid-transaction state of a single file, and
// it has the same answer: the next transaction allocates over them.
func TestPagesWrittenPastWhereTheLeaderSaysAreNotThere(t *testing.T) {
	disk := vfs.NewSim(4, vfs.Faults{})
	pages, err := CreateLed(disk, Options{})
	if err != nil {
		t.Fatal(err)
	}

	kept := filled(t, pages, 0x11)
	committed, err := pages.Flush(kept)
	if err != nil {
		t.Fatal(err)
	}

	// A transaction that got as far as the disk, and whose leader never said so.
	lost := filled(t, pages, 0x22)
	if _, err := pages.Flush(lost); err != nil {
		t.Fatal(err)
	}

	restart := vfs.NewSim(4, vfs.Faults{})
	restart.Restore(disk.Durable())
	back, err := OpenLed(restart, committed, Options{})
	if err != nil {
		t.Fatal(err)
	}

	// The page the abandoned transaction wrote is handed out again, because as
	// far as this database is concerned it was never allocated.
	again, err := back.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if again != lost {
		t.Errorf("the next page is %d; the abandoned transaction had taken %d", again, lost)
	}
	holds(t, back, kept, 0x11)
}

// TestALedStoreSaysWhetherItIsOneOfOurs: without a label, a file dropped into
// the directory would be read as pages, and "wrong key" and "not our file"
// would be one answer.
func TestALedStoreSaysWhetherItIsOneOfOurs(t *testing.T) {
	// Something else entirely.
	stranger := vfs.NewSim(5, vfs.Faults{})
	if _, err := stranger.WriteAt(bytes.Repeat([]byte{0x7F}, PageBytes), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLed(stranger, Led{}, Options{}); !errors.Is(err, ErrNotRsql) {
		t.Errorf("a file that is not ours opened as one: %v", err)
	}

	// A leader, opened as though it were led. The kinds differ, so this is not
	// reported as damage.
	leaderDisk := vfs.NewSim(6, vfs.Faults{})
	if _, err := Create(leaderDisk, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLed(leaderDisk, Led{}, Options{}); !errors.Is(err, ErrPageKind) {
		t.Errorf("a leader opened as a led store: %v", err)
	}

	// And a led store keeps the key question answerable.
	key := bytes.Repeat([]byte{0x5A}, 32)
	locked := vfs.NewSim(7, vfs.Faults{})
	sealed, err := CreateLed(locked, Options{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	secret := filled(t, sealed, 0x99)
	at, err := sealed.Flush(secret)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := OpenLed(locked, at, Options{}); !errors.Is(err, ErrKey) {
		t.Errorf("an encrypted led store opened with no key: %v", err)
	}
	wrong := bytes.Repeat([]byte{0x5B}, 32)
	if _, err := OpenLed(locked, at, Options{Key: wrong}); err == nil {
		t.Error("an encrypted led store opened under the wrong key")
	}
	right, err := OpenLed(locked, at, Options{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	holds(t, right, secret, 0x99)

	// The pages really are encrypted: the label is readable and the data is not.
	image := locked.Durable()
	if !bytes.Contains(image, Magic[:]) {
		t.Error("the label is not readable without the key")
	}
	if bytes.Contains(image[PageBytes:], bytes.Repeat([]byte{0x99}, 64)) {
		t.Error("a page of an encrypted led store is on the disk in the clear")
	}
}

// TestALedStoreGoesBackWhereItWasTold: abandoning is asking the same authority
// the same question, not working out an answer of its own.
func TestALedStoreGoesBackWhereItWasTold(t *testing.T) {
	pages, err := CreateLed(vfs.NewSim(8, vfs.Faults{}), Options{})
	if err != nil {
		t.Fatal(err)
	}

	kept := filled(t, pages, 0x33)
	at, err := pages.Flush(kept)
	if err != nil {
		t.Fatal(err)
	}

	// A transaction in progress: a page allocated, and the page the last
	// transaction used handed to the free list.
	loose := filled(t, pages, 0x44)
	pages.Free(kept)
	if !pages.Pending() {
		t.Fatal("a transaction with a page taken and a page freed reports nothing pending")
	}

	if err := pages.Abandon(at); err != nil {
		t.Fatal(err)
	}
	if pages.Pending() {
		t.Error("abandoning left a transaction in progress")
	}
	if pages.At() != at {
		t.Errorf("abandoned to %+v, want %+v", pages.At(), at)
	}

	// The page the abandoned transaction took is free again, and the page it
	// wanted to free is not — it is the one the live tree still points at.
	// Handing that one out is the corruption this rule exists to prevent.
	next, err := pages.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if next == kept {
		t.Fatal("a page the committed transaction still points at was handed out")
	}
	if next != loose {
		t.Errorf("the next page is %d, want the abandoned transaction's %d", next, loose)
	}
	holds(t, pages, kept, 0x33)
}

// TestTheLabelIsNotADataPage: everything that decides a page may be written
// over asks Dirty, so the label being dirty is the label being writable. It is
// written once, at create, and never again.
func TestTheLabelIsNotADataPage(t *testing.T) {
	pages, err := CreateLed(vfs.NewSim(9, vfs.Faults{}), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pages.Dirty(0) {
		t.Error("the label counts as a page this transaction allocated")
	}
	if err := pages.Write(pages.NewPage(0, KindLeaf)); !errors.Is(err, ErrReadOnlyPage) {
		t.Errorf("the label was written by hand: %v", err)
	}

	// And the same for the two meta pages of a leader.
	leader, err := Create(vfs.NewSim(10, vfs.Faults{}), 0)
	if err != nil {
		t.Fatal(err)
	}
	if leader.Dirty(0) || leader.Dirty(1) {
		t.Error("a meta page counts as a page this transaction allocated")
	}
}

// TestAbandoningALedStoreAsksTheFileAgain is the corruption this rule exists
// to prevent, and it is the one that has already happened here once.
//
// The pages an abandoned transaction marked as rubbish are the pages the live
// tree points at. Taking the leader's word for where the free list starts is
// not enough — the list has to be read again from there, or those pages are
// handed out to be written over, with nothing complaining until somebody
// follows a pointer that still leads to one.
func TestAbandoningALedStoreAsksTheFileAgain(t *testing.T) {
	pages, err := CreateLed(vfs.NewSim(11, vfs.Faults{}), Options{})
	if err != nil {
		t.Fatal(err)
	}

	root := filled(t, pages, 0x01)
	spare := filled(t, pages, 0x02)
	if _, err := pages.Flush(root); err != nil {
		t.Fatal(err)
	}

	// A transaction that gives a page back, committed: now the free list the
	// leader records is not empty, which is what makes the difference visible.
	pages.Free(spare)
	at, err := pages.Flush(root)
	if err != nil {
		t.Fatal(err)
	}
	if at.Freelist == 0 {
		t.Fatal("freeing a page left no free list for the leader to record")
	}

	// A transaction that takes it, and is abandoned.
	taken, err := pages.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if taken != spare {
		t.Fatalf("the free page was %d and the next allocation was %d", spare, taken)
	}
	if err := pages.Abandon(at); err != nil {
		t.Fatal(err)
	}

	// It must be free again. A store that came back believing it had nothing
	// free would grow the file instead — which is only wasteful — but the same
	// missing read is what loses the pages the live tree still points at.
	again, err := pages.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if again != spare {
		t.Errorf("after abandoning, the next page is %d; the free one is %d", again, spare)
	}
}

// TestTheFreeListPagesAreThemselvesRecycled: the pages holding last
// transaction's free list are this transaction's garbage. A store that does
// not say so leaks two pages per flush, which on a database being written to
// is a file that grows while nothing is being stored.
func TestTheFreeListPagesAreThemselvesRecycled(t *testing.T) {
	pages, err := CreateLed(vfs.NewSim(12, vfs.Faults{}), Options{})
	if err != nil {
		t.Fatal(err)
	}

	root := filled(t, pages, 0x01)
	churn := filled(t, pages, 0x02)
	if _, err := pages.Flush(root); err != nil {
		t.Fatal(err)
	}

	// The same page given back and taken again, thirty times over. Nothing is
	// being stored, so nothing should be being added to the file.
	for i := 0; i < 30; i++ {
		pages.Free(churn)
		if _, err := pages.Flush(root); err != nil {
			t.Fatalf("flush %d: %v", i, err)
		}
		taken, err := pages.Allocate()
		if err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
		churn = taken
		if _, err := pages.Flush(root); err != nil {
			t.Fatalf("flush %d: %v", i, err)
		}
	}

	// A label, a root, the page being churned, and the free list — which is
	// itself recycled. Anything much past that is a leak per transaction.
	if grown := pages.At().PageCount; grown > 8 {
		t.Errorf("thirty transactions storing nothing grew the file to %d pages", grown)
	}
}

// TestOpeningALedStoreReadsTheFreeListItWasToldAbout: the leader records where
// the list starts, and that is a page id, not the list. A store that opened
// without following it would believe it had nothing free and grow the file
// past pages that are sitting there unused — for the life of the database,
// because the list it never read is the list it never writes back either.
func TestOpeningALedStoreReadsTheFreeListItWasToldAbout(t *testing.T) {
	disk := vfs.NewSim(13, vfs.Faults{})
	pages, err := CreateLed(disk, Options{})
	if err != nil {
		t.Fatal(err)
	}

	root := filled(t, pages, 0x01)
	spare := filled(t, pages, 0x02)
	if _, err := pages.Flush(root); err != nil {
		t.Fatal(err)
	}
	pages.Free(spare)
	at, err := pages.Flush(root)
	if err != nil {
		t.Fatal(err)
	}

	restart := vfs.NewSim(13, vfs.Faults{})
	restart.Restore(disk.Durable())
	back, err := OpenLed(restart, at, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if back.FreePages() == 0 {
		t.Fatal("the store opened believing it had nothing free")
	}

	again, err := back.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if again != spare {
		t.Errorf("the next page is %d; the free one was %d", again, spare)
	}
}
