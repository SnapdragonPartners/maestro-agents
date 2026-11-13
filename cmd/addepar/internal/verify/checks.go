package verify

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/decimal"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
)

// --- Check 1: grounding ---------------------------------------------------

// columnAmount is a roll-forward amount that must sit in the record's column.
type columnAmount struct {
	amount  *statement.Amount
	chosen  *candidate // the hit in the chosen column, once found
	subject string
	hits    []candidate
}

type candidate struct {
	hit *labelHit
	tok token
}

func (v *verifier) checkGrounding() {
	r := v.rec
	v.groundText("investor", r.Investor.Text, r.Investor.Page)
	v.groundText("fund", r.Fund.Text, r.Fund.Page)
	v.groundText("period", r.Period.Label, r.Period.Page)
	if r.Currency.Evidence != "" {
		v.groundText("currency", r.Currency.Evidence, r.Currency.Page)
	}
	v.groundHeader("column", r.Column.Label, r.Column.Page)
	if r.Column.Section != "" {
		v.groundHeader("column section", r.Column.Section, r.Column.Page)
	}

	col := make([]*columnAmount, 0, len(r.Lines)+2)
	if r.Beginning != nil {
		col = append(col, &columnAmount{amount: r.Beginning, subject: "beginning"})
	}
	for i := range r.Lines {
		col = append(col, &columnAmount{amount: &r.Lines[i].Amount, subject: fmt.Sprintf("line %d", i+1)})
	}
	ending := &columnAmount{amount: &r.Ending, subject: "ending"}
	col = append(col, ending)
	for _, c := range col {
		c.hits = v.groundAmount(c.subject, c.amount)
	}
	for i := range r.Repeats {
		v.groundAmount(fmt.Sprintf("repeat %d", i+1), &r.Repeats[i])
	}
	if r.FundTotal != nil {
		v.groundAmount("fund total", r.FundTotal)
	}
	v.chooseColumn(col, ending)
	v.checkColumnHeader()
	v.checkClosing(col, ending)
}

func (v *verifier) groundHeader(subject, text string, page int) {
	p := v.page(CheckGrounding, subject, page)
	if p != nil && !p.containsHeader(text) {
		v.add(CheckGrounding, Error, "not_printed", subject, page, "%s %q is not printed on page %d", subject, text, page)
	}
}

func (v *verifier) groundText(subject, text string, page int) {
	p := v.page(CheckGrounding, subject, page)
	if p == nil {
		return
	}
	if !p.containsText(text) {
		v.add(CheckGrounding, Error, "not_printed", subject, page, "%s %q is not printed on page %d", subject, text, page)
	}
}

// groundAmount finds the amount printed beside its label on its page and
// returns every place it is. A zero amount matches a printed dash only when
// the record states that a dash means zero.
func (v *verifier) groundAmount(subject string, a *statement.Amount) []candidate {
	p := v.page(CheckGrounding, subject, a.Page)
	if p == nil {
		return nil
	}
	hits := p.findLabel(a.Label)
	if len(hits) == 0 {
		v.add(CheckGrounding, Error, "label_not_printed", subject, a.Page, "%s label %q is not printed on page %d", subject, a.Label, a.Page)
		return nil
	}
	var out []candidate
	sawDash := false
	for i := range hits {
		h := &hits[i]
		for _, t := range h.run {
			switch {
			case t.kind == tokAmount && t.value.Equal(a.Amount):
				out = append(out, candidate{hit: h, tok: t})
			case t.kind == tokDash && a.Amount.Sign() == 0:
				if v.rec.DashZero != nil {
					out = append(out, candidate{hit: h, tok: t})
				} else {
					sawDash = true
				}
			}
		}
	}
	if len(out) == 0 {
		if sawDash {
			v.add(CheckGrounding, Error, "dash_not_zero", subject, a.Page,
				"%s is 0 but the statement prints a dash beside %q; a dash is zero only when the record states the table's convention (dash_zero)", subject, a.Label)
		} else {
			v.add(CheckGrounding, Error, "amount_not_printed", subject, a.Page,
				"%s amount %s is not printed beside %q on page %d (printed there: %s)", subject, a.Amount, a.Label, a.Page, runValues(hits))
		}
	}
	return out
}

func runValues(hits []labelHit) string {
	var vals []string
	for _, h := range hits {
		for _, t := range h.run {
			switch t.kind {
			case tokAmount:
				vals = append(vals, t.value.String())
			case tokDash:
				vals = append(vals, "-")
			}
		}
	}
	if len(vals) == 0 {
		return "no amounts"
	}
	return strings.Join(vals, ", ")
}

