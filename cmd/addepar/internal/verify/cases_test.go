package verify_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/decimal"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/document"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
)

// A case file pairs a statement PDF with a hand-written extraction record
// and what verifying it must give. The synthetic cases are committed under
// testdata/synthetic; the real samples' cases live in the gitignored
// .local/addepar/cases (see local_test.go).
type caseFile struct {
	Override  *document.Override `json:"override,omitempty"`
	Statement string             `json:"statement"`
	// CopyRestricted renders a synthetic statement copy-restricted.
	CopyRestricted bool            `json:"copy_restricted,omitempty"`
	Answer         string          `json:"answer"`
	Record         json.RawMessage `json:"record"`
	Expect         struct {
		Verdict  verify.Status `json:"verdict"`
		Warnings []string      `json:"warnings"`
		Errors   []string      `json:"errors"`
	} `json:"expect"`
	Mutations []mutation `json:"mutations,omitempty"`
}

// mutation is a wrong copy of the case's record: fields set by dotted path
// ("lines.2.amount"; null removes a field) and the checks that must fail.
type mutation struct {
	Set  map[string]any `json:"set"`
	Name string         `json:"name"`
	Fail []int          `json:"fail"`
	// Exact requires exactly Fail to fail; otherwise Fail must be among the
	// failures.
	Exact bool `json:"exact,omitempty"`
}

func readCase(t *testing.T, path string) caseFile {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test data path
	if err != nil {
		t.Fatal(err)
	}
	var c caseFile
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return c
}

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

func loadDoc(t *testing.T, name string, pdf []byte, ov *document.Override) *document.Document {
	t.Helper()
	doc, err := document.Load(context.Background(), name, pdf, document.Options{Override: ov})
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return doc
}

func decodeRecord(t *testing.T, raw []byte) *statement.Record {
	t.Helper()
	rec, err := statement.DecodeBytes(raw)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	return rec
}

// runCase verifies the case's record and every mutation of it: the record
// must verify with exactly the expected warnings and no error, and its value
// must be the answer; each mutation must fail the checks it names.
func runCase(t *testing.T, doc *document.Document, c caseFile) {
	t.Helper()
	rec := decodeRecord(t, c.Record)
	res := verify.Verify(doc, rec)
	if res.Verdict != c.Expect.Verdict {
		t.Errorf("verdict %s, want %s\n%s", res.Verdict, c.Expect.Verdict, describe(&res))
	}
	if got, want := findingIDs(res.Findings(verify.Warning)), sorted(c.Expect.Warnings); !slices.Equal(got, want) {
		t.Errorf("warnings %v, want %v\n%s", got, want, describe(&res))
	}
	if got, want := findingIDs(res.Findings(verify.Error)), sorted(c.Expect.Errors); !slices.Equal(got, want) {
		t.Errorf("errors %v, want %v\n%s", got, want, describe(&res))
	}
	if c.Answer != "" && !rec.Ending.Amount.Equal(decimal.MustParse(c.Answer)) {
		t.Errorf("record's ending %s is not the answer %s", rec.Ending.Amount, c.Answer)
	}
	muts := append(slices.Clone(c.Mutations), genericMutations(rec)...)
	for _, m := range muts {
		t.Run(m.Name, func(t *testing.T) {
			mr := mutate(t, c.Record, m.Set)
			res := verify.Verify(doc, mr)
			failed := res.Failed()
			t.Logf("failed %v; errors %v", failed, findingIDs(res.Findings(verify.Error)))
			if m.Exact {
				if !slices.Equal(failed, sortedInts(m.Fail)) {
					t.Errorf("failed checks %v, want exactly %v\n%s", failed, m.Fail, describe(&res))
				}
				return
			}
			for _, want := range m.Fail {
				if !slices.Contains(failed, want) {
					t.Errorf("check %d (%s) did not fail; failed %v\n%s", want, verify.CheckName(want), failed, describe(&res))
				}
			}
		})
	}
}

