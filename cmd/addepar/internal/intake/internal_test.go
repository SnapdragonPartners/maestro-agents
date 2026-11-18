package intake

import (
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/decimal"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
)

func rec(investor, fund string, page int, label, amount string) *statement.Record {
	return &statement.Record{
		Investor: statement.Text{Text: investor, Page: page},
		Fund:     statement.Text{Text: fund, Page: page},
		Ending:   statement.Amount{Label: label, Amount: decimal.MustParse(amount), Page: page},
	}
}

// TestSamePosition: what the duplicate check treats as one position, and
// what it lets through as distinct (ADR 0018, item 4).
func TestSamePosition(t *testing.T) {
	base := rec("Example Holdings IV, LLC", "Keelson Partners VI, L.P.", 1, "Ending capital", "678250")
	same := map[string]*statement.Record{
		"identical":             rec("Example Holdings IV, LLC", "Keelson Partners VI, L.P.", 1, "Ending capital", "678250"),
		"investor prefix":       rec("Example Holdings IV", "Keelson Partners VI, L.P.", 2, "Other", "1"),
		"investor suffix":       rec("Holdings IV, LLC", "Keelson Partners VI, L.P.", 2, "Other", "1"),
		"investor longer":       rec("Example Holdings IV, LLC Series A", "Keelson Partners VI", 2, "Other", "1"),
		"spelling":              rec("EXAMPLE  HOLDINGS IV LLC", "Keelson Partners VI, L.P.", 2, "Other", "1"),
		"same ending":           rec("Another Investor", "Another Fund", 1, "Ending capital", "678250.00"),
		"same ending, footnote": rec("Another Investor", "Another Fund", 1, "Ending capital2", "678250"),
		// A table repeated on a later page: disjoint fragments of the one
		// printed name, each grounding there, and the same balance.
		"disjoint fragment, repeated page":    rec("Example", "Keelson Partners VI, L.P.", 3, "Ending capital", "678250"),
		"overlapping fragment, repeated page": rec("Holdings IV", "Partners VI", 3, "Ending capital", "678250"),
		"same amount, other label":            rec("Example Holdings V, LLC", "Keelson Partners VI, L.P.", 1, "Closing balance", "678250"),
		"repeat printed rounded":              rec("Another Investor", "Another Fund", 4, "Capital, end of period", "678249.60"),
		"repeat rounded up":                   rec("Another Investor", "Another Fund", 4, "Ending capital", "678251"),
	}
	for name, r := range same {
		if samePosition(base, r) == "" || samePosition(r, base) == "" {
			t.Errorf("%s: treated as a distinct position", name)
		}
	}
	distinct := map[string]*statement.Record{
		"other investor":         rec("Example Holdings V, LLC", "Keelson Partners VI, L.P.", 2, "Ending capital", "1000"),
		"other fund":             rec("Example Holdings IV, LLC", "Keelson Partners VII, L.P.", 2, "Ending capital", "1000"),
		"beyond rounding":        rec("Another Investor", "Another Fund", 1, "Ending capital", "678251.01"),
		"beyond rounding, below": rec("Another Investor", "Another Fund", 1, "Ending capital", "678248.99"),
		"digit kept":             rec("Example Holdings IV, LLC", "Keelson Partners 6, L.P.", 2, "Ending capital", "1000"),
	}
	for name, r := range distinct {
		if why := samePosition(base, r); why != "" {
			t.Errorf("%s: treated as the same position: %s", name, why)
		}
	}
}

// TestReading: a PDF within both limits is sent; over either, the text alone
// is read, with a note naming the limit (ADR 0019).
func TestReading(t *testing.T) {
	for _, c := range []struct {
		pages, size int
		mode, note  string
	}{
		{1, 1, ReadingPDFAndText, ""},
		{MaxPDFPages, MaxPDFBytes, ReadingPDFAndText, ""},
		{MaxPDFPages + 1, 1, ReadingTextOnly, "101 pages"},
		{1, MaxPDFBytes + 1, ReadingTextOnly, "20971521 bytes"},
	} {
		mode, note := reading(c.pages, c.size)
		if mode != c.mode || (c.note == "") != (note == "") || !strings.Contains(note, c.note) {
			t.Errorf("reading(%d, %d) = %q, %q; want %q with %q", c.pages, c.size, mode, note, c.mode, c.note)
		}
	}
}

// TestDecodeSubmissionStrict: the submission envelope and each record
// decode strictly (issue #21, ADR 0021 item 13), so a model's repeated or
// case-mismatched key is a problem it is told about, not a value silently
// chosen.
func TestDecodeSubmissionStrict(t *testing.T) {
	rec := `{"investor":{"text":"I","page":1},"fund":{"text":"F","page":1},` +
		`"period":{"end":"2026-03-31","label":"March 31, 2026","page":1},"currency":{"code":"USD"},` +
		`"precision":"cents","column":{"label":"QTD","reason":"r","page":1},` +
		`"ending":{"label":"Ending","amount":"1.00","page":1}}`
	if got, msg := decodeSubmission([]byte(`{"records":[` + rec + `]}`)); msg != "" || len(got) != 1 || got[0].problem != "" {
		t.Fatalf("valid submission: %+v %q", got, msg)
	}
	if _, msg := decodeSubmission([]byte(`{"records":[],"records":[` + rec + `]}`)); !strings.Contains(msg, "repeated") {
		t.Fatalf("repeated envelope key: %q", msg)
	}
	if _, msg := decodeSubmission([]byte(`{"Records":[` + rec + `]}`)); !strings.Contains(msg, "unknown field") {
		t.Fatalf("case-mismatched envelope key: %q", msg)
	}
	dup := strings.Replace(rec, `"amount":"1.00"`, `"amount":"1.00","amount":"2.00"`, 1)
	// A repeated key inside a record refuses the whole submission, naming
	// the record by its path: the model resubmits.
	if _, msg := decodeSubmission([]byte(`{"records":[` + rec + `,` + dup + `]}`)); !strings.Contains(msg, `/records/1/ending: key "amount" is repeated`) {
		t.Fatalf("repeated record key: %q", msg)
	}
	ci := strings.Replace(rec, `"ending"`, `"Ending"`, 1)
	got, msg := decodeSubmission([]byte(`{"records":[` + rec + `,` + ci + `]}`))
	if msg != "" || len(got) != 2 || got[0].problem != "" || !strings.Contains(got[1].problem, "unknown field") {
		t.Fatalf("case-mismatched record key: %+v %q", got, msg)
	}
}