// chooseColumn finds the right edge the roll-forward's amounts share: the
// edge, among the ending's (or else the beginning's) candidates, that the
// most amounts have a candidate at. When several edges tie (QTD and YTD
// print the same figures in a first quarter), the one under the record's
// own column header wins. An amount with no candidate at the edge was read
// from another column.
func (v *verifier) chooseColumn(col []*columnAmount, ending *columnAmount) {
	seeds := ending.hits
	if len(seeds) == 0 && len(col) > 1 {
		seeds = col[0].hits
	}
	best, ok := v.bestEdge(seeds, col)
	if !ok {
		return
	}
	v.edge, v.hasEdge = best, true
	if e := nearest(ending.hits, best); e != nil {
		v.ending, v.endTok = e.hit, &e.tok
	}
	for _, c := range col {
		switch {
		case len(c.hits) == 0:
			// ungrounded, already reported
		case c.amount.Page != v.rec.Ending.Page:
			// A column is a right edge on one page; across pages it cannot
			// be shown to be the same column.
			v.add(CheckGrounding, Error, "column_unverified", c.subject, c.amount.Page,
				"%s is on page %d and the ending on page %d; the column cannot be verified across pages", c.subject, c.amount.Page, v.rec.Ending.Page)
		default:
			c.chosen = nearest(c.hits, best)
			if c.chosen != nil && c.amount == v.rec.Beginning {
				v.beginningLine = c.chosen.hit.line.index
			}
			if c.chosen == nil {
				v.add(CheckGrounding, Error, "other_column", c.subject, c.amount.Page,
					"%s amount %s is printed beside %q but not in the column the other amounts come from", c.subject, c.amount.Amount, c.amount.Label)
			}
		}
	}
}

// checkClosing establishes from the printed table that the ending is the
// period's closing balance, whatever the record submitted:
//
//   - its label is not an opening balance's;
//   - its row is below the beginning and every submitted movement;
//   - every row between the beginning and the ending that prints a cell in
//     the chosen column was submitted (an omitted movement could otherwise
//     make an intermediate subtotal look closing);
//   - no row after it prints a cell in the chosen column before the table
//     ends, at the first line with no cells at all (a heading, a footnote).
func (v *verifier) checkClosing(col []*columnAmount, ending *columnAmount) {
	fail := func(format string, args ...any) {
		v.add(CheckRollForward, Error, "ending_not_closing", "ending", v.rec.Ending.Page, format, args...)
	}
	if beginningLabel.MatchString(foldPunct(v.rec.Ending.Label)) {
		fail("the ending's label %q is an opening balance's", v.rec.Ending.Label)
		return
	}
	if ending.chosen == nil {
		return
	}
	end := ending.chosen.hit.line.index
	submitted := map[int]bool{end: true}
	for _, c := range col {
		if c == ending || c.chosen == nil {
			continue
		}
		submitted[c.chosen.hit.line.index] = true
		if c.chosen.hit.line.index >= end {
			fail("the ending %q is printed at or above %s %q; the closing balance comes after the roll-forward",
				v.rec.Ending.Label, c.subject, c.amount.Label)
			return
		}
	}
	allChosen := true
	for _, c := range col {
		allChosen = allChosen && c.chosen != nil
	}
	// Coverage is judged only when every submitted amount was found in the
	// column; otherwise grounding has already failed the record.
	if v.beginningLine >= 0 && allChosen {
		if row, ok := v.unsubmittedRow(v.beginningLine, end, submitted); ok {
			v.add(CheckRollForward, Error, "row_not_submitted", "", v.rec.Ending.Page,
				"the row %q between the beginning and the ending prints an amount in the column but is not in the record; submit every row (a subtotal as kind \"subtotal\")", row)
			return
		}
	}
	if row, ok := v.rowAfter(end); ok {
		fail("the row %q after the ending prints an amount in the same column; the closing balance is the table's last row", row)
		return
	}
	if row, ok := v.continuesTo(end); ok {
		fail("the ending plus the rows printed after it reaches %q: the roll-forward continues past the ending, so it is not the closing balance", row)
	}
}

// subtotalLabel matches a row that may restate rows above it.
var subtotalLabel = regexp.MustCompile(`(?i)\b(total|subtotal|net)\b`)

