// Package statement defines the extraction record: what the extractor (the
// model, or a person writing one by hand) submits for one investor in one
// fund. Every field is a claim about the document that the verifier checks
// against the PDF's text; nothing here is trusted.
//
// The record carries no hash, identifier or check result: code adds those
// (intake: "The agent never supplies a hash, an ID or a check result").
package statement

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/decimal"
)

// Precision is how a statement prints its amounts.
type Precision string

// The precisions.
const (
	// Cents means amounts are printed to the cent; the roll-forward must tie
	// exactly.
	Cents Precision = "cents"
	// Whole means amounts are printed in whole currency units; the
	// roll-forward may differ by published rounding.
	Whole Precision = "whole"
)

// LineKind says how a roll-forward line takes part in the sum.
type LineKind string

// The line kinds.
const (
	// Movement is a signed line added to the beginning balance.
	Movement LineKind = "movement"
	// Subtotal is a printed subtotal (such as "Total Profit & Loss") that
	// repeats lines already counted. It is grounded but not summed.
	Subtotal LineKind = "subtotal"
)

// Text is a string as printed, with the page it is printed on.
type Text struct {
	Text string `json:"text"`
	Page int    `json:"page"`
}

// Amount is a labeled amount as printed: the label on the statement's line,
// the signed value (parentheses are negative, signs taken as printed), and
// its page.
type Amount struct {
	Label  string          `json:"label"`
	Amount decimal.Decimal `json:"amount"`
	Page   int             `json:"page"`
}

// Line is one roll-forward line between the beginning and ending balances.
type Line struct {
	Kind LineKind `json:"kind"`
	Amount
}

// Period is the statement's period end date and the printed text it was read
// from ("For the period ending March 31, 2026", "01/01/2026 - 03/31/2026").
type Period struct {
	// End is the period end date, YYYY-MM-DD.
	End   string `json:"end"`
	Label string `json:"label"`
	Page  int    `json:"page"`
}

// EndDate parses End.
func (p Period) EndDate() (time.Time, error) {
	t, err := time.Parse(time.DateOnly, p.End)
	if err != nil {
		return time.Time{}, fmt.Errorf("statement: period end %q: %w", p.End, err)
	}
	return t, nil
}

// Currency is the ISO 4217 code and the printed text that states it ("USD",
// "U.S. Dollars"). Evidence is empty when the statement does not state a
// currency; the verifier then warns until a person confirms the code.
type Currency struct {
	Code     string `json:"code"`
	Evidence string `json:"evidence,omitempty"`
	Page     int    `json:"page,omitempty"`
}

// Column is the column the amounts were read from and why it was chosen:
// the period-to-date column, under the investor (not the fund total).
type Column struct {
	// Label is the column header as printed ("Quarter to Date", "QTD").
	Label string `json:"label"`
	// Section is the header over a group of columns, when the table has one
	// ("Limited Partner"); empty otherwise.
	Section string `json:"section,omitempty"`
	Reason  string `json:"reason"`
	Page    int    `json:"page"`
}

// Record is one investor's statement in one fund, as submitted.
type Record struct {
	// Beginning is the beginning balance; nil when the statement prints no
	// roll-forward (a partial roll-forward, check 7).
	Beginning *Amount `json:"beginning,omitempty"`
	// FundTotal is the fund's total ending balance when the statement prints
	// one; the investor's value must be smaller (check 4).
	FundTotal *Amount `json:"fund_total,omitempty"`
	// DashZero, when set, states the extractor's reading that a dash in this
	// table means zero, and why. Without it a dash is not a number.
	DashZero *string  `json:"dash_zero,omitempty"`
	Currency Currency `json:"currency"`
	Investor Text     `json:"investor"`
	Fund     Text     `json:"fund"`
	Period   Period   `json:"period"`
	// Precision is how the statement prints amounts.
	Precision Precision `json:"precision"`
	Column    Column    `json:"column"`
	// Ending is the investor's ending balance: the value exported.
	Ending Amount `json:"ending"`
	// Lines are the roll-forward lines between Beginning and Ending, in the
	// order printed.
	Lines []Line `json:"lines,omitempty"`
	// Repeats are the other places the statement prints the ending balance
	// (a summary box, another page); every one must agree (check 3).
	Repeats []Amount `json:"repeats,omitempty"`
}

