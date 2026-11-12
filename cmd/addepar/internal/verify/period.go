package verify

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

// Dates are found in printed text in the forms statements use: "March 31,
// 2026", "Mar. 31, 2026", "31 March 2026", "03/31/2026", "3/31/26" and
// "2026-03-31".
var (
	dateMDY     = regexp.MustCompile(`(?i)\b(jan|feb|mar|apr|may|jun|jul|aug|sep|sept|oct|nov|dec)[a-z]*\.?\s+(\d{1,2}),?\s+(\d{4})\b`)
	dateDMY     = regexp.MustCompile(`(?i)\b(\d{1,2})\s+(jan|feb|mar|apr|may|jun|jul|aug|sep|sept|oct|nov|dec)[a-z]*\.?,?\s+(\d{4})\b`)
	dateSlash   = regexp.MustCompile(`\b(\d{1,2})/(\d{1,2})/(\d{4}|\d{2})\b`)
	dateISO     = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`)
	timeOfDay   = regexp.MustCompile(`(?i)\b\d{1,2}:\d{2}(:\d{2})?\s*(am|pm)?\b|\b\d{1,2}\s*(am|pm)\b`)
	rangeJoin   = regexp.MustCompile(`(?i)^\s*(-|to|through|thru)\s*$`)
	printedWord = regexp.MustCompile(`(?i)\b(printed|generated|run date|run on|created|downloaded|exported|print date)\b`)
	// periodWord marks a label as a period label.
	periodWord = regexp.MustCompile(`(?i)\b(period|quarter|year|months?)\s+(end|ending|ended)\b|\bfor the (period|quarter|year|three months|six months|nine months|twelve months)\b|\b(as of|as at|through|ended|ending)\b`)
	// strongPeriodWord marks a line whose date is a period end the whole
	// statement must agree on. "As of" is left out: statements use it for
	// other dates (a commitment as of its closing).
	strongPeriodWord = regexp.MustCompile(`(?i)\b(period|quarter|year|months?)\s+(end|ending|ended)\b|\bfor the (period|quarter|year|three months|six months|nine months|twelve months)\b`)
)

// monthOf returns the month a name or abbreviation names.
func monthOf(name string) int {
	months := []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
	name = strings.ToLower(name)
	for i, m := range months {
		if strings.HasPrefix(name, m) {
			return i + 1
		}
	}
	return 0
}

type foundDate struct {
	date       time.Time
	start, end int // byte offsets in the text
}

// dateFormat reads one printed date form: its pattern, and which submatch
// holds the year, the month and the day.
type dateFormat struct {
	re               *regexp.Regexp
	year, mon, day   int
	monthIsName      bool
	twoDigitYearBase int
}

func dateFormats() []dateFormat {
	return []dateFormat{
		{re: dateMDY, year: 3, mon: 1, day: 2, monthIsName: true},
		{re: dateDMY, year: 3, mon: 2, day: 1, monthIsName: true},
		{re: dateSlash, year: 3, mon: 1, day: 2, twoDigitYearBase: 2000},
		{re: dateISO, year: 1, mon: 2, day: 3},
	}
}

// findDates returns every valid date printed in s, in the order printed.
// Where two forms overlap, the first form listed wins.
func findDates(s string) []foundDate {
	var out []foundDate
	for _, f := range dateFormats() {
		for _, m := range f.re.FindAllStringSubmatchIndex(s, -1) {
			if d, ok := f.read(s, m); ok && !overlaps(out, m) {
				out = append(out, foundDate{date: d, start: m[0], end: m[1]})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

func (f dateFormat) read(s string, m []int) (time.Time, bool) {
	group := func(i int) string { return s[m[2*i]:m[2*i+1]] }
	y, d := atoi(group(f.year)), atoi(group(f.day))
	if y < 100 {
		y += f.twoDigitYearBase
	}
	mon := 0
	if f.monthIsName {
		mon = monthOf(group(f.mon))
	} else {
		mon = atoi(group(f.mon))
	}
	if mon < 1 || mon > 12 {
		return time.Time{}, false
	}
	t := time.Date(y, time.Month(mon), d, 0, 0, 0, 0, time.UTC)
	if t.Day() != d || int(t.Month()) != mon {
		return time.Time{}, false // 02/31 is not a date
	}
	return t, true
}

func overlaps(found []foundDate, m []int) bool {
	for _, f := range found {
		if m[0] < f.end && f.start < m[1] {
			return true
		}
	}
	return false
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

// periodEnd reads the period end a label states: its one date, or the later
// end of a range ("01/01/2026 - 03/31/2026"). ok is false when the label has
// no date or dates that are not one range.
func periodEnd(label string) (end time.Time, isRange, ok bool) {
	ds := findDates(label)
	switch {
	case len(ds) == 1:
		return ds[0].date, false, true
	case len(ds) == 2 && rangeJoin.MatchString(label[ds[0].end:ds[1].start]) && !ds[1].date.Before(ds[0].date):
		return ds[1].date, true, true
	}
	return time.Time{}, false, false
}

// isTimestamp reports whether text reads as a print or generation time: a
// date with a time of day, or a word such as "printed" or "generated".
func isTimestamp(text string) bool {
	return timeOfDay.MatchString(text) || printedWord.MatchString(text)
}

// isBareDate reports whether text is nothing but one date.
func isBareDate(text string) bool {
	ds := findDates(text)
	return len(ds) == 1 && strings.TrimSpace(text[:ds[0].start]) == "" && strings.TrimSpace(text[ds[0].end:]) == ""
}
