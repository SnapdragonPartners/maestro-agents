package export_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/document"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/export"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/synthpdf"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
)

var t0 = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

func record(t *testing.T, investor, fund, amount, currency string) *statement.Record {
	t.Helper()
	raw := `{
	  "investor": {"text": "` + investor + `", "page": 1},
	  "fund": {"text": "` + fund + `", "page": 1},
	  "period": {"end": "2026-03-31", "label": "For the period ending", "page": 1},
	  "currency": {"code": "` + currency + `"},
	  "precision": "whole",
	  "column": {"label": "QTD", "reason": "period to date", "page": 1},
	  "ending": {"label": "Ending Balance", "amount": "` + amount + `", "page": 1}
	}`
	r, err := statement.DecodeBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// passing is a verification result with every check passed, and warnWith
// one with the given warning.
func passing() verify.Result {
	r := verify.Result{Verdict: verify.Pass}
	for n := 1; n <= 7; n++ {
		r.Checks = append(r.Checks, verify.CheckResult{Check: n, Name: verify.CheckName(n), Status: verify.Pass})
	}
	return r
}

func withFinding(sev verify.Severity, check int, code, subject string) verify.Result {
	r := passing()
	st := verify.Fail
	r.Verdict = verify.Fail
	if sev == verify.Warning {
		st, r.Verdict = verify.Warn, verify.Warn
	}
	r.Checks[check-1].Status = st
	r.Checks[check-1].Findings = []verify.Finding{{Check: check, Code: code, Subject: subject, Severity: sev, Message: "m"}}
	return r
}

func approved(t *testing.T, id string, rec *statement.Record) export.Record {
	t.Helper()
	return export.Record{
		ID:       id,
		Record:   rec,
		Source:   export.Source{SHA256: strings.Repeat("ab", 32), Name: id + ".pdf"},
		Result:   passing(),
		Approval: &export.Approval{By: "operator one", At: t0},
	}
}

func TestBuildCSVExact(t *testing.T) {
	a := approved(t, "r1", record(t, "Example Holdings IV, LLC", "Halyard Growth Fund II, L.P.", "714760.65", "USD"))
	a.Owner = &export.Entity{ID: "1001", Name: "EXAMPLE HOLDINGS IV, LLC"}
	a.Asset = &export.Entity{ID: "2002", Name: "Halyard Growth Fund II, LP"}
	b := approved(t, "r2", record(t, "Sample Partners VII, LLC", "Tributary Fund 3, LP", "6038338", "USD"))
	exp, err := export.Build([]export.Record{b, a}, t0)
	if err != nil {
		t.Fatal(err)
	}
	want := "Direct Owner Entity ID,Direct Owner Name,Direct Owner Model Type,Asset Entity ID,Asset Name,Asset Model Type,Asset CUSIP,Asset ISIN,Asset SEDOL,Asset Ticker Symbol,Asset Ownership Type,Trade Date,Value,Units,Cost Basis,Principal Factor,Cash Position Name,Currency,Comment\r\n" +
		`1001,"EXAMPLE HOLDINGS IV, LLC",,2002,"Halyard Growth Fund II, LP",,,,,,Value Based,2026-03-31,714760.65,,,,,USD,` + "\r\n" +
		`,"Sample Partners VII, LLC",,,"Tributary Fund 3, LP",,,,,,Value Based,2026-03-31,6038338,,,,,USD,` + "\r\n"
	if string(exp.CSV) != want {
		t.Fatalf("CSV:\n%s\nwant:\n%s", exp.CSV, want)
	}
	sum := sha256.Sum256(exp.CSV)
	if exp.CSVSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("CSV hash does not match the bytes")
	}
	var m export.Manifest
	if err := json.Unmarshal(exp.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.CSVSHA256 != exp.CSVSHA256 || len(m.Rows) != 2 || m.Version != export.ManifestVersion {
		t.Fatalf("manifest header: %+v", m)
	}
	if r := m.Rows[0]; r.RecordID != "r1" || r.Row != 1 || r.NamesOnly || r.Value != "714760.65" || r.Page != 1 || r.Approval.By != "operator one" {
		t.Fatalf("row 1: %+v", r)
	}
	if r := m.Rows[1]; r.RecordID != "r2" || !r.NamesOnly || r.Source.SHA256 == "" || len(r.Checks) != 7 {
		t.Fatalf("row 2: %+v", r)
	}
	msum := sha256.Sum256(exp.Manifest)
	if exp.ManifestSHA256 != hex.EncodeToString(msum[:]) {
		t.Fatal("manifest hash does not match the bytes")
	}
}

func TestBuildDeterministic(t *testing.T) {
	recs := []export.Record{
		approved(t, "c", record(t, "B Owner", "Fund Z", "3", "USD")),
		approved(t, "a", record(t, "A Owner", "Fund Y", "1.50", "USD")),
		approved(t, "b", record(t, "A Owner", "Fund X", "-2", "AUD")),
	}
	first, err := export.Build(recs, t0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		perm := []export.Record{recs[(i+1)%3], recs[(i+2)%3], recs[i%3]}
		again, err := export.Build(perm, t0)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first.CSV, again.CSV) || !bytes.Equal(first.Manifest, again.Manifest) {
			t.Fatalf("order %d changed the output", i)
		}
	}
	if !strings.Contains(string(first.CSV), ",1.50,") || !strings.Contains(string(first.CSV), ",-2,") {
		t.Fatalf("values not written in their own precision:\n%s", first.CSV)
	}
}

