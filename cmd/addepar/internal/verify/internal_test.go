package verify

import (
	"slices"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/decimal"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		raw   string
		kind  tokenKind
		value string
		cents bool
	}{
		{raw: "$714,760.65", kind: tokAmount, value: "714760.65", cents: true},
		{raw: "($15,302.17)", kind: tokAmount, value: "-15302.17", cents: true},
		{raw: "$(2,904.66)", kind: tokAmount, value: "-2904.66", cents: true},
		{raw: "(4,165)", kind: tokAmount, value: "-4165"},
		{raw: "-4,165", kind: tokAmount, value: "-4165"},
		{raw: "124", kind: tokAmount, value: "124"},
		{raw: "0", kind: tokAmount, value: "0"},
		{raw: "2,948,715", kind: tokAmount, value: "2948715"},
		{raw: "-", kind: tokDash},
		{raw: "$-", kind: tokDash},
		{raw: "—", kind: tokDash},
		{raw: "$", kind: tokSign},
		{raw: "##", kind: tokJunk},
		{raw: "2.10%", kind: tokPct},
		{raw: "03/31/2026", kind: tokOther},
		{raw: "Balance", kind: tokOther},
		{raw: "(4,165", kind: tokOther},
		{raw: "1,04", kind: tokOther},
		{raw: "-(5)", kind: tokOther},
		{raw: "Contributions1", kind: tokOther},
	}
	for _, c := range cases {
		tok := classify(word{raw: c.raw})
		if tok.kind != c.kind {
			t.Errorf("%q: kind %d, want %d", c.raw, tok.kind, c.kind)
			continue
		}
		if c.kind == tokAmount && (!tok.value.Equal(decimal.MustParse(c.value)) || tok.cents != c.cents) {
			t.Errorf("%q: value %s cents %v, want %s %v", c.raw, tok.value, tok.cents, c.value, c.cents)
		}
	}
}

func TestNormWord(t *testing.T) {
	cases := map[string]string{
		"Contributions1": "contributions",
		"Gain/(Loss)3":   "gain/(loss)",
		"Partner:":       "partner",
		"LLC,":           "llc",
		"3":              "3",
		"2026":           "2026",
		"Partners’":      "partners'",
		"##":             "",
		"$":              "",
	}
	for in, want := range cases {
		if got := normWord(in, true); got != want {
			t.Errorf("normWord(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFindLabelRun(t *testing.T) {
	p := readPage("Beginning Balance        $  214,906     $ 214,906    $   -\n" +
		"  Capital Contributions1          90,000       90,000     360,000\n" +
		"Current Capital Balance   $ 288,412        Total Capital Commitment   $ 2,000,000\n" +
		"Capital Balance, March 31, 2026    $714,760.65\n" +
		"   Interest income ##      1,300    ##   1,300")
	run := func(label string) []string {
		var out []string
		for _, h := range p.findLabel(label) {
			for _, tok := range h.run {
				if tok.kind == tokDash {
					out = append(out, "-")
				} else {
					out = append(out, tok.value.String())
				}
			}
		}
		return out
	}
	check := func(label string, want ...string) {
		t.Helper()
		if got := run(label); !slices.Equal(got, want) {
			t.Errorf("%q: run %v, want %v", label, got, want)
		}
	}
	check("Beginning Balance", "214906", "214906", "-")
	check("Capital Contributions", "90000", "90000", "360000")
	check("Current Capital Balance", "288412") // the run stops at the next label
	check("Capital Balance, March 31, 2026", "714760.65")
	check("Capital Balance, March 31") // cut short: "2026" belongs to the label
	check("Interest income", "1300", "1300")
	check("Not a label")
}

func TestPeriodParsing(t *testing.T) {
	d := func(s string) time.Time { tm, _ := time.Parse(time.DateOnly, s); return tm }
	cases := []struct {
		label   string
		end     string
		isRange bool
		ok      bool
	}{
		{label: "For the period ending March 31, 2026", end: "2026-03-31", ok: true},
		{label: "Period Ended Mar. 31, 2026", end: "2026-03-31", ok: true},
		{label: "For the quarter ended 31 March 2026", end: "2026-03-31", ok: true},
		{label: "01/01/2026 - 03/31/2026", end: "2026-03-31", isRange: true, ok: true},
		{label: "January 1, 2026 through March 31, 2026", end: "2026-03-31", isRange: true, ok: true},
		{label: "as of 2026-03-31", end: "2026-03-31", ok: true},
		{label: "3/31/26", end: "2026-03-31", ok: true},
		{label: "02/31/2026", ok: false},
		{label: "03/31/2026 and 06/30/2026", ok: false},
		{label: "06/30/2026 - 03/31/2026", ok: false},
		{label: "Q1 2026", ok: false},
	}
	for _, c := range cases {
		end, isRange, ok := periodEnd(c.label)
		if ok != c.ok || (ok && (!end.Equal(d(c.end)) || isRange != c.isRange)) {
			t.Errorf("%q: %v %v %v, want %s %v %v", c.label, end, isRange, ok, c.end, c.isRange, c.ok)
		}
	}
	for _, s := range []string{"06/30/2026 12:31 PM", "Printed on March 31, 2026", "Generated 03/31/2026", "06/30/2026 9am"} {
		if !isTimestamp(s) {
			t.Errorf("%q not a timestamp", s)
		}
	}
	for _, s := range []string{"For the period ending March 31, 2026", "01/01/2026 - 03/31/2026"} {
		if isTimestamp(s) {
			t.Errorf("%q read as a timestamp", s)
		}
	}
	if !isBareDate("      March 31, 2026   ") || isBareDate("Statement March 31, 2026") {
		t.Error("isBareDate")
	}
}

func TestCurrencies(t *testing.T) {
	names := currencyNames()
	cases := map[string][]string{
		"All amounts in USD":                     {"USD"},
		"Expressed in U.S. Dollars":              {"USD"},
		"Australian dollars (AUD)":               {"AUD"},
		"$714,760.65":                            {},
		"the audit committee":                    {},
		"Amounts in EUR and GBP":                 {"EUR", "GBP"},
		"usd lower-case is not a code":           {},
		"A$ 6,909,665":                           {"AUD"},
		"Total Capital Commitment $ 2,000,000 C": {},
	}
	for text, want := range cases {
		if got := currenciesIn(names, text); !slices.Equal(got, want) {
			t.Errorf("%q: %v, want %v", text, got, want)
		}
	}
}

// TestCheckNotRunFails pins fail-closed for an unwired check: a check that
// neither ran nor was skipped fails as not_run instead of reporting a pass
// (buggrit, PR #14 pass 4).
func TestCheckNotRunFails(t *testing.T) {
	v := newVerifier(nil, nil)
	for n := 1; n < CheckCount; n++ {
		v.ran[n] = true
	}
	r := v.result()
	last := r.Checks[CheckCount-1]
	if r.Verdict != Fail || last.Status != Fail || len(last.Findings) != 1 || last.Findings[0].Code != "not_run" {
		t.Fatalf("unrun check: verdict %s, %+v", r.Verdict, last)
	}
	for _, c := range r.Checks[:CheckCount-1] {
		if c.Status != Pass {
			t.Fatalf("check %d: %s", c.Check, c.Status)
		}
	}
}
