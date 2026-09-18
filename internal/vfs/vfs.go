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
