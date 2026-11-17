//go:build !unix

package standalone

import (
	"fmt"
	"os"
)

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