// unsubmittedRow returns the first row strictly between from and to that
// prints a cell in the chosen column and is not submitted.
func (v *verifier) unsubmittedRow(from, to int, submitted map[int]bool) (string, bool) {
	p := v.pages[v.rec.Ending.Page]
	for li := from + 1; li < to; li++ {
		if l := &p.lines[li]; l.cellAt(v.edge) && !submitted[li] {
			return strings.TrimSpace(l.text), true
		}
	}
	return "", false
}

// rowAfter returns the first row after line end, before the table ends at a
// line with no cells, that prints a cell in the chosen column.
func (v *verifier) rowAfter(end int) (string, bool) {
	p := v.pages[v.rec.Ending.Page]
	for li := end + 1; li < len(p.lines); li++ {
		l := &p.lines[li]
		switch {
		case strings.TrimSpace(l.text) == "":
		case l.cellAt(v.edge):
			return strings.TrimSpace(l.text), true
		case !l.hasCells():
			return "", false
		}
	}
	return "", false
}

// continuesTo looks past headings for the roll-forward continuing below
// the ending: a later row in the chosen column whose amount is the ending
// plus the rows printed between them, read as movements (all of them, or
// all but those labeled as totals), with at least one nonzero row between.
// On a whole-unit statement the comparison allows check 2's rounding,
// max(1, 0.5 x the amounts summed). A copy of the ending with nothing
// between is a repeat, for check 3.
func (v *verifier) continuesTo(end int) (string, bool) {
	p := v.pages[v.rec.Ending.Page]
	all, noTotals := v.rec.Ending.Amount, v.rec.Ending.Amount
	nAll, nNoTotals := 1, 1
	moved := false
	for li := end + 1; li < len(p.lines); li++ {
		l := &p.lines[li]
		t, ok := l.amountAt(v.edge)
		if !ok {
			continue
		}
		if moved && (v.within(t.value, all, nAll) || v.within(t.value, noTotals, nNoTotals)) {
			return strings.TrimSpace(l.text), true
		}
		all, nAll = all.Add(t.value), nAll+1
		if !subtotalLabel.MatchString(l.text) {
			noTotals, nNoTotals = noTotals.Add(t.value), nNoTotals+1
		}
		moved = moved || t.value.Sign() != 0
	}
	return "", false
}

// within reports whether got equals want, exactly on a cents statement and
// within check 2's rounding tolerance for n addends on a whole-unit one.
func (v *verifier) within(got, want decimal.Decimal, n int) bool {
	if v.rec.Precision == statement.Cents {
		return got.Equal(want)
	}
	return got.Sub(want).Abs().Cmp(roundingTolerance(n)) <= 0
}

// roundingTolerance is max(1, 0.5 x n) for n rounded amounts summed.
func roundingTolerance(n int) decimal.Decimal {
	tol := decimal.FromInt(int64(n)).Half()
	if tol.Cmp(decimal.FromInt(1)) < 0 {
		return decimal.FromInt(1)
	}
	return tol
}

// tableTop returns the line of the table's first submitted row at edge:
// the beginning's, else the given line.
func (v *verifier) tableTop(col []*columnAmount, edge, line int) int {
	for _, c := range col {
		if c.amount == v.rec.Beginning {
			if b := nearest(c.hits, edge); b != nil && b.hit.line.index < line {
				return b.hit.line.index
			}
		}
	}
	return line
}

// bestEdge returns the seed edge the most column amounts have a candidate
// at, preferring, among ties, one under the record's column header.
func (v *verifier) bestEdge(seeds []candidate, col []*columnAmount) (edge int, ok bool) {
	bestN, bestHeader := -1, false
	for _, s := range seeds {
		n := 0
		for _, c := range col {
			if c.amount.Page == v.rec.Ending.Page && nearest(c.hits, s.tok.end) != nil {
				n++
			}
		}
		_, header := v.headerAt(s.tok.end, v.tableTop(col, s.tok.end, s.hit.line.index), len(columnsOf(s.hit.run)))
		if n > bestN || (n == bestN && header && !bestHeader) {
			edge, bestN, bestHeader = s.tok.end, n, header
		}
	}
	return edge, bestN >= 0
}

// nearest returns the candidate whose right edge is within the column
// tolerance of edge and closest to it, or nil.
func nearest(cs []candidate, edge int) *candidate {
	var best *candidate
	for i := range cs {
		d := abs(cs[i].tok.end - edge)
		if d <= columnTolerance && (best == nil || d < abs(best.tok.end-edge)) {
			best = &cs[i]
		}
	}
	return best
}

