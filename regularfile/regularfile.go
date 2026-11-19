// Package regularfile opens an operator-named file only if it is a
// regular file, without hanging on a FIFO or device (ADR 0017, item 11):
// the standalone host's configuration, secrets and inputs, and an agent's
// own commands' files. It is public so that agents built in their own
// modules open operators' files the same way (ADR 0023).
package regularfile

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// ErrTooLarge is returned by ReadCapped for a file over its limit.
var ErrTooLarge = errors.New("file too large")

// Check returns f if it is a regular file, checked on the open descriptor;
// otherwise it closes f and returns an error.
func Check(f *os.File, path string) (*os.File, error) {
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

// PlusOne is n+1, saturating: the one byte past a limit that shows a
// file exceeds it. At math.MaxInt64 no file can exceed the limit, and n+1
// would wrap to a negative limit that reads nothing (ADR 0017, item 11).
func PlusOne(n int64) int64 {
	if n == math.MaxInt64 {
		return n
	}
	return n + 1
}

// ReadCapped reads a regular file of at most limit bytes.
func ReadCapped(path string, limit int64) ([]byte, error) {
	f, err := Open(path, false)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	b, err := io.ReadAll(io.LimitReader(f, PlusOne(limit)))
	if err != nil {
		return nil, err //nolint:wrapcheck // the PathError names the file
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w: %s is over %d bytes", ErrTooLarge, path, limit)
	}
	return b, nil
}
