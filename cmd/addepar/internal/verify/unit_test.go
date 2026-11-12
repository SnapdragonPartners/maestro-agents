package verify_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/decimal"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/document"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
)

// These tests run the verifier on page text written here, so they need no
// PDF tools: the arithmetic and the boundaries of each rule.

const simplePage = `                 Example Fund I, LP
          For the period ending March 31, 2026
               Example Holdings IV, LLC

                              Quarter
Opening Balance                 100,000
Contributions                     5,000
Fees                               (101)
Gains                             2,000
Ending Balance                  106,899
`

func simpleDoc(pages ...string) *document.Document {
	d := &document.Document{SHA256: strings.Repeat("0", 64), Name: "t.pdf", PageCount: len(pages), TextLayer: document.TextUsable}
	for i, p := range pages {
		d.Pages = append(d.Pages, document.Page{Number: i + 1, Text: p, Usable: strings.TrimSpace(p) != ""})
	}
	return d
}

func simpleRecord(t *testing.T) *statement.Record {
	t.Helper()
	return decodeRecord(t, []byte(`{
	  "investor": {"text": "Example Holdings IV, LLC", "page": 1},
	  "fund": {"text": "Example Fund I, LP", "page": 1},
	  "period": {"end": "2026-03-31", "label": "For the period ending March 31, 2026", "page": 1},
	  "currency": {"code": "USD"},
	  "precision": "whole",
	  "column": {"label": "Quarter", "reason": "the only column", "page": 1},
	  "beginning": {"label": "Opening Balance", "amount": "100000", "page": 1},
	  "lines": [
	    {"kind": "movement", "label": "Contributions", "amount": "5000", "page": 1},
	    {"kind": "movement", "label": "Fees", "amount": "-101", "page": 1},
	    {"kind": "movement", "label": "Gains", "amount": "2000", "page": 1}
	  ],
	  "ending": {"label": "Ending Balance", "amount": "106899", "page": 1}
	}`))
}

func ids(r verify.Result, sev verify.Severity) []string { return findingIDs(r.Findings(sev)) }

func TestSimpleRecordVerifies(t *testing.T) {
	res := verify.Verify(simpleDoc(simplePage), simpleRecord(t))
	if res.Verdict != verify.Warn || !slices.Equal(ids(res, verify.Warning), []string{"6/not_stated/currency"}) {
		t.Fatalf("%s\n%s", res.Verdict, describe(&res))
	}
	if len(res.Checks) != 7 {
		t.Fatalf("%d checks", len(res.Checks))
	}
}

// TestRoundingTolerance checks check 2's boundary on a whole-unit
// statement: four addends (beginning and three movements) allow 2; a
// difference of 2 warns, 3 fails. The printed ending moves with the
// difference, so grounding still holds.
func TestRoundingTolerance(t *testing.T) {
	for _, c := range []struct {
		ending string
		want   verify.Status
	}{
		{ending: "106899", want: verify.Pass},
		{ending: "106900", want: verify.Warn},
		{ending: "106901", want: verify.Warn},
		{ending: "106897", want: verify.Warn},
		{ending: "106902", want: verify.Fail},
		{ending: "106896", want: verify.Fail},
	} {
		page := strings.Replace(simplePage, "106,899", "106,"+c.ending[3:], 1)
		rec := simpleRecord(t)
		rec.Ending.Amount = decimal.MustParse(c.ending)
		res := verify.Verify(simpleDoc(page), rec)
		if got := res.Checks[verify.CheckRollForward-1].Status; got != c.want {
			t.Errorf("ending %s: roll-forward %s, want %s\n%s", c.ending, got, c.want, describe(&res))
		}
	}
}

// TestCentsMustTie checks that a cents statement allows no difference.
func TestCentsMustTie(t *testing.T) {
	page := `Example Fund I, LP   Example Holdings IV, LLC   For the period ending March 31, 2026
                              Quarter
Opening Balance              $100.00
Fees                          ($0.10)
Ending Balance                $99.91
`
	rec := decodeRecord(t, []byte(`{
	  "investor": {"text": "Example Holdings IV, LLC", "page": 1},
	  "fund": {"text": "Example Fund I, LP", "page": 1},
	  "period": {"end": "2026-03-31", "label": "For the period ending March 31, 2026", "page": 1},
	  "currency": {"code": "USD"},
	  "precision": "cents",
	  "column": {"label": "Quarter", "reason": "the only column", "page": 1},
	  "beginning": {"label": "Opening Balance", "amount": "100.00", "page": 1},
	  "lines": [{"kind": "movement", "label": "Fees", "amount": "-0.10", "page": 1}],
	  "ending": {"label": "Ending Balance", "amount": "99.91", "page": 1}
	}`))
	res := verify.Verify(simpleDoc(page), rec)
	if !slices.Contains(ids(res, verify.Error), "2/does_not_tie") {
		t.Fatalf("a cent off passed on a cents statement\n%s", describe(&res))
	}
}

