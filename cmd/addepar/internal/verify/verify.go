// Package verify runs the intake agent's seven MVP checks on an extraction
// record against its document's text layer (intake V-1). Every check is
// code; the extractor never marks its own work.
//
// The checks, as the intake requirements define them:
//
//  1. Grounding: every amount, the date, the names and the column are printed
//     on the cited page; each amount beside its label, and the roll-forward's
//     amounts in one column.
//  2. Roll-forward: beginning plus the signed movements equals the ending,
//     exactly on a cents statement, within max(1, 0.5 x lines summed) on a
//     whole-unit one (a nonzero difference inside that is a warning).
//  3. Internal consistency: every printed copy of the ending balance agrees.
//  4. Investor, not fund: the value is not from a total-fund, general
//     partner or all-partners column, and is smaller than the fund total.
//  5. Period: one period end, from a period label, not a print timestamp.
//  6. Currency: stated by code or name; a bare "$" is a warning.
//  7. Partial roll-forward: an ending with no roll-forward is a warning.
//
// Errors block a record; warnings block it until a named person accepts
// that exact warning (Finding.ID). Verify is pure: the same document and
// record always give the same Result.
package verify

import (
	"fmt"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/document"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
)

// Check numbers, as the intake requirements number them.
const (
	CheckGrounding = iota + 1
	CheckRollForward
	CheckConsistency
	CheckInvestor
	CheckPeriod
	CheckCurrency
	CheckPartial
	// checkEnd follows the last check; a check added before it raises
	// CheckCount with it (and must update TestCheckNames, which pins the
	// intake requirements' seven checks on purpose).
	checkEnd
)

// CheckCount is the number of checks; a Result holds each once, in order.
const CheckCount = checkEnd - 1

// CheckName returns a check's name. TestCheckNames requires a name for every
// check up to CheckCount.
func CheckName(n int) string {
	switch n {
	case CheckGrounding:
		return "grounding"
	case CheckRollForward:
		return "roll_forward"
	case CheckConsistency:
		return "internal_consistency"
	case CheckInvestor:
		return "investor_not_fund"
	case CheckPeriod:
		return "period"
	case CheckCurrency:
		return "currency"
	case CheckPartial:
		return "partial_roll_forward"
	}
	return fmt.Sprintf("check_%d", n)
}

// Severity is how a finding affects the record.
type Severity string

// The severities.
const (
	// Error blocks the record; it cannot be accepted, only fixed.
	Error Severity = "error"
	// Warning blocks the record until a named person accepts it.
	Warning Severity = "warning"
)

// Status is a check's or a record's outcome.
type Status string

// The statuses.
const (
	Pass    Status = "pass"
	Warn    Status = "warn"
	Fail    Status = "fail"
	Skipped Status = "skipped"
)

// Finding is one reason a check did not pass.
type Finding struct {
	// Code is stable for a kind of finding ("amount_not_printed").
	Code string `json:"code"`
	// Subject names the part of the record it is about ("ending", "line 3").
	Subject  string   `json:"subject,omitempty"`
	Severity Severity `json:"severity"`
	// Message explains it for the extractor and the reviewer.
	Message string `json:"message"`
	Check   int    `json:"check"`
	Page    int    `json:"page,omitempty"`
}

// ID identifies the finding for a warning's acceptance: the check, the code
// and the subject. It does not include the message, so a reworded message
// does not void an acceptance; a changed record reruns verification and
// voids every acceptance anyway (intake R-4).
func (f *Finding) ID() string {
	id := fmt.Sprintf("%d/%s", f.Check, f.Code)
	if f.Subject != "" {
		id += "/" + f.Subject
	}
	return id
}

// CheckResult is one check's outcome.
type CheckResult struct {
	Name     string    `json:"name"`
	Status   Status    `json:"status"`
	Findings []Finding `json:"findings,omitempty"`
	Check    int       `json:"check"`
}

// Result is the outcome of all seven checks.
type Result struct {
	// Verdict is fail with any error, warn with any warning, else pass.
	Verdict Status        `json:"verdict"`
	Checks  []CheckResult `json:"checks"`
}

// Findings returns every finding of the given severity, in check order.
func (r *Result) Findings(sev Severity) []Finding {
	var out []Finding
	for _, c := range r.Checks {
		for _, f := range c.Findings {
			if f.Severity == sev { //nolint:gocritic // rangeValCopy: findings are few
				out = append(out, f)
			}
		}
	}
	return out
}