func TestBuildRefuses(t *testing.T) {
	base := func() export.Record {
		return approved(t, "r", record(t, "Owner", "Fund", "10", "USD"))
	}
	cases := []struct {
		edit func(*export.Record)
		name string
		want string
	}{
		{name: "not approved", edit: func(r *export.Record) { r.Approval = nil }, want: "not approved"},
		{name: "approver unnamed", edit: func(r *export.Record) { r.Approval.By = " " }, want: "not approved"},
		{name: "approval voided by a later change", edit: func(r *export.Record) {
			r.Reviews = []export.ReviewAction{{Kind: "correct", By: "operator two", At: t0.Add(time.Minute), Reason: "fix", Change: "ending"}}
		}, want: "is void"},
		{name: "verification error", edit: func(r *export.Record) { r.Result = withFinding(verify.Error, 2, "does_not_tie", "") }, want: "verification error 2/does_not_tie"},
		{name: "warning not accepted", edit: func(r *export.Record) { r.Result = withFinding(verify.Warning, 6, "not_stated", "currency") }, want: "warning 6/not_stated/currency not accepted"},
		{name: "a different warning accepted", edit: func(r *export.Record) {
			r.Result = withFinding(verify.Warning, 6, "not_stated", "currency")
			r.Reviews = []export.ReviewAction{{Kind: export.AcceptWarning, Finding: "2/rounding_difference", By: "operator one", At: t0.Add(-time.Minute), Reason: "ok"}}
		}, want: "not accepted"},
		{name: "never verified", edit: func(r *export.Record) { r.Result = verify.Result{} }, want: "never verified"},
		// The gate reads the recorded outcome, not only the findings (buggrit f-80eb4d).
		{name: "verdict fail with no findings", edit: func(r *export.Record) { r.Result.Verdict = verify.Fail }, want: `verdict is "fail"`},
		{name: "verdict missing", edit: func(r *export.Record) { r.Result.Verdict = "" }, want: `verdict is ""`},
		{name: "check failed with no findings", edit: func(r *export.Record) { r.Result.Checks[3].Status = verify.Fail }, want: `check 4`},
		{name: "check skipped", edit: func(r *export.Record) { r.Result.Checks[0].Status = verify.Skipped }, want: `is "skipped"`},
		{name: "a check missing", edit: func(r *export.Record) { r.Result.Checks = r.Result.Checks[:6] }, want: "holds 6 checks"},
		{name: "checks out of order", edit: func(r *export.Record) {
			r.Result.Checks[0], r.Result.Checks[1] = r.Result.Checks[1], r.Result.Checks[0]
		}, want: "out of place"},
		{name: "formula in a name", edit: func(r *export.Record) { r.Record.Investor.Text = "=HYPERLINK(\"x\")" }, want: "formula"},
		{name: "formula in an entered name", edit: func(r *export.Record) { r.Asset = &export.Entity{Name: "@SUM(A1)"} }, want: "formula"},
		{name: "no ID", edit: func(r *export.Record) { r.ID = "" }, want: "no ID"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := base()
			c.edit(&r)
			_, err := export.Build([]export.Record{r}, t0)
			if !errors.Is(err, export.ErrRefused) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want refusal containing %q", err, c.want)
			}
		})
	}
	t.Run("listed twice", func(t *testing.T) {
		r := base()
		if _, err := export.Build([]export.Record{r, r}, t0); !errors.Is(err, export.ErrRefused) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if _, err := export.Build(nil, t0); !errors.Is(err, export.ErrRefused) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("accepted warning exports", func(t *testing.T) {
		r := base()
		r.Result = withFinding(verify.Warning, 6, "not_stated", "currency")
		r.Reviews = []export.ReviewAction{{Kind: export.AcceptWarning, Finding: "6/not_stated/currency", By: "operator one", At: t0.Add(-time.Minute), Reason: "USD per the fund's LPA"}}
		exp, err := export.Build([]export.Record{r}, t0)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(exp.Manifest), "USD per the fund's LPA") {
			t.Fatal("acceptance missing from the manifest")
		}
	})
	t.Run("negative value is not a formula", func(t *testing.T) {
		r := base()
		r.Record.Ending.Amount = r.Record.Ending.Amount.Neg()
		if _, err := export.Build([]export.Record{r}, t0); err != nil {
			t.Fatal(err)
		}
	})
}