func TestUnreadableDocuments(t *testing.T) {
	rec := simpleRecord(t)
	scan := simpleDoc("  ")
	scan.TextLayer = document.TextNone
	res := verify.Verify(scan, rec)
	if !slices.Equal(ids(res, verify.Error), []string{"1/no_text_layer"}) {
		t.Fatalf("scan:\n%s", describe(&res))
	}
	// The roll-forward still runs without text; it ties here.
	if res.Checks[verify.CheckRollForward-1].Status != verify.Pass {
		t.Fatalf("roll-forward on a scan:\n%s", describe(&res))
	}

	partial := simpleDoc("cover page", simplePage)
	partial.Pages[0].Usable = false
	partial.TextLayer = document.TextPartial
	res = verify.Verify(partial, rec) // the record cites page 1, the unusable one
	if !slices.Contains(ids(res, verify.Error), "1/page_unusable/investor") {
		t.Fatalf("unusable page:\n%s", describe(&res))
	}
}

func TestInvalidRecord(t *testing.T) {
	rec := simpleRecord(t)
	rec.Precision = "thousands"
	res := verify.Verify(simpleDoc(simplePage), rec)
	if res.Verdict != verify.Fail || !slices.Equal(ids(res, verify.Error), []string{"1/invalid_record"}) {
		t.Fatalf("%s", describe(&res))
	}
	for _, c := range res.Checks[1:] {
		if c.Status != verify.Skipped {
			t.Fatalf("check %d ran on an invalid record", c.Check)
		}
	}
}

func TestPartialRollForward(t *testing.T) {
	page := `Example Fund I, LP   Example Holdings IV, LLC
For the period ending March 31, 2026
                              Quarter
Capital account             106,899
`
	rec := simpleRecord(t)
	rec.Beginning, rec.Lines = nil, nil
	rec.Ending.Label = "Capital account"
	res := verify.Verify(simpleDoc(page), rec)
	if got := ids(res, verify.Warning); !slices.Equal(got, []string{"6/not_stated/currency", "7/partial_roll_forward"}) || res.Verdict != verify.Warn {
		t.Fatalf("%v\n%s", got, describe(&res))
	}
}

func TestPeriodConflicts(t *testing.T) {
	page := strings.Replace(simplePage, "               Example Holdings IV, LLC",
		"               Example Holdings IV, LLC\n   For the quarter ended June 30, 2026", 1)
	res := verify.Verify(simpleDoc(page), simpleRecord(t))
	if !slices.Contains(ids(res, verify.Error), "5/conflicting_period/period") {
		t.Fatalf("%s", describe(&res))
	}
	// An "as of" date elsewhere is not a period end and does not conflict.
	page = strings.Replace(simplePage, "               Example Holdings IV, LLC",
		"               Example Holdings IV, LLC\n   Commitment as of June 30, 2019", 1)
	res = verify.Verify(simpleDoc(page), simpleRecord(t))
	if res.Checks[verify.CheckPeriod-1].Status != verify.Pass {
		t.Fatalf("%s", describe(&res))
	}
}

func TestFindingID(t *testing.T) {
	f := verify.Finding{Check: 1, Code: "amount_not_printed", Subject: "line 3"}
	if f.ID() != "1/amount_not_printed/line 3" {
		t.Fatal(f.ID())
	}
	if g := (verify.Finding{Check: 2, Code: "rounding_difference"}); g.ID() != "2/rounding_difference" {
		t.Fatal("no subject")
	}
	if verify.CheckName(99) != "check_99" || verify.CheckName(4) != "investor_not_fund" {
		t.Fatal("CheckName")
	}
}

