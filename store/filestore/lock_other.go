//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package filestore

import "os"

func lockFile(*os.File) error { return errUnsupported }

func unlockFile(*os.File) error { return nil }