// Failed returns the numbers of the checks that found an error.
func (r *Result) Failed() []int {
	var out []int
	for _, c := range r.Checks {
		if c.Status == Fail {
			out = append(out, c.Check)
		}
	}
	return out
}

// Verify runs the seven checks on rec against doc. rec must have passed
// statement.Record.Validate; a record that has not is reported as a
// grounding error rather than checked.
func Verify(doc *document.Document, rec *statement.Record) Result {
	v := newVerifier(doc, rec)
	if err := rec.Validate(); err != nil {
		v.add(CheckGrounding, Error, "invalid_record", "", 0, "%v", err)
		for n := CheckRollForward; n <= CheckCount; n++ {
			v.skipped[n] = true
		}
		return v.result()
	}
	if !v.textReadable() {
		v.checkRollForward()
		v.checkPartial()
		for _, n := range []int{CheckConsistency, CheckInvestor, CheckPeriod, CheckCurrency} {
			v.skipped[n] = true
		}
		return v.result()
	}
	v.checkGrounding()
	v.findSection()
	v.checkRollForward()
	v.checkConsistency()
	v.checkInvestor()
	v.checkPeriod()
	v.checkCurrency()
	v.checkPartial()
	return v.result()
}

type verifier struct {
	doc      *document.Document
	rec      *statement.Record
	pages    map[int]*pageText
	findings map[int][]Finding
	skipped  map[int]bool
	// The ending balance as printed, once grounded.
	ending *labelHit
	endTok *token
	// The section header over the ending's line, when the table has one.
	section *sectionHeader
	// fundTable is a one-phrase fund or general-partner heading over the
	// table, when there is one and no partner-group row.
	fundTable string
	// The column the roll-forward's amounts were read from, once check 1
	// has found it: their shared right edge.
	edge    int
	hasEdge bool
	// beginningLine is the beginning balance's line on the ending's page,
	// or -1.
	beginningLine int
}

func newVerifier(doc *document.Document, rec *statement.Record) *verifier {
	return &verifier{
		doc:           doc,
		rec:           rec,
		pages:         map[int]*pageText{},
		findings:      map[int][]Finding{},
		skipped:       map[int]bool{},
		beginningLine: -1,
	}
}

func (v *verifier) add(check int, sev Severity, code, subject string, page int, format string, args ...any) {
	v.findings[check] = append(v.findings[check], Finding{
		Check:    check,
		Severity: sev,
		Code:     code,
		Subject:  subject,
		Page:     page,
		Message:  fmt.Sprintf(format, args...),
	})
}

func (v *verifier) result() Result {
	r := Result{Verdict: Pass, Checks: make([]CheckResult, 0, CheckCount)}
	for n := 1; n <= CheckCount; n++ {
		c := CheckResult{Check: n, Name: CheckName(n), Status: Pass, Findings: v.findings[n]}
		for _, f := range c.Findings {
			switch {
			case f.Severity == Error:
				c.Status = Fail
			case c.Status == Pass:
				c.Status = Warn
			}
		}
		if v.skipped[n] && len(c.Findings) == 0 {
			c.Status = Skipped
		}
		switch {
		case c.Status == Fail:
			r.Verdict = Fail
		case c.Status == Warn && r.Verdict == Pass:
			r.Verdict = Warn
		}
		r.Checks = append(r.Checks, c)
	}
	return r
}

// textReadable reports whether the document's text was read; when it was
// not, it records why as a grounding error.
func (v *verifier) textReadable() bool {
	switch {
	case v.doc.TextLayer == document.TextNotRead:
		v.add(CheckGrounding, Error, "text_not_read", "", 0,
			"the PDF is copy-restricted and no operator accepted reading its text; it goes to manual entry")
		return false
	case v.doc.TextLayer == document.TextNone || len(v.doc.Pages) == 0:
		v.add(CheckGrounding, Error, "no_text_layer", "", 0,
			"the PDF has no usable text layer (a scan?); it goes to manual entry, never to OCR")
		return false
	}
	return true
}

// page returns page n's text, or records why it cannot be used.
func (v *verifier) page(check int, subject string, n int) *pageText {
	if p, ok := v.pages[n]; ok {
		return p
	}
	pg, ok := v.doc.Page(n)
	switch {
	case !ok:
		v.add(check, Error, "no_such_page", subject, n, "%s cites page %d; the document has %d", subject, n, v.doc.PageCount)
		return nil
	case !pg.Usable:
		v.add(check, Error, "page_unusable", subject, n, "%s cites page %d, which has no usable text layer", subject, n)
		return nil
	}
	p := readPage(pg.Text)
	v.pages[n] = p
	return p
}
