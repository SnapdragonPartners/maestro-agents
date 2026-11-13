package verify

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

// currencyNames maps how statements state a currency, in code or in words,
// to its ISO 4217 code. A bare "$" is deliberately absent: USD, AUD, CAD and
// others all print it (check 6).
type currencyName struct {
	re   *regexp.Regexp
	code string
}

// currencyTable compiles the table once; it is read-only afterwards.
var currencyTable = sync.OnceValue(currencyNames) //nolint:gochecknoglobals // compiled once, never written

func currencyNames() []currencyName {
	words := []struct{ pattern, code string }{
		{`\bUSD\b|\bU\.S\.\s*dollars?\b|\bUS\s+dollars?\b|\bUnited States dollars?\b`, "USD"},
		{`\bAUD\b|\bA\$|\bAustralian dollars?\b`, "AUD"},
		{`\bCAD\b|\bC\$|\bCanadian dollars?\b`, "CAD"},
		{`\bNZD\b|\bNew Zealand dollars?\b`, "NZD"},
		{`\bSGD\b|\bSingapore dollars?\b`, "SGD"},
		{`\bHKD\b|\bHong Kong dollars?\b`, "HKD"},
		{`\bEUR\b|\beuros?\b|€`, "EUR"},
		{`\bGBP\b|\bpounds? sterling\b|£`, "GBP"},
		{`\bCHF\b|\bSwiss francs?\b`, "CHF"},
		{`\bJPY\b|\bJapanese yen\b|¥`, "JPY"},
	}
	out := make([]currencyName, len(words))
	for i, w := range words {
		// Codes are matched case-sensitively (a word "aud" in prose is not
		// AUD); names case-insensitively.
		out[i] = currencyName{re: regexp.MustCompile(caseFoldNames(w.pattern)), code: w.code}
	}
	return out
}

// caseFoldNames makes the word alternatives of a pattern case-insensitive
// while leaving the three-letter codes case-sensitive.
func caseFoldNames(pattern string) string {
	alts := strings.Split(pattern, "|")
	for i, a := range alts {
		if regexp.MustCompile(`^\\b[A-Z]{3}\\b$`).MatchString(a) {
			continue
		}
		alts[i] = "(?i:" + a + ")"
	}
	return strings.Join(alts, "|")
}

// currenciesIn returns the codes text states, sorted and distinct.
func currenciesIn(names []currencyName, text string) []string {
	seen := map[string]bool{}
	for _, n := range names {
		if n.re.MatchString(text) {
			seen[n.code] = true
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