// headerAt finds the table's column header row, the first header row
// above its first row (top) with no table row between, and reports whether
// the header nearest edge on it is the record's column label, matched as a
// whole header (a phrase equal to it). A header row has two or more
// phrases and no cells; a one-phrase line counts only when the table has a
// single column (cols). Otherwise nearestText is the header that is
// nearest instead, or "" when the table has no header row: a heading
// elsewhere on the page proves nothing about this table.
func (v *verifier) headerAt(edge, top, cols int) (nearestText string, ok bool) {
	c := v.rec.Column
	p := v.pages[c.Page]
	if p == nil || c.Page != v.rec.Ending.Page {
		return "", false
	}
	want := normWords(c.Label)
	for li := top - 1; li >= 0 && li >= top-headerWindow; li-- {
		l := &p.lines[li]
		if l.hasLooseCells() {
			return "", false // a row of another table: this one has no header row
		}
		phs := l.phrases()
		if len(phs) == 0 || (len(phs) < 2 && cols > 1) {
			continue
		}
		best := 0
		for i, ph := range phs {
			if ph.distance(edge) < phs[best].distance(edge) {
				best = i
			}
		}
		if slices.Equal(phs[best].norm, want) {
			return "", true
		}
		return phs[best].text, false
	}
	return "", false
}

// headerWindow is how many lines above the table's first row the column
// header row may be (titles and blank lines between).
const headerWindow = 6

// checkColumnHeader confirms that the column the amounts sit in is the one
// the record names: on a header line printing the column label, the header
// nearest the amounts' right edge must be that label. A label printed
// nowhere as a header is reported by grounding when it is not printed at
// all; when it is printed only inside other text, no header line confirms
// the column and that is reported here.
func (v *verifier) checkColumnHeader() {
	c := v.rec.Column
	if c.Page != v.rec.Ending.Page {
		v.add(CheckGrounding, Error, "column_unverified", "column", c.Page,
			"the column header is cited on page %d and the ending on page %d; a column is verified only on the amounts' own page", c.Page, v.rec.Ending.Page)
		return
	}
	if !v.hasEdge || v.ending == nil || v.pages[c.Page] == nil {
		return // the ending was not grounded: already an error
	}
	top := v.ending.line.index
	if v.beginningLine >= 0 {
		top = v.beginningLine
	}
	nearestText, ok := v.headerAt(v.edge, top, len(columnsOf(v.ending.run)))
	switch {
	case ok:
	case nearestText != "":
		v.add(CheckGrounding, Error, "column_mismatch", "column", c.Page,
			"the amounts sit under %q, not under the column the record names, %q", nearestText, c.Label)
	default:
		v.add(CheckGrounding, Error, "column_not_header", "column", c.Page,
			"no header row directly above the table names %q; name the column header exactly as printed", c.Label)
	}
}

// --- Section headers (check 3 and 4) ---------------------------------------

// fundHeader matches a column or section header for something other than
// the investor: the fund total, the general partner, all partners.
var fundHeader = regexp.MustCompile(`(?i)\b(total fund|fund total|total partnership|partnership total|total partners|all partners|general partners?|including gp|incl\.? gp|gp)\b`)

// totalHeader matches a header for a total over every partner: a value
// under it is larger than any one investor's.
var totalHeader = regexp.MustCompile(`(?i)\b(total fund|fund total|total partnership|partnership total|total partners|all partners|including gp)\b`)

// sectionHeader is a header line over groups of columns, such as
// "Limited Partner      Total Fund (Including GP)".
type sectionHeader struct {
	groupOf func(edge int) int // the phrase a column's edge falls under, or -1
	phrases []phrase
}

// columnsOf returns the distinct right edges of a run's cells.
func columnsOf(run []token) []int {
	var cols []int
	for _, t := range run {
		if t.kind == tokPct {
			continue
		}
		dup := false
		for _, c := range cols {
			if abs(c-t.end) <= columnTolerance {
				dup = true
			}
		}
		if !dup {
			cols = append(cols, t.end)
		}
	}
	return cols
}

