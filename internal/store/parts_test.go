package store

import (
	"bytes"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/material-atomic/rsql/internal/pager"
	"github.com/material-atomic/rsql/internal/vfs"
)

// shelfOfFiles is a directory that lives in memory, so a test can look at what
// actually reached each disk.
type shelfOfFiles struct {
	disks map[string]*vfs.SimDisk
	// durable is what a restart would find, kept separately so that a test can
	// restore the whole directory at the moment of a crash.
	seed int64
}

func newShelf(seed int64) *shelfOfFiles {
	return &shelfOfFiles{disks: map[string]*vfs.SimDisk{}, seed: seed}
}

func (s *shelfOfFiles) Open(name string) (File, error) {
	if disk, made := s.disks[name]; made {
		return kept{disk}, nil
	}
	disk := vfs.NewSim(s.seed, vfs.Faults{})
	s.disks[name] = disk
	return kept{disk}, nil
}

// kept is a file whose Close lets go of the handle and not of the file, which
// is what closing a file does. A double that destroyed the bytes would fail
// things that work.
type kept struct{ *vfs.SimDisk }

func (kept) Close() error { return nil }

func (s *shelfOfFiles) Remove(name string) error {
	delete(s.disks, name)
	return nil
}

func (s *shelfOfFiles) Names() ([]string, error) {
	names := make([]string, 0, len(s.disks))
	for name := range s.disks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// restart is the directory as a restart would find it: only what was synced.
func (s *shelfOfFiles) restart() *shelfOfFiles {
	back := newShelf(s.seed)
	for name, disk := range s.disks {
		fresh := vfs.NewSim(s.seed, vfs.Faults{})
		fresh.Restore(disk.Durable())
		back.disks[name] = fresh
	}
	return back
}

// partitioned is a database with somewhere to keep partitions.
func partitioned(t *testing.T, seed int64) (*vfs.SimDisk, *shelfOfFiles, *Store) {
	t.Helper()

	disk, store := fresh(t, seed)
	shelf := newShelf(seed)
	store.Keep(shelf, nil)
	return disk, shelf, store
}

// reopen opens the leader and its directory as a restart would find them.
func reopen(t *testing.T, disk *vfs.SimDisk, shelf *shelfOfFiles, seed int64) (*Store, *shelfOfFiles) {
	t.Helper()

	restart := vfs.NewSim(seed, vfs.Faults{})
	restart.Restore(disk.Durable())
	pages, err := pager.Open(restart, 0)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(pages)
	if err != nil {
		t.Fatal(err)
	}
	back := shelf.restart()
	store.Keep(back, nil)
	return store, back
}

func putPart(t *testing.T, store *Store, name, key, value string) {
	t.Helper()

	tree, err := store.Part(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Put([]byte(key), []byte(value)); err != nil {
		t.Fatal(err)
	}
}

func inPart(t *testing.T, store *Store, name, key string) (string, bool) {
	t.Helper()

	tree, err := store.Part(name)
	if err != nil {
		t.Fatal(err)
	}
	value, found, err := tree.Get([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	return string(value), found
}

// TestAPartitionIsPartOfTheDatabaseWhenTheLeaderSaysSo: the partition's pages
// are on the disk before the leader commits, and mean nothing until it does.
func TestAPartitionIsPartOfTheDatabaseWhenTheLeaderSaysSo(t *testing.T) {
	disk, shelf, store := partitioned(t, 200)

	putPart(t, store, "2026-01", "a", "january")
	putPart(t, store, "2026-02", "b", "february")
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	back, _ := reopen(t, disk, shelf, 200)
	if names := back.Parts(); len(names) != 2 || names[0] != "2026-01" || names[1] != "2026-02" {
		t.Fatalf("the database holds partitions %v", names)
	}
	if value, found := inPart(t, back, "2026-01", "a"); !found || value != "january" {
		t.Errorf("january came back as %q, %v", value, found)
	}
	if value, found := inPart(t, back, "2026-02", "b"); !found || value != "february" {
		t.Errorf("february came back as %q, %v", value, found)
	}
}

// TestAPartitionThatNoCommitClaimedIsNotThere is the crash this design exists
// to answer: everything written, everything synced, and the leader never got
// to say so.
func TestAPartitionThatNoCommitClaimedIsNotThere(t *testing.T) {
	disk, shelf, store := partitioned(t, 201)

	putPart(t, store, "kept", "a", "one")
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	// A second transaction, into an existing partition and a new one, that
	// gets all the way to the disk. The leader is never told.
	putPart(t, store, "kept", "b", "two")
	putPart(t, store, "lost", "c", "three")
	if err := store.commitParts(); err != nil {
		t.Fatal(err)
	}

	back, files := reopen(t, disk, shelf, 201)

	if names := back.Parts(); len(names) != 1 || names[0] != "kept" {
		t.Fatalf("the database holds partitions %v, want just the one the leader committed", names)
	}
	if value, found := inPart(t, back, "kept", "a"); !found || value != "one" {
		t.Errorf("the committed write came back as %q, %v", value, found)
	}
	if _, found := inPart(t, back, "kept", "b"); found {
		t.Error("a write the leader never committed is in the database")
	}

	// The file the abandoned transaction made is still on the disk, claimed by
	// nothing. Nothing deletes it: what to do with a file of data nobody
	// points at is a decision, and a program in a hurry gives the wrong answer.
	stray, err := back.Stray()
	if err != nil {
		t.Fatal(err)
	}
	if len(stray) != 1 || stray[0] != "lost.part" {
		t.Errorf("the strays are %v, want just lost.part", stray)
	}
	if _, here := files.disks["lost.part"]; !here {
		t.Error("the file was removed by something that was only supposed to notice it")
	}
}

// TestAbandoningPutsEveryPartitionBack: a rollback asks the committed leader
// where each partition belongs. Asking the transaction being undone would be
// asking the wrong one.
func TestAbandoningPutsEveryPartitionBack(t *testing.T) {
	_, _, store := partitioned(t, 202)

	putPart(t, store, "old", "a", "one")
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	putPart(t, store, "old", "b", "two")
	putPart(t, store, "new", "c", "three")
	if err := store.Rollback(); err != nil {
		t.Fatal(err)
	}

	if value, found := inPart(t, store, "old", "a"); !found || value != "one" {
		t.Errorf("what was committed came back as %q, %v", value, found)
	}
	if _, found := inPart(t, store, "old", "b"); found {
		t.Error("a write that was rolled back is still in the partition")
	}
	if names := store.Parts(); len(names) != 1 || names[0] != "old" {
		t.Errorf("after the rollback the database holds %v", names)
	}

	// And the partition the abandoned transaction made is a stray file, not a
	// deleted one.
	stray, err := store.Stray()
	if err != nil {
		t.Fatal(err)
	}
	if len(stray) != 1 || stray[0] != "new.part" {
		t.Errorf("the strays are %v, want just new.part", stray)
	}
}

// TestDroppingAPartitionIsOneUnlink is what partitions are for.
func TestDroppingAPartitionIsOneUnlink(t *testing.T) {
	disk, shelf, store := partitioned(t, 203)

	putPart(t, store, "2025-12", "a", "december")
	putPart(t, store, "2026-01", "b", "january")
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := store.DropPart("2025-12"); err != nil {
		t.Fatal(err)
	}

	// Not yet: a file unlinked before the transaction that dropped it landed is
	// a file the database would still point at after a crash.
	if _, here := shelf.disks["2025-12.part"]; !here {
		t.Fatal("the file went before the transaction that dropped it")
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, here := shelf.disks["2025-12.part"]; here {
		t.Error("the file is still here after the drop was committed")
	}

	back, _ := reopen(t, disk, shelf, 203)
	if names := back.Parts(); len(names) != 1 || names[0] != "2026-01" {
		t.Errorf("after the drop the database holds %v", names)
	}
	if value, found := inPart(t, back, "2026-01", "b"); !found || value != "january" {
		t.Errorf("the partition that stayed came back as %q, %v", value, found)
	}
}

// TestADropThatWasRolledBackKeepsItsFile: the unlink happens after the commit
// precisely so that a transaction which never commits takes nothing with it.
func TestADropThatWasRolledBackKeepsItsFile(t *testing.T) {
	_, shelf, store := partitioned(t, 204)

	putPart(t, store, "2025-12", "a", "december")
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := store.DropPart("2025-12"); err != nil {
		t.Fatal(err)
	}
	if err := store.Rollback(); err != nil {
		t.Fatal(err)
	}

	if _, here := shelf.disks["2025-12.part"]; !here {
		t.Fatal("a drop that was rolled back took the file with it")
	}
	if names := store.Parts(); len(names) != 1 || names[0] != "2025-12" {
		t.Fatalf("after the rollback the database holds %v", names)
	}
	if value, found := inPart(t, store, "2025-12", "a"); !found || value != "december" {
		t.Errorf("the partition came back as %q, %v", value, found)
	}
}

// TestAPartitionIsEncryptedIfTheDatabaseIs: a partition made without the key
// would be a file of plaintext beside a database somebody encrypted, which is
// the sort of thing nobody notices until it matters.
func TestAPartitionIsEncryptedIfTheDatabaseIs(t *testing.T) {
	key := bytes.Repeat([]byte{0x2C}, 32)

	disk := vfs.NewSim(205, vfs.Faults{})
	pages, err := pager.CreateWith(disk, pager.Options{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(pages)
	if err != nil {
		t.Fatal(err)
	}
	shelf := newShelf(205)
	store.Keep(shelf, key)

	putPart(t, store, "secret", "a", "the-quick-brown-fox-jumped")
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	image := shelf.disks["secret.part"].Durable()
	if bytes.Contains(image, []byte("the-quick-brown-fox-jumped")) {
		t.Error("a partition of an encrypted database is on the disk in the clear")
	}

	// And it opens again under the same key, and not without it.
	store.Keep(shelf, nil)
	delete(store.parts, "secret")
	if _, err := store.Part("secret"); !errors.Is(err, pager.ErrKey) {
		t.Errorf("an encrypted partition opened with no key: %v", err)
	}
}

// TestWhatCanBeCalledAPartition: the name becomes a file name, so it must not
// be able to walk out of the directory or arrive somewhere that cannot spell
// it.
func TestWhatCanBeCalledAPartition(t *testing.T) {
	_, _, store := partitioned(t, 206)

	for _, name := range []string{"", ".", "..", "../escape", "a/b", "a\x00b", "with space", "café"} {
		if _, err := store.Part(name); !errors.Is(err, ErrPartName) {
			t.Errorf("%q was accepted as a partition name: %v", name, err)
		}
	}
	for _, name := range []string{"2026-01", "a.b", "a_b", "A1"} {
		if _, err := store.Part(name); err != nil {
			t.Errorf("%q was refused as a partition name: %v", name, err)
		}
	}
}

// TestADatabaseWithNowhereToPutThemSaysSo: one file is a perfectly good
// database, and asking it for a partition should be an error rather than a
// file appearing somewhere nobody chose.
func TestADatabaseWithNowhereToPutThemSaysSo(t *testing.T) {
	_, store := fresh(t, 207)

	if _, err := store.Part("2026-01"); !errors.Is(err, ErrNoFiles) {
		t.Errorf("a database with nowhere to keep partitions made one: %v", err)
	}
	if err := store.DropPart("2026-01"); !errors.Is(err, ErrNoFiles) {
		t.Errorf("a database with nowhere to keep partitions dropped one: %v", err)
	}
	if stray, err := store.Stray(); err != nil || len(stray) != 0 {
		t.Errorf("a database with no directory reported strays %v, %v", stray, err)
	}
}

// TestADropWhoseCommitFailedKeepsItsFile: the unlink comes after the commit,
// and this is the difference that makes. A commit that does not land must take
// nothing with it — otherwise the database comes back pointing at a partition
// whose file was already removed, which is the one failure a design with no
// recovery path cannot survive.
func TestADropWhoseCommitFailedKeepsItsFile(t *testing.T) {
	disk, shelf, store := partitioned(t, 208)

	putPart(t, store, "2025-12", "a", "december")
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := store.DropPart("2025-12"); err != nil {
		t.Fatal(err)
	}

	// The leader's disk goes away, so its commit cannot land.
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err == nil {
		t.Fatal("the commit succeeded on a disk that is not there")
	}

	if _, here := shelf.disks["2025-12.part"]; !here {
		t.Error("a commit that failed took the partition's file with it")
	}
}

// TestRollingBackADropGivesThePartitionBack, in full: the row, the open file,
// and the fact that the next commit does not go looking for something to
// unlink.
func TestRollingBackADropGivesThePartitionBack(t *testing.T) {
	_, shelf, store := partitioned(t, 209)

	putPart(t, store, "2025-12", "a", "december")
	putPart(t, store, "2026-01", "b", "january")
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := store.DropPart("2025-12"); err != nil {
		t.Fatal(err)
	}
	if err := store.Rollback(); err != nil {
		t.Fatal(err)
	}

	// Nothing is being held back any more: a partition still set aside is one
	// that a second drop, or a close, would have to know about.
	if len(store.dropping) != 0 {
		t.Errorf("after the rollback, %d partitions are still set aside", len(store.dropping))
	}
	if _, open := store.parts["2025-12"]; !open {
		t.Error("the partition was not given back")
	}

	// And the next commit does not unlink what an abandoned transaction wanted
	// gone. This is a different mistake from the one above: there the commit
	// failed, here it succeeds and must still leave the file alone.
	putPart(t, store, "2026-01", "c", "more")
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, here := shelf.disks["2025-12.part"]; !here {
		t.Error("a later commit unlinked the file a rolled-back drop had wanted gone")
	}
	if value, found := inPart(t, store, "2025-12", "a"); !found || value != "december" {
		t.Errorf("the partition reads back as %q, %v", value, found)
	}
}

// TestAPartitionAnAbandonedTransactionMadeIsEmptyAgain: a rollback has to
// forget the file as well as the row. A partition left open at a transaction
// that never happened would serve its writes to whoever asked next.
func TestAPartitionAnAbandonedTransactionMadeIsEmptyAgain(t *testing.T) {
	_, _, store := partitioned(t, 210)

	putPart(t, store, "settled", "a", "one")
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	putPart(t, store, "never", "b", "two")
	if err := store.Rollback(); err != nil {
		t.Fatal(err)
	}

	if _, open := store.parts["never"]; open {
		t.Error("a partition the rollback undid is still open")
	}
	if _, found := inPart(t, store, "never", "b"); found {
		t.Error("a write from an abandoned transaction came back out of the partition it made")
	}
}

// TestAPartitionNobodyTouchedIsNotSynced: a commit costs one sync per
// partition it writes to, and a database with a year of monthly partitions
// writes to one of them. Syncing the other eleven would make the cost of a
// write grow with how much history is being kept.
func TestAPartitionNobodyTouchedIsNotSynced(t *testing.T) {
	_, shelf, store := partitioned(t, 211)

	putPart(t, store, "2026-01", "a", "one")
	putPart(t, store, "2026-02", "b", "two")
	putPart(t, store, "2026-03", "c", "three")
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	before := map[string]int{}
	for name, disk := range shelf.disks {
		before[name] = syncs(disk)
	}

	// One partition written to, and a commit.
	putPart(t, store, "2026-02", "d", "four")
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	for name, disk := range shelf.disks {
		grew := syncs(disk) - before[name]
		if name == "2026-02.part" {
			if grew == 0 {
				t.Errorf("the partition that was written to was not synced")
			}
			continue
		}
		if grew != 0 {
			t.Errorf("%s was synced %d times for a write that did not touch it", name, grew)
		}
	}
}

func syncs(disk *vfs.SimDisk) int {
	count := 0
	for _, line := range disk.Trace {
		if strings.Contains(line, "sync") {
			count++
		}
	}
	return count
}
