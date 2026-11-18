package ui

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
)

// blankRows is how many empty line and repeat rows a form offers.
const blankRows = 3

// maxRows bounds the line and repeat rows a form submits.
const maxRows = 200

// AmountForm is an amount's form fields.
type AmountForm struct{ Label, Amount, Page string }

// LineForm is a roll-forward line's form fields.
type LineForm struct {
	Kind string
	AmountForm
}

// RecordForm is an extraction record as form fields: what the correct
// and manual forms show, and read back (ADR 0021, item 7).
type RecordForm struct {
	InvestorText, InvestorPage                           string
	FundText, FundPage                                   string
	PeriodEnd, PeriodLabel, PeriodPage                   string
	CurrencyCode, CurrencyEvidence, CurrencyPage         string
	Precision                                            string
	ColumnLabel, ColumnSection, ColumnReason, ColumnPage string
	DashZero                                             string
	Beginning, Ending, FundTotal                         AmountForm
	Lines                                                []LineForm
	Repeats                                              []AmountForm
}

func page(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func amountForm(a *statement.Amount) AmountForm {
	if a == nil {
		return AmountForm{}
	}
	f := AmountForm{Label: a.Label, Page: page(a.Page)}
	if a.Amount.IsSet() {
		f.Amount = a.Amount.String()
	}
	return f
}

// formOf fills the form from a record; nil gives a blank form.
func formOf(r *statement.Record) RecordForm {
	f := RecordForm{Precision: string(statement.Cents)}
	if r != nil {
		f = RecordForm{
			InvestorText: r.Investor.Text, InvestorPage: page(r.Investor.Page),
			FundText: r.Fund.Text, FundPage: page(r.Fund.Page),
			PeriodEnd: r.Period.End, PeriodLabel: r.Period.Label, PeriodPage: page(r.Period.Page),
			CurrencyCode: r.Currency.Code, CurrencyEvidence: r.Currency.Evidence, CurrencyPage: page(r.Currency.Page),
			Precision:   string(r.Precision),
			ColumnLabel: r.Column.Label, ColumnSection: r.Column.Section, ColumnReason: r.Column.Reason, ColumnPage: page(r.Column.Page),
			Beginning: amountForm(r.Beginning), Ending: amountForm(&r.Ending), FundTotal: amountForm(r.FundTotal),
		}
		if r.DashZero != nil {
			f.DashZero = *r.DashZero
		}
		for i := range r.Lines {
			f.Lines = append(f.Lines, LineForm{Kind: string(r.Lines[i].Kind), AmountForm: amountForm(&r.Lines[i].Amount)})
		}
		for i := range r.Repeats {
			f.Repeats = append(f.Repeats, amountForm(&r.Repeats[i]))
		}
	}
	for range blankRows {
		f.Lines = append(f.Lines, LineForm{Kind: string(statement.Movement)})
		f.Repeats = append(f.Repeats, AmountForm{})
	}
	return f
}

// formError is a form a person filled in wrongly: the message is theirs.
type formError struct{ msg string }

func (e *formError) Error() string { return e.msg }

func formErr(format string, args ...any) error { return &formError{msg: fmt.Sprintf(format, args...)} }

// formReader reads a posted form's fields.
type formReader struct {
	v   url.Values
	err error
}

func (f *formReader) text(name string) string { return strings.TrimSpace(f.v.Get(name)) }

// page reads a page number: blank is 0, which the record's validation
// refuses where a page is required.
func (f *formReader) page(name, what string) int {
	s := f.text(name)
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		if f.err == nil {
			f.err = formErr("%s's page must be a page number, not %q", what, s)
		}
		return 0
	}
	return n
}

// amount reads an amount's fields as JSON: nil when every field is
// blank and the amount is optional. The amount stays a string, which the
// record's strict decoding checks (a bad one is refused as malformed).
func (f *formReader) amount(prefix, what string, optional bool) map[string]any {
	label, amt, pg := f.text(prefix+".label"), f.text(prefix+".amount"), f.text(prefix+".page")
	if optional && label == "" && amt == "" && pg == "" {
		return nil
	}
	m := map[string]any{"label": label, "page": f.page(prefix+".page", what)}
	if amt != "" {
		m["amount"] = normalizeAmount(amt)
	}
	return m
}

// normalizeAmount reads an amount as a person types it: separators and a
// currency sign dropped, parentheses as a minus.
func normalizeAmount(s string) string {
	s = strings.NewReplacer(",", "", "$", "", " ", "").Replace(s)
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		s = "-" + s[1:len(s)-1]
	}
	return s
}

// parseRecordForm builds the extraction record's JSON from the posted
// form, in the record's exact shape, for the review run to decode
// strictly and verify (ADR 0021, item 13).
func parseRecordForm(v url.Values) (json.RawMessage, error) {
	f := &formReader{v: v}
	m := map[string]any{
		"investor":  map[string]any{"text": f.text("investor.text"), "page": f.page("investor.page", "the investor")},
		"fund":      map[string]any{"text": f.text("fund.text"), "page": f.page("fund.page", "the fund")},
		"period":    map[string]any{"end": f.text("period.end"), "label": f.text("period.label"), "page": f.page("period.page", "the period")},
		"precision": f.text("precision"),
		"column": map[string]any{
			"label": f.text("column.label"), "section": f.text("column.section"),
			"reason": f.text("column.reason"), "page": f.page("column.page", "the column"),
		},
		"ending": f.amount("ending", "the ending balance", false),
	}
	cur := map[string]any{"code": strings.ToUpper(f.text("currency.code"))}
	if e := f.text("currency.evidence"); e != "" {
		cur["evidence"] = e
		cur["page"] = f.page("currency.page", "the currency")
	}
	m["currency"] = cur
	if b := f.amount("beginning", "the beginning balance", true); b != nil {
		m["beginning"] = b
	}
	if t := f.amount("fund_total", "the fund total", true); t != nil {
		m["fund_total"] = t
	}
	if d := f.text("dash_zero"); d != "" {
		m["dash_zero"] = d
	}
	lines := make([]map[string]any, 0, blankRows)
	for i := range maxRows {
		p := fmt.Sprintf("lines.%d", i)
		if !v.Has(p + ".label") {
			break
		}
		a := f.amount(p, fmt.Sprintf("line %d", i+1), true)
		if a == nil {
			continue
		}
		a["kind"] = f.text(p + ".kind")
		lines = append(lines, a)
	}
	if len(lines) > 0 {
		m["lines"] = lines
	}
	var repeats []map[string]any
	for i := range maxRows {
		p := fmt.Sprintf("repeats.%d", i)
		if !v.Has(p + ".label") {
			break
		}
		if a := f.amount(p, fmt.Sprintf("repeat %d", i+1), true); a != nil {
			repeats = append(repeats, a)
		}
	}
	if len(repeats) > 0 {
		m["repeats"] = repeats
	}
	if f.err != nil {
		return nil, f.err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode the record: %w", err)
	}
	return b, nil
}