// TestSectionNearestPhrase covers a section header whose phrases do not
// divide the columns evenly (two investor columns, one general partner,
// one total): each column goes to the nearest phrase, and an ending read
// from the general partner's column fails check 4.
func TestSectionNearestPhrase(t *testing.T) {
	page := `Example Fund I, LP   Example Holdings IV, LLC
For the period ending March 31, 2026
                         Limited Partner           General Partner    Total Partners
                         Quarter        Year       Quarter            Quarter
Opening Balance          100,000      90,000         1,000            101,000
Gains                      6,899      16,899            10              6,909
Ending Balance           106,899     106,899         1,010            107,909
`
	rec := simpleRecord(t)
	rec.Lines = rec.Lines[2:] // just Gains
	rec.Lines[0].Amount.Amount = decimal.MustParse("6899")
	rec.Column.Section = "Limited Partner"
	res := verify.Verify(simpleDoc(page), rec)
	if res.Checks[verify.CheckInvestor-1].Status != verify.Pass || res.Checks[verify.CheckConsistency-1].Status != verify.Pass {
		t.Fatalf("investor column:\n%s", describe(&res))
	}

	gp := simpleRecord(t)
	gp.Lines = gp.Lines[2:]
	gp.Beginning.Amount = decimal.MustParse("1000")
	gp.Lines[0].Amount.Amount = decimal.MustParse("10")
	gp.Ending.Amount = decimal.MustParse("1010")
	res = verify.Verify(simpleDoc(page), gp)
	if !slices.Contains(ids(res, verify.Error), "4/fund_column/ending") {
		t.Fatalf("general partner column passed:\n%s", describe(&res))
	}
}

// TestColumnLabelOnlyInProse: a column label printed only inside other
// text ("For the quarter ended") does not confirm the column.
func TestColumnLabelOnlyInProse(t *testing.T) {
	page := strings.Replace(simplePage, "                              Quarter\n", "                                QTD\n", 1)
	page = strings.Replace(page, "For the period ending March 31, 2026", "For the quarter ended March 31, 2026", 1)
	rec := simpleRecord(t)
	rec.Period.Label = "For the quarter ended March 31, 2026"
	res := verify.Verify(simpleDoc(page), rec)
	// The table's own header row says QTD; the prose mention confirms nothing.
	if !slices.Contains(ids(res, verify.Error), "1/column_mismatch/column") {
		t.Fatalf("%s", describe(&res))
	}
	rec.Column.Label = "QTD"
	res = verify.Verify(simpleDoc(page), rec)
	if res.Checks[verify.CheckGrounding-1].Status != verify.Pass {
		t.Fatalf("%s", describe(&res))
	}
}

// The fail-closed regressions (Codex round 1). Each page states its
// currency, so without the fix the wrong record would pass all seven checks.

const usdLine = "All amounts in USD\n"

func usd(page string) string { return usdLine + page }

func assertError(t *testing.T, res verify.Result, id string) {
	t.Helper()
	if !slices.Contains(ids(res, verify.Error), id) {
		t.Fatalf("want error %s\n%s", id, describe(&res))
	}
}

const groupedPage = `Example Fund I, LP   Example Holdings IV, LLC
For the period ending March 31, 2026
                         Limited Partner           General Partner    Total Partners
                         Quarter        Year       Quarter            Quarter
Opening Balance          100,000      90,000         1,000            101,000
Gains                      6,899      16,899            10              6,909
Ending Balance           106,899     106,899         1,010            107,909
`

func gpRecord(t *testing.T) *statement.Record {
	t.Helper()
	gp := simpleRecord(t)
	gp.Lines = gp.Lines[2:]
	gp.Beginning.Amount = decimal.MustParse("1000")
	gp.Lines[0].Amount.Amount = decimal.MustParse("10")
	gp.Ending.Amount = decimal.MustParse("1010")
	return gp
}

func TestClaimedSectionDoesNotMaskPartnerGroups(t *testing.T) {
	gp := gpRecord(t)
	gp.Column.Section = "Quarter" // a header row, claimed as the section
	assertError(t, verify.Verify(simpleDoc(usd(groupedPage)), gp), "4/fund_column/ending")
}

func TestOnePhraseFundHeading(t *testing.T) {
	page := strings.Replace(simplePage, "                              Quarter\n",
		"                    Total Fund (Including GP)\n                              Quarter\n", 1)
	assertError(t, verify.Verify(simpleDoc(usd(page)), simpleRecord(t)), "4/fund_table/ending")
}

func TestOpeningRowAsEnding(t *testing.T) {
	rec := simpleRecord(t)
	rec.Ending = *rec.Beginning
	rec.Lines = nil
	assertError(t, verify.Verify(simpleDoc(usd(simplePage)), rec), "2/ending_not_closing/ending")

	// An ending row printed above a movement is not the closing balance
	// either, whatever its label.
	page := strings.Replace(simplePage, "Ending Balance                  106,899\n", "", 1)
	page = strings.Replace(page, "Opening Balance                 100,000\n",
		"Opening Balance                 100,000\nEnding Balance                  106,899\n", 1)
	assertError(t, verify.Verify(simpleDoc(usd(page)), simpleRecord(t)), "2/ending_not_closing/ending")
}

