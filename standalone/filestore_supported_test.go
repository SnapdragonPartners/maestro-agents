//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package standalone

// fileStoreSupported mirrors the platforms store/filestore can lock on
// (store/filestore/lock_*.go); elsewhere filestore.Open fails.
const fileStoreSupported = true
