package standalone

import (
	"os"

	"github.com/SnapdragonPartners/maestro-agents/internal/regularfile"
)

// openRegular opens an operator's regular file for reading without hanging
// on a FIFO or device; with noFollow, a final symbolic link is refused
// (ADR 0017, items 11 and 16).
func openRegular(path string, noFollow bool) (*os.File, error) {
	return regularfile.Open(path, noFollow) //nolint:wrapcheck // the error names the file
}
