//go:build unix

package standalone

import (
	"os"
	"syscall"

	"github.com/SnapdragonPartners/maestro-agents/regularfile"
)

// openAppend opens a regular file for appending, creating it owner-only,
// without blocking on a FIFO (a write-only non-blocking open of one with
// no reader fails at once) and refusing any file that is not regular,
// checked on the open descriptor (ADR 0017, item 20).
func openAppend(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NONBLOCK, 0o600) //nolint:gosec // G304: the operator names the file
	if err != nil {
		return nil, err //nolint:wrapcheck // the PathError names the file
	}
	return regularfile.Check(f, path) //nolint:wrapcheck // names the file
}
