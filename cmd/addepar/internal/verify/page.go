package verify

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/decimal"
)

// A page in layout mode is read as lines of words, each word with the
// character columns it occupies. Layout mode keeps a label on the same line
// as its numbers and keeps a column's numbers right-aligned, so a column is
// a right edge shared, within columnTolerance, by every amount in it.

// columnTolerance is how far apart (in characters) two right edges may be
// and still be one column. Measured on the samples: a column's amounts
// share a right edge within about 3 characters (a closing parenthesis or a
// centered dash shifts it); adjacent columns are about 20 apart.
const columnTolerance = 4

type word struct {
	raw string // as printed
	// norm is the word normalized for matching a label or header, with a
	// glued footnote reference dropped; "" for a word that matches nothing.
	norm string
	// exact is normalized like norm but keeps every digit, for matching a
	// name: "Series1" and "Series2" are different funds.
	exact string
	start int // first rune column
	end   int // one past the last rune column
}

type tokenKind int

const (
	tokOther  tokenKind = iota // letters, or digits that are not an amount (a date)
	tokAmount                  // a number: 1,234 (1,234.56) $5 -7
	tokDash                    // a dash standing for an empty or zero cell
	tokSign                    // a lone currency sign, "$"
	tokJunk                    // no letters or digits ("##"): skipped
	tokPct                     // a percentage: not an amount, part of a run
)

type token struct {
	value decimal.Decimal
	word
	kind  tokenKind
	cents bool // printed with exactly two fractional digits
}

type pageLine struct {
	text  string
	words []word
	toks  []token
	index int // 0-based line number on the page
}

type pageText struct {
	lines []pageLine
	exact []string // every non-empty exact-normalized word on the page, in order
}

func readPage(text string) *pageText {
	p := &pageText{}
	for i, l := range strings.Split(text, "\n") {
		pl := pageLine{text: l, index: i, words: splitWords(l)}
		pl.toks = make([]token, len(pl.words))
		for j, w := range pl.words {
			pl.toks[j] = classify(w)
			if w.exact != "" {
				p.exact = append(p.exact, w.exact)
			}
		}
		p.lines = append(p.lines, pl)
	}
	return p
}

// splitWords splits a line on spaces, keeping each word's rune columns.
func splitWords(line string) []word {
	var out []word
	runes := []rune(line)
	for i := 0; i < len(runes); {
		if unicode.IsSpace(runes[i]) {
			i++
			continue
		}
		j := i
		for j < len(runes) && !unicode.IsSpace(runes[j]) {
			j++
		}
		raw := string(runes[i:j])
		out = append(out, word{raw: raw, norm: normWord(raw, true), exact: normWord(raw, false), start: i, end: j})
		i = j
	}
	return out
}

// foldPunct folds typographic quotes, dashes and the no-break space to
// their ASCII forms.
func foldPunct(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\u2018', '\u2019':
			return '\''
		case '\u201c', '\u201d':
			return '"'
		case '\u2013', '\u2014':
			return '-'
		case '\u00a0':
			return ' '
		}
		return r
	}, s)
}

// normWord folds case and typographic punctuation and drops trailing ':'
// ',' ';'. With footnotes set it also drops a footnote reference glued to
// the end of a word ("Contributions1", "Gain/(Loss)3"), for labels and
// headers; names keep every digit. A standalone number is always kept:
// "Fund 3" stays "fund" "3".
func normWord(raw string, footnotes bool) string {
	w := strings.ToLower(foldPunct(raw))
	w = strings.TrimRight(w, ":,;")
	if i := strings.LastIndexFunc(w, func(r rune) bool { return r < '0' || r > '9' }); footnotes && i >= 0 && i < len(w)-1 {
		prev := rune(w[i])
		if unicode.IsLetter(prev) || prev == ')' {
			w = w[:i+1]
		}
	}
	w = strings.TrimRight(w, ":,;")
	if strings.IndexFunc(w, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) < 0 {
		return ""
	}
	return w
}

// normWords normalizes a label or header into the words matching compares,
// dropping glued footnote references.
func normWords(s string) []string { return splitNorm(s, true) }

