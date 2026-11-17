//go:build unix

package regularfile

import (
	"os"
	"syscall"
)

// Open opens a regular file for reading without blocking on a
// FIFO or device: O_NONBLOCK makes the open return at once, and the file's
// kind is checked on the open descriptor, so nothing can be swapped in
// between. With noFollow, a final symbolic link is refused (O_NOFOLLOW)
// rather than followed out of a watched folder (ADR 0017, items 11 and 16).
func Open(path string, noFollow bool) (*os.File, error) {
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if noFollow {
		flags |= syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(path, flags, 0) //nolint:gosec // G304: the operator's file
	if err != nil {
		return nil, err //nolint:wrapcheck // the PathError names the file
	}
	return Check(f, path)
}
