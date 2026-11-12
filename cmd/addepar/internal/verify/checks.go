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
	v.groundText("column", r.Column.Label, r.Column.Page)
	if r.Column.Section != "" {
		v.groundText("column section", r.Column.Section, r.Column.Page)
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
		if len(c.hits) == 0 || c.amount.Page != v.rec.Ending.Page {
			continue // ungrounded (already reported) or on another page
		}
		if nearest(c.hits, best) == nil {
			v.add(CheckGrounding, Error, "other_column", c.subject, c.amount.Page,
				"%s amount %s is printed beside %q but not in the column the other amounts come from", c.subject, c.amount.Amount, c.amount.Label)
		}
	}
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
		_, header := v.headerAt(s.tok.end, len(columnsOf(s.hit.run)))
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

// headerAt looks at the header lines that print the record's column label
// as a whole header (a phrase equal to it, not merely containing it), and
// reports whether, on one of them, the header nearest edge is that label.
// When it is on none, nearestText is the header that was nearest instead
// ("" when no line prints the label as a header). Column headers come in
// rows, so a line with one phrase (a title) counts only when the table has
// a single column (cols), where no other column could have been read.
func (v *verifier) headerAt(edge, cols int) (nearestText string, ok bool) {
	c := v.rec.Column
	p := v.pages[c.Page]
	if p == nil || c.Page != v.rec.Ending.Page {
		return "", false
	}
	want := normWords(c.Label)
	for li := range p.lines {
		l := &p.lines[li]
		if l.hasCells() {
			continue
		}
		phs := l.phrases()
		if (len(phs) < 2 && cols > 1) || !slices.ContainsFunc(phs, func(ph phrase) bool { return slices.Equal(ph.norm, want) }) {
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
		nearestText = phs[best].text
	}
	return nearestText, false
}

// checkColumnHeader confirms that the column the amounts sit in is the one
// the record names: on a header line printing the column label, the header
// nearest the amounts' right edge must be that label. A label printed
// nowhere as a header is reported by grounding when it is not printed at
// all; when it is printed only inside other text, no header line confirms
// the column and that is reported here.
func (v *verifier) checkColumnHeader() {
	c := v.rec.Column
	if !v.hasEdge || v.ending == nil || c.Page != v.rec.Ending.Page || v.pages[c.Page] == nil {
		return
	}
	nearestText, ok := v.headerAt(v.edge, len(columnsOf(v.ending.run)))
	switch {
	case ok:
	case nearestText != "":
		v.add(CheckGrounding, Error, "column_mismatch", "column", c.Page,
			"the amounts sit under %q, not under the column the record names, %q", nearestText, c.Label)
	case v.pages[c.Page].containsText(c.Label):
		v.add(CheckGrounding, Error, "column_not_header", "column", c.Page,
			"%q is not printed as a column header on page %d; name the header exactly as printed", c.Label, c.Page)
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

// findSection looks above the ending's line for a section header that names
// the fund, the general partner or the record's own section, and maps the
// ending line's columns onto its phrases: in order when the columns divide
// evenly among them (three under "Limited Partner", three under "Total
// Fund"), else each to the nearest phrase.
func (v *verifier) findSection() {
	if v.ending == nil {
		return
	}
	p := v.pages[v.rec.Ending.Page]
	cols := columnsOf(v.ending.run)
	secNorm := normWords(v.rec.Column.Section)
	for li := v.ending.line.index - 1; li >= 0; li-- {
		l := &p.lines[li]
		if l.hasCells() {
			continue
		}
		phs := l.phrases()
		named := fundHeader.MatchString(l.text) || (len(secNorm) > 0 && findPhrase(flatten(phs), secNorm, 0) >= 0)
		if len(phs) < 2 || !named {
			continue
		}
		v.section = &sectionHeader{phrases: phs, groupOf: groupColumns(phs, cols)}
		return
	}
}

func flatten(phs []phrase) []string {
	var out []string
	for _, ph := range phs {
		out = append(out, ph.norm...)
	}
	return out
}

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
	tol := decimal.FromInt(int64(n)).Half()
	if tol.Cmp(decimal.FromInt(1)) < 0 {
		tol = decimal.FromInt(1)
	}
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
	for i, rep := range r.Repeats {
		if !rep.Amount.Equal(r.Ending.Amount) {
			v.add(CheckConsistency, Error, "repeat_disagrees", fmt.Sprintf("repeat %d", i+1), rep.Page,
				"%q prints %s, but the ending is %s", rep.Label, rep.Amount, r.Ending.Amount)
		}
	}
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
			if !ok || (!isRange && !strongPeriodWord.MatchString(line)) {
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

// --- Check 6: currency -------------------------------------------------------

func (v *verifier) checkCurrency() {
	r := v.rec
	names := currencyNames()
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