// findSection looks for a partner-group header over the table, from the
// page alone: a line without cells, among the few lines above the table's
// first roll-forward row, that names the fund total, the general partner or
// all partners. With two or more phrases ("Limited Partner   Total Fund
// (Including GP)") it maps the ending line's columns onto its phrases: in
// order when they divide evenly among them, else each to the nearest
// phrase. Any other fund heading above the table (one phrase, or beyond the
// window) leaves its ownership unverified (fundTable), which fails check 4.
// The record's claimed section plays no part.
func (v *verifier) findSection() {
	if v.ending == nil {
		return
	}
	p := v.pages[v.rec.Ending.Page]
	top := v.ending.line.index
	if v.beginningLine >= 0 && v.beginningLine < top {
		top = v.beginningLine
	}
	cols := columnsOf(v.ending.run)
	seen := 0
	for li := top - 1; li >= 0; li-- {
		l := &p.lines[li]
		if strings.TrimSpace(l.text) == "" {
			continue
		}
		seen++
		if l.hasLooseCells() || !fundHeader.MatchString(l.text) {
			continue
		}
		if phs := l.phrases(); len(phs) >= 2 && seen <= sectionWindow && v.section == nil && v.fundTable == "" && partnerGroups(phs) {
			v.section = &sectionHeader{phrases: phs, groupOf: groupColumns(phs, cols)}
			continue
		}
		// Any other fund or general-partner heading above the table (one
		// phrase, beyond the window, nearer than the group row or above
		// it) is evidence the group row does not resolve: the table's
		// ownership is unverified.
		if v.fundTable == "" {
			v.fundTable = strings.TrimSpace(l.text)
		}
	}
}

// partnerHeader matches a phrase naming a partner group: the investor's or
// another's.
var partnerHeader = regexp.MustCompile(`(?i)\b(limited partners?|general partners?|partners|investors?|members?|lp|gp|shareholders?|unitholders?|total fund|fund total|total partnership|all partners|your (share|account|capital))\b`)

// partnerGroups reports whether every phrase of a line names a partner
// group, so that the line splits the table's columns among owners. A fund
// heading beside anything else ("Total Fund (Including GP)    USD") does
// not, and stays a table-wide heading.
func partnerGroups(phs []phrase) bool {
	for _, ph := range phs {
		if !partnerHeader.MatchString(ph.text) {
			return false
		}
	}
	return true
}

// sectionWindow is how many printed lines above the table's first row are
// searched for a partner-group header: the column header row, the group
// row and a title or two.
const sectionWindow = 4

func groupColumns(phs []phrase, cols []int) func(edge int) int {
	if len(cols) > 0 && len(cols)%len(phs) == 0 {
		per := len(cols) / len(phs)
		return func(edge int) int {
			for i, c := range cols {
				if abs(c-edge) <= columnTolerance {
					return i / per
				}
			}
			return -1
		}
	}
	return func(edge int) int {
		best := -1
		for i, ph := range phs {
			if best < 0 || ph.distance(edge) < phs[best].distance(edge) {
				best = i
			}
		}
		return best
	}
}

// --- Check 2: roll-forward -------------------------------------------------

// beginningLabel matches a printed beginning balance.
var beginningLabel = regexp.MustCompile(`(?i)\b(beginning balance|opening balance|beginning of (the )?(period|quarter|year)|balance,? beginning|beginning capital|opening capital)\b`)

func (v *verifier) checkRollForward() {
	r := v.rec
	if r.Beginning == nil {
		v.rollForwardOmitted()
		return
	}
	v.checkPrecision()
	sum := r.Beginning.Amount
	n := 1
	for _, l := range r.Lines {
		if l.Kind == statement.Movement {
			sum = sum.Add(l.Amount.Amount)
			n++
		}
	}
	diff := r.Ending.Amount.Sub(sum)
	if diff.Sign() == 0 {
		return
	}
	if r.Precision == statement.Cents {
		v.add(CheckRollForward, Error, "does_not_tie", "", r.Ending.Page,
			"beginning %s plus the movements is %s, not the ending %s (off by %s; a cents statement must tie exactly)",
			r.Beginning.Amount, sum, r.Ending.Amount, diff)
		return
	}
	tol := roundingTolerance(n)
	if diff.Abs().Cmp(tol) > 0 {
		v.add(CheckRollForward, Error, "does_not_tie", "", r.Ending.Page,
			"beginning %s plus the movements is %s, not the ending %s (off by %s; rounding allows %s over %d lines)",
			r.Beginning.Amount, sum, r.Ending.Amount, diff, tol, n)
		return
	}
	v.add(CheckRollForward, Warning, "rounding_difference", "", r.Ending.Page,
		"beginning plus the movements is off from the ending by %s, within the rounding allowed (%s over %d lines)", diff, tol, n)
}

