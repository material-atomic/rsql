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
	"io"
	"os"
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

// ErrClosed is returned by every method of a file that has been closed.
var ErrClosed = errors.New("rsql/vfs: file is closed")

// OpenFile opens a real file for reading and writing, creating it if needed.
func OpenFile(path string, perm os.FileMode) (File, error) {
	handle, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, perm)
	if err != nil {
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
