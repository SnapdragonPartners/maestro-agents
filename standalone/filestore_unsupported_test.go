//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package standalone

// fileStoreSupported: store/filestore cannot lock on this platform.
const fileStoreSupported = false