// rollForwardOmitted reports a record with no beginning balance when the
// ending's page prints one: the roll-forward exists and must be submitted.
func (v *verifier) rollForwardOmitted() {
	p := v.pages[v.rec.Ending.Page]
	if p == nil {
		return
	}
	for _, l := range p.lines {
		if beginningLabel.MatchString(foldPunct(l.text)) {
			v.add(CheckRollForward, Error, "roll_forward_omitted", "", v.rec.Ending.Page,
				"the record has no roll-forward, but page %d prints a beginning balance; submit the beginning balance and every movement", v.rec.Ending.Page)
			return
		}
	}
}

// checkPrecision confirms the claimed precision against how the ending is
// printed and against the amounts' own digits: a whole-unit claim on a
// cents statement would loosen the roll-forward's tolerance.
func (v *verifier) checkPrecision() {
	r := v.rec
	if v.endTok != nil && v.endTok.cents != (r.Precision == statement.Cents) {
		v.add(CheckRollForward, Error, "precision_mismatch", "", r.Ending.Page,
			"the record says %q but the ending is printed as %q", r.Precision, v.endTok.raw)
	}
	check := func(subject string, d decimal.Decimal) {
		whole := d.IsInteger()
		cents := d.Sub(roundCents(d)).Sign() == 0
		if (r.Precision == statement.Whole && !whole) || (r.Precision == statement.Cents && !cents) {
			v.add(CheckRollForward, Error, "precision_mismatch", subject, 0,
				"%s amount %s does not fit the record's precision %q", subject, d, r.Precision)
		}
	}
	check("beginning", r.Beginning.Amount)
	for i, l := range r.Lines {
		check(fmt.Sprintf("line %d", i+1), l.Amount.Amount)
	}
	check("ending", r.Ending.Amount)
}

// roundCents truncates d to two fractional digits.
func roundCents(d decimal.Decimal) decimal.Decimal {
	s := d.String()
	whole, frac, ok := strings.Cut(s, ".")
	if !ok || len(frac) <= 2 {
		return d
	}
	return decimal.MustParse(whole + "." + frac[:2])
}

// --- Check 3: internal consistency -----------------------------------------

func (v *verifier) checkConsistency() {
	r := v.rec
	if v.ending != nil {
		group := -1
		if v.section != nil {
			group = v.section.groupOf(v.endTok.end)
		}
		for _, t := range v.ending.run {
			if t.kind != tokAmount {
				continue
			}
			if v.section != nil && v.section.groupOf(t.end) != group {
				continue // another section's balance, such as the fund total
			}
			if !t.value.Equal(r.Ending.Amount) {
				v.add(CheckConsistency, Error, "ending_disagrees", "ending", r.Ending.Page,
					"the ending line %q prints %s beside the ending %s; every copy of the ending balance must agree",
					r.Ending.Label, t.value, r.Ending.Amount)
			}
		}
	}
	v.discoverRepeats()
	for i, rep := range r.Repeats {
		if !rep.Amount.Equal(r.Ending.Amount) {
			v.add(CheckConsistency, Error, "repeat_disagrees", fmt.Sprintf("repeat %d", i+1), rep.Page,
				"%q prints %s, but the ending is %s", rep.Label, rep.Amount, r.Ending.Amount)
		}
	}
}

// discoverRepeats finds, without relying on the record's Repeats, every
// other line in the document that prints the ending's label with amounts,
// and requires every investor balance on it to agree with the ending. On a
// page the record cites, or one printing the investor's name, a
// disagreement is an error. On any other page it may be another investor's
// statement in the same PDF (E-4), so it is a warning a person must accept,
// never silence.
func (v *verifier) discoverRepeats() {
	r := v.rec
	cited := v.citedPages()
	for _, pg := range v.doc.Pages {
		if !pg.Usable {
			continue
		}
		p := v.pages[pg.Number]
		if p == nil {
			p = readPage(pg.Text)
			v.pages[pg.Number] = p
		}
		mine := slices.Contains(cited, pg.Number) || p.containsText(r.Investor.Text)
		if t, ok := v.disagreeingCopy(p, pg.Number); ok {
			sev, code := Error, "ending_disagrees"
			if !mine {
				sev, code = Warning, "other_page_ending"
			}
			v.add(CheckConsistency, sev, code, fmt.Sprintf("page %d", pg.Number), pg.Number,
				"page %d prints %q as %s, but the ending is %s; every copy of the ending balance must agree (a page of another statement must be confirmed as such)",
				pg.Number, r.Ending.Label, t.value, r.Ending.Amount)
		}
	}
}

