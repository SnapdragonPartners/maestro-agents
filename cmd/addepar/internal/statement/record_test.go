package statement_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
)

const valid = `{
  "investor": {"text": "Example Holdings IV, LLC", "page": 1},
  "fund": {"text": "Halyard Growth Fund II, L.P.", "page": 1},
  "period": {"end": "2026-03-31", "label": "For the period ending March 31, 2026", "page": 1},
  "currency": {"code": "USD"},
  "precision": "cents",
  "column": {"label": "Quarter to Date", "reason": "period to date", "page": 1},
  "beginning": {"label": "Opening Balance", "amount": "604211.40", "page": 1},
  "lines": [{"kind": "movement", "label": "Capital Contribution", "amount": "125000.00", "page": 1}],
  "ending": {"label": "Capital Balance", "amount": "729211.40", "page": 1}
}`

func TestDecodeValid(t *testing.T) {
	r, err := statement.DecodeBytes([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if r.Ending.Amount.String() != "729211.40" || len(r.Lines) != 1 || r.Beginning == nil {
		t.Fatalf("%+v", r)
	}
	// An explicit "0" is a value.
	z, err := statement.DecodeBytes([]byte(strings.Replace(valid, `"amount": "125000.00"`, `"amount": "0"`, 1)))
	if err != nil || !z.Lines[0].Amount.Amount.IsSet() || z.Lines[0].Amount.Amount.Sign() != 0 {
		t.Fatalf("an explicit zero: %+v %v", z, err)
	}
	c := r.Clone()
	c.Lines[0].Label = "changed"
	if r.Lines[0].Label == "changed" {
		t.Fatal("Clone shares lines")
	}
}

func TestDecodeRejects(t *testing.T) {
	cases := map[string]func(string) string{
		"unknown field":           func(s string) string { return strings.Replace(s, `"precision"`, `"hash": "x", "precision"`, 1) },
		"trailing data":           func(s string) string { return s + `{}` },
		"amount as a JSON number": func(s string) string { return strings.Replace(s, `"729211.40"`, `729211.40`, 1) },
		"bad precision":           func(s string) string { return strings.Replace(s, `"cents"`, `"thousands"`, 1) },
		"bad date":                func(s string) string { return strings.Replace(s, `"2026-03-31"`, `"03/31/2026"`, 1) },
		"lower-case currency":     func(s string) string { return strings.Replace(s, `"USD"`, `"usd"`, 1) },
		"evidence without page": func(s string) string {
			return strings.Replace(s, `{"code": "USD"}`, `{"code": "USD", "evidence": "USD"}`, 1)
		},
		"page zero":      func(s string) string { return strings.Replace(s, `"page": 1}`, `"page": 0}`, 1) },
		"blank investor": func(s string) string { return strings.Replace(s, `"Example Holdings IV, LLC"`, `" "`, 1) },
		"bad line kind":  func(s string) string { return strings.Replace(s, `"movement"`, `"total"`, 1) },
		"lines without beginning": func(s string) string {
			return strings.Replace(s, `"beginning": {"label": "Opening Balance", "amount": "604211.40", "page": 1},`, "", 1)
		},
		"empty dash reason": func(s string) string { return strings.Replace(s, `"precision"`, `"dash_zero": "", "precision"`, 1) },
		"no column reason":  func(s string) string { return strings.Replace(s, `"reason": "period to date"`, `"reason": ""`, 1) },
		// Issue #21: encoding/json keeps the last of a repeated key and
		// matches keys case-insensitively, so the record could be read
		// two ways.
		"repeated top-level key": func(s string) string {
			return strings.Replace(s, `"precision": "cents",`, `"precision": "cents", "precision": "whole",`, 1)
		},
		"repeated nested key": func(s string) string {
			return strings.Replace(s, `"amount": "729211.40"`, `"amount": "729211.40", "amount": "1.00"`, 1)
		},
		"case-mismatched key":  func(s string) string { return strings.Replace(s, `"ending"`, `"Ending"`, 1) },
		"case-mismatched line": func(s string) string { return strings.Replace(s, `"kind": "movement"`, `"KIND": "movement"`, 1) },
		"invalid UTF-8":        func(s string) string { return strings.Replace(s, `Opening`, "Open\xffing", 1) },
		"oversized":            func(s string) string { return s + strings.Repeat(" ", statement.MaxRecordBytes) },
		// PR #22: an absent or null amount is not zero.
		"ending amount absent": func(s string) string {
			return strings.Replace(s, `"ending": {"label": "Capital Balance", "amount": "729211.40", "page": 1}`, `"ending": {"label": "Capital Balance", "page": 1}`, 1)
		},
		"ending amount null": func(s string) string { return strings.Replace(s, `"amount": "729211.40"`, `"amount": null`, 1) },
		"line amount null":   func(s string) string { return strings.Replace(s, `"amount": "125000.00"`, `"amount": null`, 1) },
		"beginning amount absent": func(s string) string {
			return strings.Replace(s, `"amount": "604211.40", `, ``, 1)
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			in := edit(valid)
			if in == valid {
				t.Fatal("edit did not apply")
			}
			if _, err := statement.DecodeBytes([]byte(in)); !errors.Is(err, statement.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestPartialRecordIsValid(t *testing.T) {
	in := strings.Replace(valid, `"beginning": {"label": "Opening Balance", "amount": "604211.40", "page": 1},`, "", 1)
	in = strings.Replace(in, `"lines": [{"kind": "movement", "label": "Capital Contribution", "amount": "125000.00", "page": 1}],`, "", 1)
	if _, err := statement.DecodeBytes([]byte(in)); err != nil {
		t.Fatalf("a record with only an ending is valid (check 7 warns): %v", err)
	}
}
