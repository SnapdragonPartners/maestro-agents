//go:build integration

package verify_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocalSamples runs the real sample statements, which are client data
// and never committed (the repository is public). They live in the
// gitignored .local/addepar at the repository root, or in the directory
// ADDEPAR_LOCAL_DIR names: statements/ holds the PDFs and cases/ their
// hand-written case files. The test skips when the directory is absent.
func TestLocalSamples(t *testing.T) {
	needPoppler(t)
	dir := os.Getenv("ADDEPAR_LOCAL_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", "..", "..", ".local", "addepar")
	}
	files := caseFiles(t, filepath.Join(dir, "cases"))
	if len(files) == 0 {
		t.Skipf("no local cases in %s", dir)
	}
	for _, f := range files {
		t.Run(strings.TrimSuffix(filepath.Base(f), ".json"), func(t *testing.T) {
			c := readCase(t, f)
			pdf, err := os.ReadFile(filepath.Join(dir, "statements", c.Statement)) //nolint:gosec // local test data
			if err != nil {
				t.Fatal(err)
			}
			runCase(t, loadDoc(t, c.Statement, pdf, c.Override), c)
		})
	}
}
