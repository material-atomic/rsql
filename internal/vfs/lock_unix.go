//go:build unix

package vfs

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrLocked is a database another process already has open for writing.
var ErrLocked = errors.New("rsql/vfs: another process has this database open")

// lock takes an exclusive lock on the whole file, and does not wait for it.
//
// The engine is built for one writer. Nothing in the file format defends
// against two, and two would not produce an error — they would produce a file
// where each process's pages are correct and the two sets disagree, which
// neither of them can detect and no checksum can catch. So the second one is
// refused here, at the only place both of them pass through.
//
// Not waiting is deliberate. A command that blocks on a lock held by a running
// server looks like a command that is slow, and somebody will leave it running
// and wonder. Told no, they can stop the server or use a copy.
func lock(handle *os.File) error {
	if err := syscall.Flock(int(handle.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("%w: %s", ErrLocked, handle.Name())
		}
		return fmt.Errorf("rsql/vfs: locking %s: %w", handle.Name(), err)
	}
	return nil
}
