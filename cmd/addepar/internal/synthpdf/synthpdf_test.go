package synthpdf_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/document"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/synthpdf"
)

func needPoppler(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"pdftotext", "pdfinfo"} {
		if _, err := exec.LookPath(bin); err != nil {
			if os.Getenv("ADDEPAR_REQUIRE_POPPLER") != "" {
				t.Fatalf("%s not installed (poppler-utils), and ADDEPAR_REQUIRE_POPPLER is set", bin)
			}
			t.Skipf("%s not installed (poppler-utils)", bin)
		}
	}
}

func TestBuildReadsBackInLayout(t *testing.T) {
	needPoppler(t)
	pages := []string{
		"Example Fund (I), L.P.\n\nOpening Balance        $1,000.00      $2,000.00\nEnding \\ Balance        (5.00)",
		"Second page",
	}
	for _, restricted := range []bool{false, true} {
		pdf, err := synthpdf.Build(pages, synthpdf.Options{CopyRestricted: restricted})
		if err != nil {
			t.Fatal(err)
		}
		var ov *document.Override
		doc, err := document.Load(context.Background(), "x.pdf", pdf, document.Options{Override: ov})
		if err != nil {
			t.Fatal(err)
		}
		if doc.PageCount != 2 || doc.Restriction.CopyRestricted() != restricted {
			t.Fatalf("restricted=%v: pages %d, restriction %+v", restricted, doc.PageCount, doc.Restriction)
		}
		if restricted {
			if doc.TextLayer != document.TextNotRead || doc.Pages != nil {
				t.Fatalf("restricted PDF read without an override: %+v", doc)
			}
			continue
		}
		// Layout mode re-spaces columns on its own grid; the words and their
		// order on each line are what must survive.
		text := strings.Join(strings.Fields(doc.Pages[0].Text), " ")
		want := strings.Join(strings.Fields(pages[0]), " ")
		if text != want {
			t.Errorf("page 1 reads back as %q, want %q", text, want)
		}
	}
}

func TestBuildRestrictedReadsWithPoppler(t *testing.T) {
	needPoppler(t)
	pdf, err := synthpdf.Build([]string{"Restricted Statement   1,234"}, synthpdf.Options{CopyRestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("pdftotext", "-layout", "-", "-")
	cmd.Stdin = bytes.NewReader(pdf)
	got, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "Restricted Statement   1,234") {
		t.Fatalf("encrypted content did not decrypt: %q", got)
	}
	info := exec.Command("pdfinfo", "-")
	info.Stdin = bytes.NewReader(pdf)
	rep, err := info.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rep), "copy:no") || !strings.Contains(string(rep), "print:yes") {
		t.Fatalf("pdfinfo report: %s", rep)
	}
}

func TestBuildRefusesNonASCII(t *testing.T) {
	if _, err := synthpdf.Build([]string{"caf\u00e9"}, synthpdf.Options{}); err == nil {
		t.Fatal("non-ASCII accepted")
	}
}
