// Package vfs is the only way the engine touches a disk.
//
// One interface, two implementations: a real file, and a simulated one that
// can be told to lie. The simulated disk is what makes durability testable —
// power can be cut between any two writes, an fsync can return success without
// having written anything, and a write can land half-done. All of it
// deterministic from a seed, so a failure can be replayed exactly.
//
// This exists before the engine on purpose. A storage engine is not hard to
// write; knowing it is right is, and the knowing is what took the engines we
// learned from twenty years to accumulate. They accumulated it from the field,
// one corrupted database at a time. The simulator is how that experience is
// bought up front instead.
package vfs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// File is what a storage engine needs from a file, and nothing more.
//
// Deliberately without Seek: every read and write names its offset, so nothing
// depends on a cursor that a concurrent reader could move.
type File interface {
	io.ReaderAt
	io.WriterAt

	// Sync makes every write so far durable. Until it returns, nothing written
	// is guaranteed to survive a power cut.
	Sync() error

	// Truncate changes the size of the file.
	Truncate(size int64) error

	// Size is the current length in bytes.
	Size() (int64, error)

	Close() error
}

// LockDir takes an exclusive lock on a whole directory of databases, and hands
// back the thing that releases it.
//
// Locking each database file is not enough on its own, because a server opens
// them only when somebody asks for one. Two servers started on one directory
// would both come up, both look healthy, and only collide later — at whichever
// request first touched the same database, with an error nobody expects at
// that moment. A lock taken when the process starts turns that into the thing
// it actually is: the second one does not start.
//
// It also makes the answer the tools give true. "The server is probably
// running" is a guess if the lock only covers files it happens to have opened,
// and a fact if it covers the directory.
func LockDir(dir string) (io.Closer, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	handle, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lock(handle); err != nil {
		handle.Close()
		if errors.Is(err, ErrLocked) {
			// Naming the directory rather than "this database": the caller is
			// about to show this to somebody, and pointing at a lock file while
			// saying "database" sends them looking for the wrong thing.
			return nil, fmt.Errorf("%w: the directory %s", ErrLocked, dir)
		}
		return nil, err
	}
	return handle, nil
}

// ErrClosed is returned by every method of a file that has been closed.
var ErrClosed = errors.New("rsql/vfs: file is closed")

// OpenFile opens a real file for reading and writing, creating it if needed,
// and takes an exclusive lock on it.
//
// The lock is not optional and is not a courtesy: see lock() for what two
// writers do to one of these files.
func OpenFile(path string, perm os.FileMode) (File, error) {
	handle, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, perm)
	if err != nil {
		return nil, err
	}
	if err := lock(handle); err != nil {
		handle.Close()
		return nil, err
	}
	return &osFile{handle: handle}, nil
}

type osFile struct {
	handle *os.File
}

func (f *osFile) ReadAt(p []byte, off int64) (int, error)  { return f.handle.ReadAt(p, off) }
func (f *osFile) WriteAt(p []byte, off int64) (int, error) { return f.handle.WriteAt(p, off) }
func (f *osFile) Sync() error                              { return f.handle.Sync() }
func (f *osFile) Truncate(size int64) error                { return f.handle.Truncate(size) }
func (f *osFile) Close() error                             { return f.handle.Close() }

func (f *osFile) Size() (int64, error) {
	info, err := f.handle.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// Folder is a directory of files, which is what a partitioned database is.
//
// One directory per database rather than one shared by all of them, so that
// "what files does this database have" is a question the directory answers by
// itself. A shared directory would make it a question about naming, and a
// wrong answer there deletes somebody else's data.
type Folder struct {
	path string
	perm os.FileMode
}

// At is a folder, made if it is not there yet.
func At(path string, perm os.FileMode) (*Folder, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, err
	}
	return &Folder{path: path, perm: perm}, nil
}

// Open is one file in the folder, made if it is not there.
func (f *Folder) Open(name string) (File, error) {
	if err := safeName(name); err != nil {
		return nil, err
	}
	return OpenFile(filepath.Join(f.path, name), f.perm)
}

// Remove unlinks one file.
func (f *Folder) Remove(name string) error {
	if err := safeName(name); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(f.path, name))
	if errors.Is(err, os.ErrNotExist) {
		// Already gone is the outcome that was wanted. A crash between a file
		// being unlinked and the database noticing must not turn into an error
		// every time afterwards.
		return nil
	}
	return err
}

// Names is every file in the folder.
func (f *Folder) Names() ([]string, error) {
	entries, err := os.ReadDir(f.path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// ErrName is a file name that is not one.
var ErrName = errors.New("rsql/vfs: a file name may not be a path")

// safeName refuses anything that could leave the folder. The names come from
// inside this program and are checked before they are used anywhere else, so
// this is the second lock on a door that is already locked — which is the
// right number of locks on the door that leads to os.Remove.
func safeName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("%w: %q", ErrName, name)
	}
	return nil
}