func TestWrite(t *testing.T) {
	exp, err := export.Build([]export.Record{approved(t, "r", record(t, "Owner", "Fund", "10", "USD"))}, t0)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	csvPath, manPath, err := exp.Write(dir, "valuations-2026-03-31")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]byte{csvPath: exp.CSV, manPath: exp.Manifest} {
		got, err := os.ReadFile(path) //nolint:gosec // test temp dir
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: %v", path, err)
		}
	}
	if _, _, err := exp.Write(dir, "valuations-2026-03-31"); err == nil {
		t.Fatal("an existing export was replaced")
	}
	for _, bad := range []string{"", "..", "a/b"} {
		if _, _, err := exp.Write(dir, bad); err == nil {
			t.Fatalf("name %q accepted", bad)
		}
	}
	left, _ := filepath.Glob(filepath.Join(dir, ".export-*"))
	if len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

// TestEndToEnd takes a synthetic statement from PDF through verification to
// the CSV: the value exported is the one printed, and the manifest carries
// the document's hash, the checks and the warning's acceptance.
func TestEndToEnd(t *testing.T) {
	for _, bin := range []string{"pdftotext", "pdfinfo"} {
		if _, err := exec.LookPath(bin); err != nil {
			if os.Getenv("ADDEPAR_REQUIRE_POPPLER") != "" {
				t.Fatalf("%s not installed (poppler-utils), and ADDEPAR_REQUIRE_POPPLER is set", bin)
			}
			t.Skipf("%s not installed", bin)
		}
	}
	dir := filepath.Join("..", "verify", "testdata", "synthetic")
	text, err := os.ReadFile(filepath.Join(dir, "halyard-ii.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Record json.RawMessage `json:"record"`
	}
	cb, err := os.ReadFile(filepath.Join(dir, "halyard-ii.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(cb, &c); err != nil {
		t.Fatal(err)
	}
	pdf, err := synthpdf.Build([]string{string(text)}, synthpdf.Options{})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := document.Load(context.Background(), "halyard-ii.pdf", pdf, document.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := statement.DecodeBytes(c.Record)
	if err != nil {
		t.Fatal(err)
	}
	res := verify.Verify(doc, rec)
	r := export.Record{ID: "halyard", Record: rec, Source: export.SourceOf(doc), Result: res, Approval: &export.Approval{By: "operator one", At: t0}}
	if _, err = export.Build([]export.Record{r}, t0); !errors.Is(err, export.ErrRefused) {
		t.Fatalf("exported with an unaccepted warning: %v", err)
	}
	for _, f := range res.Findings(verify.Warning) {
		r.Reviews = append(r.Reviews, export.ReviewAction{Kind: export.AcceptWarning, Finding: f.ID(), By: "operator one", At: t0.Add(-time.Minute), Reason: "confirmed USD"})
	}
	exp, err := export.Build([]export.Record{r}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(exp.CSV), ",2026-03-31,714760.65,") {
		t.Fatalf("CSV:\n%s", exp.CSV)
	}
	if !strings.Contains(string(exp.Manifest), doc.SHA256) || !strings.Contains(string(exp.Manifest), `"6/not_stated/currency"`) {
		t.Fatalf("manifest lacks the document hash or the acceptance")
	}
}
