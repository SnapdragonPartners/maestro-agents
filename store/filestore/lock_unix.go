//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package filestore

import (
	"fmt"
	"os"
	"syscall"
)

// lockFile takes an exclusive, non-blocking flock. The lock belongs to the
// open file description, so it is released when the file is closed or the
// process exits, however it exits.
func lockFile(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // G115: a file descriptor fits in an int
		return fmt.Errorf("flock: %w", err)
	}
	return nil
}

func unlockFile(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil { //nolint:gosec // G115: a file descriptor fits in an int
		return fmt.Errorf("flock: %w", err)
	}
	return nil
}
