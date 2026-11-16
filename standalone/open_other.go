//go:build !unix

package standalone

import (
	"fmt"
	"os"
)

// openRegular opens a regular file for reading. Without O_NOFOLLOW and
// O_NONBLOCK, it checks the path before opening and requires the open file
// to be the very file it checked (os.SameFile): a path swapped for a link
// or another file in between opens something else and is refused, so
// noFollow holds as on unix (ADR 0017, item 16).
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
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	// On Windows an Lstat result's file ID is read either at the Lstat or,
	// lazily, here, from the path with FILE_FLAG_OPEN_REPARSE_POINT: it is
	// the checked file's or a swapped-in link's, never the link's target's.
	if !os.SameFile(fi, opened) {
		_ = f.Close()
		return nil, fmt.Errorf("%s changed while being opened", path)
	}
	return f, nil
}

// openAppend opens a regular file for appending, creating it owner-only,
// refusing a path that exists and is not a regular file.
func openAppend(path string) (*os.File, error) {
	if fi, err := os.Stat(path); err == nil && !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) //nolint:gosec // G304: the operator names the file
	if err != nil {
		return nil, err //nolint:wrapcheck // the PathError names the file
	}
	return f, nil
}