// ErrInvalid reports a record that is malformed rather than wrong: a missing
// field, an unknown enumeration value, a page below 1.
var ErrInvalid = errors.New("statement: invalid record")

// Decode reads one record from JSON, refusing unknown fields and trailing
// data.
func Decode(r io.Reader) (*Record, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var rec Record
	if err := dec.Decode(&rec); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing data after the record", ErrInvalid)
	}
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	return &rec, nil
}

// DecodeBytes is Decode over a byte slice.
func DecodeBytes(b []byte) (*Record, error) { return Decode(bytes.NewReader(b)) }

// Validate checks the record's shape: every required field present, pages
// positive, enumerations known. It checks nothing against the document.
func (r *Record) Validate() error {
	var c checker
	c.need(!blank(r.Investor.Text) && r.Investor.Page > 0, "investor text and page")
	c.need(!blank(r.Fund.Text) && r.Fund.Page > 0, "fund text and page")
	c.need(!blank(r.Period.End) && !blank(r.Period.Label) && r.Period.Page > 0, "period end, label and page")
	if !blank(r.Period.End) {
		_, err := r.Period.EndDate()
		c.need(err == nil, "period end as YYYY-MM-DD")
	}
	c.need(len(r.Currency.Code) == 3 && strings.ToUpper(r.Currency.Code) == r.Currency.Code, "currency as a three-letter upper-case code")
	c.need(blank(r.Currency.Evidence) == (r.Currency.Page == 0), "currency evidence and its page together")
	c.need(r.Precision == Cents || r.Precision == Whole, `precision "cents" or "whole"`)
	c.need(!blank(r.Column.Label) && !blank(r.Column.Reason) && r.Column.Page > 0, "column label, reason and page")
	if r.DashZero != nil {
		c.need(!blank(*r.DashZero), "a reason with dash_zero")
	}
	r.validateAmounts(&c)
	if len(c.problems) > 0 {
		return fmt.Errorf("%w: needs %s", ErrInvalid, strings.Join(c.problems, "; "))
	}
	return nil
}

func (r *Record) validateAmounts(c *checker) {
	c.amount(&r.Ending, "ending")
	if r.Beginning != nil {
		c.amount(r.Beginning, "beginning")
	} else {
		c.need(len(r.Lines) == 0, "a beginning balance when lines are given")
	}
	if r.FundTotal != nil {
		c.amount(r.FundTotal, "fund total")
	}
	for i := range r.Lines {
		l := &r.Lines[i]
		c.amount(&l.Amount, fmt.Sprintf("line %d", i+1))
		c.need(l.Kind == Movement || l.Kind == Subtotal, fmt.Sprintf(`line %d kind "movement" or "subtotal"`, i+1))
	}
	for i := range r.Repeats {
		c.amount(&r.Repeats[i], fmt.Sprintf("repeat %d", i+1))
	}
}

type checker struct{ problems []string }

func (c *checker) need(ok bool, what string) {
	if !ok {
		c.problems = append(c.problems, what)
	}
}

func (c *checker) amount(a *Amount, what string) {
	c.need(!blank(a.Label) && a.Page > 0, what+" label and page")
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }

// Clone returns a deep copy, for mutation in tests and corrections.
func (r *Record) Clone() *Record {
	b, err := json.Marshal(r)
	if err != nil {
		panic(fmt.Sprintf("statement: marshal record: %v", err))
	}
	var c Record
	if err := json.Unmarshal(b, &c); err != nil {
		panic(fmt.Sprintf("statement: unmarshal record: %v", err))
	}
	return &c
}