// disagreeingCopy returns the first amount printed beside the ending's
// label on page n, other than on the ending's own line and other than
// another partner group's, that differs from the ending.
func (v *verifier) disagreeingCopy(p *pageText, n int) (token, bool) {
	r := v.rec
	for _, h := range p.findLabel(r.Ending.Label) {
		if v.ending != nil && h.line == v.ending.line && h.at == v.ending.at {
			continue // the selected occurrence; check 3 checks its run
		}
		for _, t := range h.run {
			if t.kind != tokAmount {
				continue
			}
			if n == r.Ending.Page && v.section != nil && v.endTok != nil && v.section.groupOf(t.end) != v.section.groupOf(v.endTok.end) {
				continue
			}
			if !t.value.Equal(r.Ending.Amount) {
				return t, true
			}
		}
	}
	return token{}, false
}

// citedPages lists the pages the record cites, in order.
func (v *verifier) citedPages() []int {
	r := v.rec
	pages := []int{r.Investor.Page, r.Fund.Page, r.Period.Page, r.Column.Page, r.Ending.Page}
	if r.Beginning != nil {
		pages = append(pages, r.Beginning.Page)
	}
	for _, l := range r.Lines {
		pages = append(pages, l.Page)
	}
	for _, a := range r.Repeats {
		pages = append(pages, a.Page)
	}
	slices.Sort(pages)
	return slices.Compact(pages)
}

// --- Check 4: investor, not fund -------------------------------------------

func (v *verifier) checkInvestor() {
	r := v.rec
	for _, claimed := range []struct{ what, text string }{{"column", r.Column.Label}, {"column section", r.Column.Section}} {
		if fundHeader.MatchString(claimed.text) {
			v.add(CheckInvestor, Error, "fund_column", claimed.what, r.Column.Page,
				"the record's %s %q is a fund, general partner or all-partners column, not the investor's", claimed.what, claimed.text)
		}
	}
	if v.section != nil && v.endTok != nil {
		if g := v.section.groupOf(v.endTok.end); g >= 0 && fundHeader.MatchString(v.section.phrases[g].text) {
			v.add(CheckInvestor, Error, "fund_column", "ending", r.Ending.Page,
				"the ending %s is printed under %q, not under the investor", r.Ending.Amount, v.section.phrases[g].text)
		}
	}
	if v.fundTable != "" {
		v.add(CheckInvestor, Error, "fund_table", "ending", r.Ending.Page,
			"%q is printed above the table and the partner-group row, if any, does not resolve it: the balances may be the fund's or a general partner's, not the investor's", v.fundTable)
	}
	v.checkLargest()
	if r.FundTotal != nil && r.Ending.Amount.Abs().Cmp(r.FundTotal.Amount.Abs()) >= 0 {
		v.add(CheckInvestor, Error, "not_below_fund_total", "ending", r.Ending.Page,
			"the ending %s is not smaller than the fund total %s", r.Ending.Amount, r.FundTotal.Amount)
	}
}

// checkLargest applies "smaller than the fund total when one is printed"
// to the ending's own line: when a header above names a total over every
// partner and the line prints balances that differ, the investor's is not
// the largest of them.
func (v *verifier) checkLargest() {
	if v.ending == nil || v.endTok == nil {
		return
	}
	p := v.pages[v.rec.Ending.Page]
	total := false
	for li := v.ending.line.index - 1; li >= 0 && !total; li-- {
		l := &p.lines[li]
		total = !l.hasCells() && totalHeader.MatchString(l.text)
	}
	if !total {
		return
	}
	mine := v.endTok.value.Abs()
	larger, other := false, false
	for _, t := range v.ending.run {
		if t.kind != tokAmount || t.value.Equal(v.endTok.value) {
			continue
		}
		other = true
		if t.value.Abs().Cmp(mine) > 0 {
			larger = true
		}
	}
	if other && !larger {
		v.add(CheckInvestor, Error, "largest_balance", "ending", v.rec.Ending.Page,
			"the ending %s is the largest balance on its line under a fund-total header; an investor's balance is smaller than the fund's", v.rec.Ending.Amount)
	}
}

// --- Check 5: period ---------------------------------------------------------

