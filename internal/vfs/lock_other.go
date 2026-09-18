//go:build !unix

package vfs

import (
	"errors"
	"fmt"
	"os"
	"runtime"
)

// ErrLocked is a database another process already has open for writing.
var ErrLocked = errors.New("rsql/vfs: another process has this database open")

// lock refuses to open a database at all where it cannot lock one.
//
// Opening without a lock and hoping is the wrong direction to be incomplete
// in: two writers do not fail, they quietly produce a file where each one's
// pages are correct and the two disagree. Refusing is recoverable; that is
// not.
func lock(handle *os.File) error {
	return fmt.Errorf("rsql/vfs: no file locking on %s, so a database cannot be opened safely here", runtime.GOOS)
}

var _ = errors.Is