// exactWords normalizes a name, keeping every digit.
func exactWords(s string) []string { return splitNorm(s, false) }

func splitNorm(s string, footnotes bool) []string {
	var out []string
	for _, f := range strings.Fields(foldPunct(s)) {
		if n := normWord(f, footnotes); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// amountRE matches a printed amount after its currency sign has been
// removed: an optional '-' or '(' , digits grouped by commas or plain, an
// optional fraction, and the matching ')'.
var amountRE = regexp.MustCompile(`^(-)?(\()?(-)?([0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)(\.[0-9]+)?(\))?$`)

func classify(w word) token {
	t := token{word: w}
	s := foldPunct(w.raw)
	switch {
	case s == "$":
		t.kind = tokSign
	case s == "-" || s == "$-" || s == "-$" || s == "(-)":
		t.kind = tokDash
	case strings.IndexFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) < 0:
		t.kind = tokJunk
	case strings.HasSuffix(s, "%"):
		t.kind = tokPct
	default:
		if v, cents, ok := parseAmount(s); ok {
			t.kind, t.value, t.cents = tokAmount, v, cents
		}
	}
	return t
}

// parseAmount reads a printed amount: a currency sign outside or inside a
// parenthesis ($(1,234) or ($1,234)), a leading '-' or parentheses for a
// negative, grouped or plain digits and an optional fraction.
func parseAmount(s string) (v decimal.Decimal, cents, ok bool) {
	m := amountRE.FindStringSubmatch(strings.Replace(s, "$", "", 1))
	if len(m) == 0 || (m[2] == "") != (m[6] == "") || (m[1] != "" && (m[2] != "" || m[3] != "")) {
		return decimal.Decimal{}, false, false
	}
	v, err := decimal.Parse(strings.ReplaceAll(m[4], ",", "") + m[5])
	if err != nil {
		return decimal.Decimal{}, false, false
	}
	if m[1] != "" || m[2] != "" || m[3] != "" {
		v = v.Neg()
	}
	return v, len(m[5]) == 3, true
}

// findPhrase reports where the word sequence want starts in words, from
// index from on, or -1.
func findPhrase(words, want []string, from int) int {
	if len(want) == 0 {
		return -1
	}
outer:
	for i := from; i+len(want) <= len(words); i++ {
		for j, w := range want {
			if words[i+j] != w {
				continue outer
			}
		}
		return i
	}
	return -1
}

// lineNorms returns a line's normalized words and, for each, its index in
// the line's words.
func (l *pageLine) lineNorms() (norms []string, idx []int) {
	for i, w := range l.words {
		if w.norm != "" {
			norms = append(norms, w.norm)
			idx = append(idx, i)
		}
	}
	return norms, idx
}

// containsText reports whether the page prints the name or text s, compared
// word by word, every digit kept; the words may run across lines.
func (p *pageText) containsText(s string) bool {
	return findPhrase(p.exact, exactWords(s), 0) >= 0
}

// containsHeader is containsText for a label or header, which may carry a
// glued footnote reference.
func (p *pageText) containsHeader(s string) bool {
	want := normWords(s)
	for li := range p.lines {
		norms, _ := p.lines[li].lineNorms()
		if findPhrase(norms, want, 0) >= 0 {
			return true
		}
	}
	return false
}

// labelHit is one place a label is printed with the amounts that follow it.
type labelHit struct {
	line *pageLine
	run  []token // the cells after the label: amounts, dashes, percentages
	at   int     // the label's first word's index in the line's words
}

// findLabel returns every line printing the label, each with its run: the
// tokens after the label up to the next word that is not a cell (a word with
// letters, a date). Currency signs and junk inside the run are skipped.
//
// The label must be the whole printed label, not its tail: it starts the
// line or follows a cell (a side-by-side box), never another word, so
// "Contributions" does not match "Less: contributions".
func (p *pageText) findLabel(label string) []labelHit {
	want := normWords(label)
	var hits []labelHit
	for li := range p.lines {
		l := &p.lines[li]
		norms, idx := l.lineNorms()
		for from := 0; ; {
			at := findPhrase(norms, want, from)
			if at < 0 {
				break
			}
			from = at + 1
			if first := idx[at]; first > 0 && l.toks[first-1].kind == tokOther {
				continue // the tail of a longer label
			}
			after := idx[at+len(want)-1] + 1
			var run []token
			// A cell stands at least two spaces from its label; a number one
			// space after the label's last word is part of the label (the
			// year of "Balance, March 31, 2026"), so the label was cut short.
			// A currency sign or a junk glyph ("##") may sit closer.
			if after < len(l.toks) && (l.words[after].start-l.words[after-1].end >= 2 ||
				l.toks[after].kind == tokSign || l.toks[after].kind == tokJunk) {
				run = cellRun(l.toks[after:])
			}
			hits = append(hits, labelHit{line: l, run: run, at: idx[at]})
		}
	}
	return hits
}

func cellRun(toks []token) []token {
	var run []token
	for _, t := range toks {
		switch t.kind {
		case tokAmount, tokDash, tokPct:
			run = append(run, t)
		case tokSign, tokJunk:
		default:
			return run
		}
	}
	return run
}

// phrase is a group of words on a line separated by single spaces: a column
// header such as "Quarter to Date" or "Total Fund (Including GP)".
type phrase struct {
	text  string
	norm  []string
	start int
	end   int
}

func (l *pageLine) phrases() []phrase {
	var out []phrase
	for i := 0; i < len(l.words); {
		j := i + 1
		for j < len(l.words) && l.words[j].start-l.words[j-1].end < 2 {
			j++
		}
		var raws []string
		for _, w := range l.words[i:j] {
			raws = append(raws, w.raw)
		}
		text := strings.Join(raws, " ")
		out = append(out, phrase{norm: normWords(text), text: text, start: l.words[i].start, end: l.words[j-1].end})
		i = j
	}
	return out
}

// distance is how far a right edge lies from a header phrase: 0 when the
// edge falls under the phrase (allowing a right-aligned number to overhang
// it by the column tolerance), else the gap to its nearer end.
func (ph phrase) distance(edge int) int {
	switch {
	case edge < ph.start:
		return ph.start - edge
	case edge > ph.end+columnTolerance:
		return edge - ph.end - columnTolerance
	}
	return 0
}

// hasCells reports whether a line prints any table cell: an amount, a
// dash or a percentage. A header line prints none.
func (l *pageLine) hasCells() bool {
	for _, t := range l.toks {
		switch t.kind {
		case tokAmount, tokDash, tokPct:
			return true
		}
	}
	return false
}

// hasLooseCells reports whether the line prints a cell standing alone: a
// phrase of nothing but cells, signs and glyphs, set apart by two or more
// spaces ("Commitments        2.10%"). A number or dash inside a lettered
// phrase ("Total Fund (Including GP) - Class 1") belongs to a heading.
func (l *pageLine) hasLooseCells() bool {
	for i := 0; i < len(l.words); {
		j := i + 1
		for j < len(l.words) && l.words[j].start-l.words[j-1].end < 2 {
			j++
		}
		cells, letters := false, false
		for _, t := range l.toks[i:j] {
			switch t.kind {
			case tokAmount, tokDash, tokPct:
				cells = true
			case tokOther:
				letters = true
			}
		}
		if cells && !letters {
			return true
		}
		i = j
	}
	return false
}

// cellAt reports whether the line prints an amount or a dash in the column
// whose right edge is edge.
func (l *pageLine) cellAt(edge int) bool {
	for _, t := range l.toks {
		if (t.kind == tokAmount || t.kind == tokDash) && abs(t.end-edge) <= columnTolerance {
			return true
		}
	}
	return false
}

// amountAt returns the amount the line prints in the column whose right
// edge is edge; a dash there reads as zero.
func (l *pageLine) amountAt(edge int) (token, bool) {
	for _, t := range l.toks {
		if abs(t.end-edge) > columnTolerance {
			continue
		}
		switch t.kind {
		case tokAmount:
			return t, true
		case tokDash:
			t.value = decimal.Decimal{}
			return t, true
		}
	}
	return token{}, false
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