// genericMutations are the mistakes every case must catch, built from the
// record itself: one digit changed in the ending and in a movement, the
// period end moved, the investor renamed, the roll-forward dropped, the
// column named by a printed title rather than its header, and the precision
// flipped.
func genericMutations(rec *statement.Record) []mutation {
	muts := []mutation{
		{Name: "one digit changed in the ending", Set: map[string]any{"ending.amount": bumpDigit(rec.Ending.Amount)}, Fail: []int{verify.CheckGrounding, verify.CheckRollForward}},
		{Name: "period end moved a quarter", Set: map[string]any{"period.end": shiftQuarter(rec.Period.End)}, Fail: []int{verify.CheckPeriod}},
		{Name: "investor not printed", Set: map[string]any{"investor.text": rec.Investor.Text + " II"}, Fail: []int{verify.CheckGrounding}},
		{Name: "roll-forward dropped", Set: map[string]any{"beginning": nil, "lines": nil}, Fail: []int{verify.CheckRollForward}},
		// Printed, but not a column header: no header row confirms the column.
		{Name: "column named by the fund's title", Set: map[string]any{"column.label": rec.Fund.Text, "column.page": rec.Fund.Page}, Fail: []int{verify.CheckGrounding}},
	}
	for i, l := range rec.Lines {
		if l.Kind == statement.Movement && l.Amount.Amount.Sign() != 0 {
			muts = append(muts, mutation{
				Name: "one digit changed in a movement",
				Set:  map[string]any{fmt.Sprintf("lines.%d.amount", i): bumpDigit(l.Amount.Amount)},
				Fail: []int{verify.CheckGrounding, verify.CheckRollForward},
			})
			break
		}
	}
	flip := statement.Whole
	if rec.Precision == statement.Whole {
		flip = statement.Cents
	}
	muts = append(muts, mutation{Name: "precision flipped", Set: map[string]any{"precision": string(flip)}, Fail: []int{verify.CheckRollForward}})
	return muts
}

// bumpDigit changes one digit before the point: the thousands digit when
// there is one, so that the change is outside any rounding tolerance and the
// roll-forward must catch it as well as grounding, else the last digit.
func bumpDigit(d decimal.Decimal) string {
	s := d.String()
	whole, frac, hasFrac := strings.Cut(s, ".")
	i := len(whole) - 1
	if digits := strings.TrimPrefix(whole, "-"); len(digits) >= 4 {
		i = len(whole) - 4
	}
	b := []byte(whole)
	if b[i] == '9' {
		b[i] = '8'
	} else {
		b[i]++
	}
	if hasFrac {
		return string(b) + "." + frac
	}
	return string(b)
}

func shiftQuarter(end string) string {
	y, _ := strconv.Atoi(end[:4])
	m, _ := strconv.Atoi(end[5:7])
	m -= 3
	if m < 1 {
		m += 12
		y--
	}
	day := map[int]int{3: 31, 6: 30, 9: 30, 12: 31}[m]
	if day == 0 {
		day = 28
	}
	return fmt.Sprintf("%04d-%02d-%02d", y, m, day)
}

// mutate applies dotted-path sets to the raw record and decodes it.
func mutate(t *testing.T, raw json.RawMessage, set map[string]any) *statement.Record {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		setPath(t, m, strings.Split(k, "."), set[k])
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := statement.DecodeBytes(b)
	if err != nil {
		t.Fatalf("mutated record: %v", err)
	}
	return rec
}

func setPath(t *testing.T, node any, path []string, val any) {
	t.Helper()
	key := path[0]
	last := len(path) == 1
	switch n := node.(type) {
	case map[string]any:
		if last {
			if val == nil {
				delete(n, key)
			} else {
				n[key] = val
			}
			return
		}
		next, ok := n[key]
		if !ok {
			next = map[string]any{}
			n[key] = next
		}
		setPath(t, next, path[1:], val)
	case []any:
		i, err := strconv.Atoi(key)
		if err != nil || i < 0 || i >= len(n) {
			t.Fatalf("bad index %q", key)
		}
		if last {
			n[i] = val
			return
		}
		setPath(t, n[i], path[1:], val)
	default:
		t.Fatalf("cannot set %q in %T", key, node)
	}
}

func findingIDs(fs []verify.Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.ID())
	}
	sort.Strings(out)
	return out
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

func sortedInts(s []int) []int {
	out := slices.Clone(s)
	sort.Ints(out)
	return out
}

func describe(r *verify.Result) string {
	var b strings.Builder
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "  %d %s: %s\n", c.Check, c.Name, c.Status)
		for _, f := range c.Findings {
			fmt.Fprintf(&b, "      %s %s: %s\n", f.Severity, f.ID(), f.Message)
		}
	}
	return b.String()
}

// caseFiles lists the *.json case files in dir.
func caseFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}
