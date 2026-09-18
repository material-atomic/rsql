package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/sapedb/sapedb/internal/btree"
	"github.com/sapedb/sapedb/internal/pager"
	"github.com/sapedb/sapedb/internal/vfs"
)

// One database, several files, one commit.
//
// A partition earns its keep by being droppable: unlinking a file returns a
// month of data to the operating system, where deleting the same documents
// through a copy-on-write tree would rewrite most of it and leave a free list
// that only grows. That is the whole reason this exists, and everything here
// is in service of it staying true.
//
// Where each partition is — its tree root, its free list, its length — is kept
// in the leader's own tree, under a prefix of its own. Nothing about the file
// format changes, because the leader's tree root already is the commit point:
// recording a partition's position is an ordinary write, and it becomes true
// at exactly the moment every other write of that transaction does.
//
// The order is what makes it safe, and it is the only order that works:
//
//  1. Each partition writes its pages and syncs. Nothing about the database
//     has changed yet — those pages are not reachable from anything.
//  2. The leader records where each partition now is, in its tree.
//  3. The leader commits: data, sync, meta, sync.
//
// A crash anywhere before the end of 3 leaves partitions holding pages nobody
// points at, and a leader that never heard of them. That is the same state a
// single file has had all along mid-transaction, and it has the same answer:
// the next transaction allocates over them.

// spaceParts is where the leader keeps the position of each partition.
const spaceParts byte = 0x07 // 0x07 | name -> root, freelist, page count

// partRecord is the fixed size of what is kept: three page numbers.
const partRecord = 24

var (
	// ErrNoFiles is a database asked for a partition when it was opened
	// without anywhere to put one.
	ErrNoFiles = errors.New("sapedb/store: this database has nowhere to keep partitions")
	// ErrPartName is a partition name that cannot be part of a file name.
	ErrPartName = errors.New("sapedb/store: a partition name may hold letters, digits, dot, dash and underscore")
	// ErrStray is a file in the directory that no partition claims.
	ErrStray = errors.New("sapedb/store: a file is here that this database does not know about")
)

// Files is where the other files of one database come from.
//
// An interface because a database is a directory in production, a map in a
// test, and neither should have to know about the other. Open makes the file
// if it is not there, which is what a partition being created means.
type Files interface {
	Open(name string) (vfs.File, error)
	Remove(name string) error
	Names() ([]string, error)
}

// part is one partition: its own pages, its own tree, and no say in when
// either becomes real.
type part struct {
	name  string
	pages *pager.Pager
	tree  *btree.Tree
}

// Keep sets where this database's partitions live, and the key they are made
// under. Without it, a database is one file and asking for a partition is an
// error rather than a surprise.
//
// The key is passed rather than taken from the leader because the leader does
// not hand it back — it has a cipher, not a key. Whoever opened the database
// has it, and a partition made without it would be a file of plaintext beside
// a database somebody encrypted.
func (s *Store) Keep(files Files, key []byte) {
	s.files = files
	s.key = key
}

// Part is the tree of a partition, making it if this is the first time.
//
// The file appears now and the partition becomes part of the database at the
// next commit. A crash in between leaves a file nothing claims, which Stray
// reports and nothing deletes on its own: a file removed by a program that was
// recovering is a file nobody chose to lose.
func (s *Store) Part(name string) (*btree.Tree, error) {
	if made, open := s.parts[name]; open {
		return made.tree, nil
	}
	if s.files == nil {
		return nil, fmt.Errorf("%w: cannot make %q", ErrNoFiles, name)
	}
	if !usablePartName(name) {
		return nil, fmt.Errorf("%w: %q", ErrPartName, name)
	}

	at, recorded, err := s.readPart(name)
	if err != nil {
		return nil, err
	}

	file, err := s.files.Open(partFile(name))
	if err != nil {
		return nil, err
	}

	options := pager.Options{Key: s.key}
	var pages *pager.Pager
	if recorded {
		pages, err = pager.OpenLed(file, at, options)
	} else {
		pages, err = pager.CreateLed(file, options)
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}

	made := &part{name: name, pages: pages, tree: btree.At(pages, at.Root)}
	s.parts[name] = made
	return made.tree, nil
}

