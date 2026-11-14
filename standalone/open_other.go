//go:build !unix

package standalone

import (
	"fmt"
	"os"
)

// openRegular opens a regular file for reading. Without O_NOFOLLOW and
// O_NONBLOCK, it checks the path's kind before opening and the open
// file's after; the unix version closes the window between them.
func openRegular(path string, noFollow bool) (*os.File, error) {
	stat := os.Stat
	if noFollow {
		stat = os.Lstat
	}
	fi, err := stat(path)
	if err != nil {
		return nil, err //nolint:wrapcheck // the PathError names the file
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path) //nolint:gosec // G304: the operator's file
	if err != nil {
		return nil, err //nolint:wrapcheck // the PathError names the file
	}
	if fi, err = f.Stat(); err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return f, nil
}