func (v *verifier) checkPeriod() {
	r := v.rec
	want, err := r.Period.EndDate()
	if err != nil {
		v.add(CheckPeriod, Error, "bad_date", "period", r.Period.Page, "%v", err)
		return
	}
	end, isRange, ok := periodEnd(r.Period.Label)
	switch {
	case !ok:
		v.add(CheckPeriod, Error, "no_single_date", "period", r.Period.Page,
			"the period label %q does not state one date or one date range", r.Period.Label)
		return
	case !end.Equal(want):
		v.add(CheckPeriod, Error, "date_mismatch", "period", r.Period.Page,
			"the period label %q states %s, not the record's %s", r.Period.Label, end.Format("2006-01-02"), r.Period.End)
	}
	lineText := v.labelLine(r.Period.Label, r.Period.Page)
	switch {
	case isTimestamp(r.Period.Label) || isTimestamp(lineText):
		v.add(CheckPeriod, Error, "timestamp", "period", r.Period.Page,
			"%q is a print or generation timestamp, not a period label", strings.TrimSpace(lineText))
	case !isRange && !periodWord.MatchString(r.Period.Label) && !isBareDate(lineText):
		v.add(CheckPeriod, Error, "not_period_label", "period", r.Period.Page,
			"%q is not a period label (such as \"For the period ending\", \"Period Ended\" or a date range)", r.Period.Label)
	}
	v.otherPeriods(want)
}

// labelLine returns the text of the first line on page printing label, or
// the label itself when it runs across lines.
func (v *verifier) labelLine(label string, page int) string {
	p := v.pages[page]
	if p == nil {
		return label
	}
	want := normWords(label)
	for _, l := range p.lines {
		norms, _ := l.lineNorms()
		if findPhrase(norms, want, 0) >= 0 {
			return l.text
		}
	}
	return label
}

// otherPeriods reports a different period end printed under a period
// phrase or as a date range anywhere in the document: the statement must
// identify exactly one.
func (v *verifier) otherPeriods(want time.Time) {
	for _, pg := range v.doc.Pages {
		if !pg.Usable {
			continue
		}
		for _, line := range strings.Split(pg.Text, "\n") {
			line = foldPunct(line)
			if isTimestamp(line) {
				continue
			}
			end, isRange, ok := periodEnd(line)
			strong := strongPeriodWord.MatchString(line)
			if !ok && strong {
				// Several dates on a period line: every one must be this
				// period's end, or the statement names two periods.
				if d, bad := otherDate(line, want); bad {
					end, ok = d, true
				}
			}
			if !ok || (!isRange && !strong) {
				continue
			}
			if !want.Equal(end) {
				v.add(CheckPeriod, Error, "conflicting_period", "period", pg.Number,
					"page %d also prints a period ending %s: %q", pg.Number, end.Format("2006-01-02"), strings.TrimSpace(line))
				return
			}
		}
	}
}

// otherDate returns the first date on line that is not want, if any.
func otherDate(line string, want time.Time) (time.Time, bool) {
	for _, d := range findDates(line) {
		if !d.date.Equal(want) {
			return d.date, true
		}
	}
	return time.Time{}, false
}

// --- Check 6: currency -------------------------------------------------------

func (v *verifier) checkCurrency() {
	r := v.rec
	names := currencyTable()
	var stated []string
	seen := map[string]bool{}
	for _, pg := range v.doc.Pages {
		for _, c := range currenciesIn(names, foldPunct(pg.Text)) {
			if !seen[c] {
				seen[c] = true
				stated = append(stated, c)
			}
		}
	}
	if r.Currency.Evidence != "" {
		ev := currenciesIn(names, r.Currency.Evidence)
		if len(ev) != 1 || ev[0] != r.Currency.Code {
			v.add(CheckCurrency, Error, "evidence_mismatch", "currency", r.Currency.Page,
				"the currency evidence %q does not state %s", r.Currency.Evidence, r.Currency.Code)
			return
		}
	}
	for _, c := range stated {
		if c != r.Currency.Code {
			v.add(CheckCurrency, Error, "other_currency", "currency", 0,
				"the statement states %s, but the record says %s", c, r.Currency.Code)
			return
		}
	}
	if len(stated) == 0 {
		v.add(CheckCurrency, Warning, "not_stated", "currency", 0,
			"the statement does not state its currency by code or name (a bare \"$\" is not enough); a person must confirm %s", r.Currency.Code)
	}
}

// --- Check 7: partial roll-forward ------------------------------------------

func (v *verifier) checkPartial() {
	if v.rec.Beginning == nil {
		v.add(CheckPartial, Warning, "partial_roll_forward", "", v.rec.Ending.Page,
			"the record has an ending balance but no roll-forward to reconcile it against")
	}
}