// Parts is every partition this database has, in order, as the transaction in
// progress sees it.
//
// Which means the ones committed plus the ones made since, minus the ones
// dropped since. Reading only the committed rows would make a partition
// invisible to the transaction that just created it — and the first thing that
// went wrong when this was written was that expiry could not see the partition
// whose arrival was supposed to trigger it.
func (s *Store) Parts() []string {
	prefix := []byte{spaceParts}
	seen := map[string]bool{}
	names := []string{}

	_ = s.tree.Ascend(prefix, func(key, _ []byte) bool {
		if len(key) <= 1 || key[0] != spaceParts {
			return false
		}
		name := string(key[1:])
		seen[name] = true
		names = append(names, name)
		return true
	})

	for name := range s.parts {
		if !seen[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// DropPart removes a partition: one unlink, whatever it held.
//
// The row goes first and the file second, because the other order can lose
// data. A crash between them leaves a file nothing claims — which Stray finds
// and somebody decides about — where removing the file first would leave the
// database pointing at something that is not there.
func (s *Store) DropPart(name string) error {
	if s.files == nil {
		return fmt.Errorf("%w: cannot drop %q", ErrNoFiles, name)
	}
	_, recorded, err := s.readPart(name)
	if err != nil {
		return err
	}

	open, live := s.parts[name]
	if !recorded && !live {
		return nil
	}

	// Set aside rather than closed. A drop that is rolled back has to leave a
	// partition somebody can still read, and a handle closed halfway through a
	// transaction is one that cannot be given back — the first test written
	// for this fell straight into it.
	if live {
		delete(s.parts, name)
		s.dropping[name] = open
	}

	// A partition made by this same transaction has no row to delete — it was
	// never committed. It still has a file, and the file still goes, because
	// everything in it was written by the transaction that is dropping it.
	if recorded {
		if _, err := s.tree.Delete(append([]byte{spaceParts}, name...)); err != nil {
			return err
		}
	}
	s.dropped = append(s.dropped, name)
	return nil
}

// Stray is the files in this directory that no partition claims.
//
// A crash between a file being made and the transaction that would have
// claimed it leaves one. So does a crash between a partition's row being
// deleted and its file being removed. Neither is dangerous and neither is
// tidied automatically: what to do with a file full of data nobody is pointing
// at is a decision, and the answer from a program in a hurry is the wrong one.
func (s *Store) Stray() ([]string, error) {
	if s.files == nil {
		return nil, nil
	}

	claimed := map[string]bool{}
	for _, name := range s.Parts() {
		claimed[partFile(name)] = true
	}

	here, err := s.files.Names()
	if err != nil {
		return nil, err
	}

	stray := []string{}
	for _, name := range here {
		if !claimed[name] {
			stray = append(stray, name)
		}
	}
	sort.Strings(stray)
	return stray, nil
}

// commitParts makes every partition's pages durable and records where they
// are, before the leader commits.
func (s *Store) commitParts() error {
	// In name order, so that a database being written to produces the same
	// sequence of syncs every time and a test can say what happened.
	for _, name := range sortedNames(s.parts) {
		open := s.parts[name]
		if !open.pages.Pending() && open.pages.At().Root == open.tree.Root() {
			continue
		}

		at, err := open.pages.Flush(open.tree.Root())
		if err != nil {
			return err
		}
		if err := s.writePart(name, at); err != nil {
			return err
		}
	}

	// Files are unlinked only once the transaction that dropped them has
	// landed, which is the caller's job — this only remembers which.
	return nil
}

// dropFiles unlinks what a committed transaction dropped.
func (s *Store) dropFiles() error {
	for _, name := range s.dropped {
		if open, held := s.dropping[name]; held {
			if err := open.pages.Close(); err != nil {
				return err
			}
			delete(s.dropping, name)
		}
		if err := s.files.Remove(partFile(name)); err != nil {
			return err
		}
	}
	s.dropped = nil
	return nil
}

// abandonParts puts every open partition back where the committed leader says
// it is.
func (s *Store) abandonParts() error {
	for _, name := range sortedNames(s.parts) {
		open := s.parts[name]

		at, recorded, err := s.readPart(name)
		if err != nil {
			return err
		}
		if !recorded {
			// Made by the transaction being abandoned. The file stays, because
			// deleting one while undoing is how an undo loses data; Stray will
			// report it.
			if err := open.pages.Close(); err != nil {
				return err
			}
			delete(s.parts, name)
			continue
		}

		if err := open.pages.Abandon(at); err != nil {
			return err
		}
		open.tree = btree.At(open.pages, at.Root)
	}

	// A drop that was rolled back gives its partition back, open and where the
	// committed leader says it is.
	for name, held := range s.dropping {
		at, recorded, err := s.readPart(name)
		if err != nil {
			return err
		}
		if !recorded {
			continue
		}
		if err := held.pages.Abandon(at); err != nil {
			return err
		}
		held.tree = btree.At(held.pages, at.Root)
		s.parts[name] = held
		delete(s.dropping, name)
	}

	s.dropped = nil
	return nil
}

// closeParts lets go of the files.
func (s *Store) closeParts() error {
	var first error
	for _, open := range s.parts {
		if err := open.pages.Close(); err != nil && first == nil {
			first = err
		}
	}
	for _, held := range s.dropping {
		if err := held.pages.Close(); err != nil && first == nil {
			first = err
		}
	}
	s.parts = map[string]*part{}
	s.dropping = map[string]*part{}
	return first
}

func (s *Store) readPart(name string) (pager.Led, bool, error) {
	value, found, err := s.tree.Get(append([]byte{spaceParts}, name...))
	if err != nil || !found {
		return pager.Led{}, false, err
	}
	if len(value) != partRecord {
		return pager.Led{}, false, fmt.Errorf("%w: the record for partition %q is %d bytes", ErrDamaged, name, len(value))
	}
	return pager.Led{
		Root:      binary.BigEndian.Uint64(value[0:]),
		Freelist:  binary.BigEndian.Uint64(value[8:]),
		PageCount: binary.BigEndian.Uint64(value[16:]),
	}, true, nil
}

func (s *Store) writePart(name string, at pager.Led) error {
	value := make([]byte, partRecord)
	binary.BigEndian.PutUint64(value[0:], at.Root)
	binary.BigEndian.PutUint64(value[8:], at.Freelist)
	binary.BigEndian.PutUint64(value[16:], at.PageCount)
	return s.tree.Put(append([]byte{spaceParts}, name...), value)
}

func partFile(name string) string { return name + ".part" }

// usableName keeps a partition name to what can be a file name anywhere, and
// what cannot walk out of the directory.
func usablePartName(name string) bool {
	if name == "" || len(name) > 128 || name == "." || name == ".." {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

func sortedNames(parts map[string]*part) []string {
	names := make([]string, 0, len(parts))
	for name := range parts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