func TestNamesKeepDigits(t *testing.T) {
	page := strings.Replace(simplePage, "Example Fund I, LP", "Example Series1, LP", 1)
	rec := simpleRecord(t)
	rec.Fund.Text = "Example Series2, LP"
	assertError(t, verify.Verify(simpleDoc(usd(page)), rec), "1/not_printed/fund")
	rec.Fund.Text = "Example Series1, LP"
	if res := verify.Verify(simpleDoc(usd(page)), rec); res.Verdict != verify.Pass {
		t.Fatalf("%s", describe(&res))
	}
}

func TestUnreportedEndingCopy(t *testing.T) {
	// A cited or investor-naming page disagreeing is an error.
	second := "Example Holdings IV, LLC\nEnding Balance                  999,999\n"
	assertError(t, verify.Verify(simpleDoc(usd(simplePage), second), simpleRecord(t)), "3/ending_disagrees/page 2")
	// A page of possibly another statement is a warning that blocks export.
	other := "Another Investor, LLC\nEnding Balance                  999,999\n"
	res := verify.Verify(simpleDoc(usd(simplePage), other), simpleRecord(t))
	if !slices.Contains(ids(res, verify.Warning), "3/other_page_ending/page 2") || res.Verdict != verify.Warn {
		t.Fatalf("%s", describe(&res))
	}
}

func TestTwoPeriodsOnOneLine(t *testing.T) {
	page := strings.Replace(simplePage, "          For the period ending March 31, 2026",
		"          For the period ending March 31, 2026     For the period ending June 30, 2026", 1)
	assertError(t, verify.Verify(simpleDoc(usd(page)), simpleRecord(t)), "5/conflicting_period/period")
}

func TestColumnOnAnotherPage(t *testing.T) {
	rec := simpleRecord(t)
	rec.Column.Page = 2
	res := verify.Verify(simpleDoc(usd(simplePage), "                Quarter     YTD\n"), rec)
	assertError(t, res, "1/column_unverified/column")
}

func TestLabelTailDoesNotGround(t *testing.T) {
	page := usd(simplePage) + "Less: contributions               (5,000)\n"
	rec := simpleRecord(t)
	rec.Lines[0].Amount.Amount = decimal.MustParse("-5000")
	assertError(t, verify.Verify(simpleDoc(page), rec), "1/amount_not_printed/line 1")
}

// Codex round 2: the table is bound from the page.

func TestIntermediateSubtotalAsEnding(t *testing.T) {
	page := strings.Replace(simplePage, "Fees                               (101)\n",
		"Fees                               (101)\nCapital before gains            104,899\n", 1)
	rec := simpleRecord(t)
	rec.Lines = rec.Lines[:2]
	rec.Ending = statement.Amount{Label: "Capital before gains", Amount: decimal.MustParse("104899"), Page: 1}
	assertError(t, verify.Verify(simpleDoc(usd(page)), rec), "2/ending_not_closing/ending")
}

func TestOmittedRow(t *testing.T) {
	rec := simpleRecord(t)
	rec.Lines = append(rec.Lines[:1], rec.Lines[2:]...) // drop Fees
	rec.Ending.Amount = decimal.MustParse("107000")
	page := strings.Replace(simplePage, "106,899", "107,000", 1)
	assertError(t, verify.Verify(simpleDoc(usd(page)), rec), "2/row_not_submitted")
}

func TestFundHeadingBeyondWindow(t *testing.T) {
	page := strings.Replace(simplePage, "                              Quarter\n",
		"                    Total Fund (Including GP)\n            Unaudited\n            Statement\n            Detail\n                              Quarter\n", 1)
	assertError(t, verify.Verify(simpleDoc(usd(page)), simpleRecord(t)), "4/fund_table/ending")
}

func TestSideBySideEndingCopies(t *testing.T) {
	page := strings.Replace(simplePage, "Ending Balance                  106,899\n",
		"Ending Balance                  106,899        Ending Balance     999,999\n", 1)
	assertError(t, verify.Verify(simpleDoc(usd(page)), simpleRecord(t)), "3/ending_disagrees/page 1")
}

func TestHeaderBoundToTable(t *testing.T) {
	page := strings.Replace(simplePage, "                              Quarter\n", "                                YTD\n", 1)
	page += "\n                              Quarter\n"
	assertError(t, verify.Verify(simpleDoc(usd(page)), simpleRecord(t)), "1/column_mismatch/column")
}
