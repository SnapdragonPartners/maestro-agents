//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package filestore

import (
	"errors"
	"os"
)

// errUnsupported refuses to open a file store where this package cannot
// take an exclusive lock, rather than open one unprotected.
var errUnsupported = errors.New("the file store's directory lock is not supported on this platform")

func lockFile(*os.File) error { return errUnsupported }

func unlockFile(*os.File) error { return nil }
