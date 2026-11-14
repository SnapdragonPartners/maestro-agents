//go:build unix

package standalone

import (
	"fmt"
	"os"
	"syscall"
)

// openRegular opens a regular file for reading without blocking on a
// FIFO or device: O_NONBLOCK makes the open return at once, and the file's
// kind is checked on the open descriptor, so nothing can be swapped in
// between. With noFollow, a final symbolic link is refused (O_NOFOLLOW)
// rather than followed out of a watched folder (ADR 0017, item 16).
func openRegular(path string, noFollow bool) (*os.File, error) {
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if noFollow {
		flags |= syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(path, flags, 0) //nolint:gosec // G304: the operator's file
	if err != nil {
		return nil, err //nolint:wrapcheck // the PathError names the file
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err //nolint:wrapcheck // the PathError names the file
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return f, nil
}
