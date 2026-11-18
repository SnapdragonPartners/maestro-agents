package review

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/decimal"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/export"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
)

var t0 = time.Date(2026, 4, 14, 15, 0, 0, 0, time.UTC)

func rec(amount string) *statement.Record {
	return &statement.Record{
		Investor: statement.Text{Text: "I", Page: 1},
		Fund:     statement.Text{Text: "F", Page: 1},
		Ending:   statement.Amount{Label: "Ending", Amount: decimal.MustParse(amount), Page: 1},
	}
}

func res(v verify.Status) *verify.Result { return &verify.Result{Verdict: v} }

// TestCurrent: the fold (ADR 0021, item 8).
func TestCurrent(t *testing.T) {
	base := &export.Record{ID: "x", Record: rec("1"), Result: *res(verify.Warn)}
	snap := &export.Record{ID: "x"}
	entries := []Entry{
		{N: 1, At: t0, Kind: AcceptWarning, By: "a", Reason: "ok", Finding: "6/c", Result: res(verify.Warn)},
		{N: 2, At: t0.Add(time.Minute), Kind: Correct, By: "a", Reason: "typo", Record: rec("2"), Result: res(verify.Pass), Change: "c"},
		{N: 3, At: t0.Add(2 * time.Minute), Kind: SetEntities, By: "a", Reason: "ids", Owner: &export.Entity{ID: "1"}, Result: res(verify.Pass)},
		{N: 4, At: t0.Add(3 * time.Minute), Kind: Approve, By: "b", Snapshot: snap, Result: res(verify.Pass)},
	}
	s, err := Current(base, entries)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Approved() || s.Approval != 4 || s.Snapshot != snap || s.Entries != 4 {
		t.Fatalf("state %+v", s)
	}
	r := s.Record
	if r.Record.Ending.Amount.String() != "2" || r.Result.Verdict != verify.Pass || r.Owner.ID != "1" || len(r.Reviews) != 3 ||
		r.Approval == nil || r.Approval.By != "b" || r.Origin != "" {
		t.Fatalf("record %+v", r)
	}
	if len(base.Reviews) != 0 || base.Record.Ending.Amount.String() != "1" {
		t.Fatal("Current changed the stored record")
	}

	// Any later entry voids the approval (R-4); a manual entry makes the
	// record a person's.
	more := append(append([]Entry(nil), entries...), Entry{N: 5, At: t0.Add(4 * time.Minute), Kind: Manual, By: "a", Reason: "read", Record: rec("3"), Result: res(verify.Warn)})
	s, err = Current(base, more)
	if err != nil || s.Approved() || s.Record.Approval != nil || s.Record.Origin != export.OriginManual || s.Snapshot != nil {
		t.Fatalf("after a later entry: %+v %v", s, err)
	}

	for name, bad := range map[string][]Entry{
		"gap":                {{N: 2, Kind: AcceptWarning, Result: res(verify.Pass)}},
		"no result":          {{N: 1, Kind: AcceptWarning}},
		"correct, no record": {{N: 1, Kind: Correct, Result: res(verify.Pass)}},
		"approve, no snap":   {{N: 1, Kind: Approve, Result: res(verify.Pass)}},
		"unknown kind":       {{N: 1, Kind: "delete", Result: res(verify.Pass)}},
	} {
		if _, err := Current(base, bad); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSplitRecordID(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	if s, n, ok := SplitRecordID(sha + "/03"); !ok || s != sha || n != 3 {
		t.Fatalf("%s %d %v", s, n, ok)
	}
	for _, bad := range []string{"", sha, sha + "/3", sha + "/00", sha + "/03/01", strings.ToUpper(sha) + "/01", "../" + sha + "/01", sha[:62] + "/01"} {
		if _, _, ok := SplitRecordID(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if EntryKey(sha+"/03", 7) != "reviews/"+sha+"/03/007" {
		t.Fatal(EntryKey(sha+"/03", 7))
	}
}

func TestDiff(t *testing.T) {
	a, b := rec("1"), rec("2")
	b.Lines = []statement.Line{{Kind: statement.Movement, Amount: statement.Amount{Label: "Fee", Amount: decimal.MustParse("-1"), Page: 1}}}
	got := diff(a, b)
	for _, want := range []string{`ending.amount: "1" -> "2"`, `lines.1.label: (none) -> "Fee"`} {
		if !strings.Contains(got, want) {
			t.Errorf("diff %q lacks %q", got, want)
		}
	}
	if d := diff(a, rec("1")); d != "" {
		t.Fatalf("no change: %q", d)
	}
	many := rec("1")
	for range 30 {
		many.Lines = append(many.Lines, statement.Line{Kind: statement.Movement, Amount: statement.Amount{Label: "x", Amount: decimal.MustParse("1"), Page: 1}})
	}
	if d := diff(a, many); !strings.Contains(d, "more") || strings.Count(d, "->") != maxChanges {
		t.Fatalf("capped diff %q", d)
	}
}

func TestDecodeActionStrict(t *testing.T) {
	if _, err := DecodeAction([]byte(`{"kind":"approve","by":"a","at":"2026-04-14T15:00:00Z","seen":1}`)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{"kind":"approve","kind":"export"}`,
		`{"Kind":"approve"}`,
		`{"kind":"approve","extra":1}`,
		`{"kind":"approve"} {}`,
	} {
		if _, err := DecodeAction([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	// An action's JSON round-trips through the strict decoder.
	a := Action{Kind: Correct, Record: "r", By: "a", At: t0, Reason: "x", Extraction: json.RawMessage(`{"a":1}`), Owner: &export.Entity{ID: "1"}}
	b, _ := json.Marshal(a)
	if _, err := DecodeAction(b); err != nil {
		t.Fatalf("round trip: %v", err)
	}
}
