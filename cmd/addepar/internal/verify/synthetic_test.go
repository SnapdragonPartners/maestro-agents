package verify_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/document"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/synthpdf"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
)

const syntheticDir = "testdata/synthetic"

// syntheticPDF renders a synthetic statement's text to a PDF.
func syntheticPDF(t *testing.T, c caseFile) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(syntheticDir, c.Statement)) //nolint:gosec // test data path
	if err != nil {
		t.Fatal(err)
	}
	pages := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n<<<page>>>\n")
	pdf, err := synthpdf.Build(pages, synthpdf.Options{CopyRestricted: c.CopyRestricted, FileID: c.Statement})
	if err != nil {
		t.Fatal(err)
	}
	return pdf
}

// TestSyntheticStatements is build step 1's acceptance test on committed
// data: every hand-written record verifies with exactly its expected
// findings, and every mutation fails the checks it names.
func TestSyntheticStatements(t *testing.T) {
	needPoppler(t)
	files := caseFiles(t, syntheticDir)
	if len(files) < 7 {
		t.Fatalf("found %d synthetic cases, want at least 7", len(files))
	}
	for _, f := range files {
		t.Run(strings.TrimSuffix(filepath.Base(f), ".json"), func(t *testing.T) {
			c := readCase(t, f)
			runCase(t, loadDoc(t, c.Statement, syntheticPDF(t, c), c.Override), c)
		})
	}
}

// TestCopyRestrictedWithoutOverride is the decline path: without an
// operator's acceptance the text is never read, and grounding fails with
// the reason, sending the statement to manual entry.
func TestCopyRestrictedWithoutOverride(t *testing.T) {
	needPoppler(t)
	c := readCase(t, filepath.Join(syntheticDir, "brightwater-iv.json"))
	doc := loadDoc(t, c.Statement, syntheticPDF(t, c), nil)
	if doc.TextLayer != document.TextNotRead || doc.Pages != nil || doc.Override != nil {
		t.Fatalf("restricted document read without an override: layer %s, %d pages", doc.TextLayer, len(doc.Pages))
	}
	res := verify.Verify(doc, decodeRecord(t, c.Record))
	if res.Verdict != verify.Fail {
		t.Fatalf("verdict %s, want fail", res.Verdict)
	}
	errs := findingIDs(res.Findings(verify.Error))
	if len(errs) != 1 || errs[0] != "1/text_not_read" {
		t.Fatalf("errors %v, want [1/text_not_read]\n%s", errs, describe(&res))
	}
	for _, n := range []int{verify.CheckConsistency, verify.CheckInvestor, verify.CheckPeriod, verify.CheckCurrency} {
		if s := res.Checks[n-1].Status; s != verify.Skipped {
			t.Errorf("check %d status %s, want skipped", n, s)
		}
	}
}

// TestCopyRestrictedOverrideRecorded checks the accept path keeps the
// operator's acceptance on the document, for the manifest.
func TestCopyRestrictedOverrideRecorded(t *testing.T) {
	needPoppler(t)
	c := readCase(t, filepath.Join(syntheticDir, "brightwater-iv.json"))
	doc := loadDoc(t, c.Statement, syntheticPDF(t, c), c.Override)
	if doc.Override == nil || doc.Override.By != c.Override.By || doc.TextLayer != document.TextUsable {
		t.Fatalf("override not recorded: %+v", doc)
	}
}
