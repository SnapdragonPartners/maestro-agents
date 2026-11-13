package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/synthpdf"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
)

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"export"}, {"verify"}, {"verify", "-pdf", "x"}, {"verify", "-bogus"}} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), args, &out, &errb); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
}

func TestVerifyCommand(t *testing.T) {
	for _, bin := range []string{"pdftotext", "pdfinfo"} {
		if _, err := exec.LookPath(bin); err != nil {
			if os.Getenv("ADDEPAR_REQUIRE_POPPLER") != "" {
				t.Fatalf("%s not installed (poppler-utils), and ADDEPAR_REQUIRE_POPPLER is set", bin)
			}
			t.Skipf("%s not installed", bin)
		}
	}
	dir := t.TempDir()
	synth := filepath.Join("internal", "verify", "testdata", "synthetic")
	text, err := os.ReadFile(filepath.Join(synth, "brightwater-iv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	pdf, err := synthpdf.Build([]string{string(text)}, synthpdf.Options{CopyRestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	pdfPath := filepath.Join(dir, "s.pdf")
	if err = os.WriteFile(pdfPath, pdf, 0o600); err != nil {
		t.Fatal(err)
	}
	var c struct {
		Record json.RawMessage `json:"record"`
	}
	cb, err := os.ReadFile(filepath.Join(synth, "brightwater-iv.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(cb, &c); err != nil {
		t.Fatal(err)
	}
	recPath := filepath.Join(dir, "r.json")
	if err = os.WriteFile(recPath, c.Record, 0o600); err != nil {
		t.Fatal(err)
	}

	verdict := func(args ...string) (int, verify.Result) {
		t.Helper()
		var out, errb bytes.Buffer
		code := run(context.Background(), append([]string{"verify", "-pdf", pdfPath, "-record", recPath}, args...), &out, &errb)
		var res verify.Result
		if code != exitUsage {
			if err := json.Unmarshal(out.Bytes(), &res); err != nil {
				t.Fatalf("output: %v\n%s", err, out.String())
			}
		}
		return code, res
	}
	// Declined (no override): the restricted text is not read.
	code, res := verdict()
	if code != exitFail || res.Verdict != verify.Fail || res.Checks[0].Findings[0].Code != "text_not_read" {
		t.Fatalf("without override: exit %d, %+v", code, res)
	}
	// Accepted: verified, with the currency warning left for a person.
	code, res = verdict("-override-by", "operator one", "-override-reason", "our own statement")
	if code != exitOK || res.Verdict != verify.Warn {
		t.Fatalf("with override: exit %d, %+v", code, res)
	}
	// An override without a reason is refused.
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"verify", "-pdf", pdfPath, "-record", recPath, "-override-by", "x"}, &out, &errb); code != exitUsage || !strings.Contains(errb.String(), "reason") {
		t.Fatalf("incomplete override: exit %d, %s", code, errb.String())
	}
}
