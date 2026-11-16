package intake

import (
	"testing"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/decimal"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
)

func rec(investor, fund string, page int, label, amount string) *statement.Record {
	return &statement.Record{
		Investor: statement.Text{Text: investor, Page: page},
		Fund:     statement.Text{Text: fund, Page: page},
		Ending:   statement.Amount{Label: label, Amount: decimal.MustParse(amount), Page: page},
	}
}

// TestSamePosition: what the duplicate check treats as one position, and
// what it lets through as distinct (ADR 0018, item 4).
func TestSamePosition(t *testing.T) {
	base := rec("Example Holdings IV, LLC", "Keelson Partners VI, L.P.", 1, "Ending capital", "678250")
	same := map[string]*statement.Record{
		"identical":             rec("Example Holdings IV, LLC", "Keelson Partners VI, L.P.", 1, "Ending capital", "678250"),
		"investor prefix":       rec("Example Holdings IV", "Keelson Partners VI, L.P.", 2, "Other", "1"),
		"investor suffix":       rec("Holdings IV, LLC", "Keelson Partners VI, L.P.", 2, "Other", "1"),
		"investor longer":       rec("Example Holdings IV, LLC Series A", "Keelson Partners VI", 2, "Other", "1"),
		"spelling":              rec("EXAMPLE  HOLDINGS IV LLC", "Keelson Partners VI, L.P.", 2, "Other", "1"),
		"same ending":           rec("Another Investor", "Another Fund", 1, "Ending capital", "678250.00"),
		"same ending, footnote": rec("Another Investor", "Another Fund", 1, "Ending capital2", "678250"),
		// A table repeated on a later page: disjoint fragments of the one
		// printed name, each grounding there, and the same balance.
		"disjoint fragment, repeated page":    rec("Example", "Keelson Partners VI, L.P.", 3, "Ending capital", "678250"),
		"overlapping fragment, repeated page": rec("Holdings IV", "Partners VI", 3, "Ending capital", "678250"),
		"same amount, other label":            rec("Example Holdings V, LLC", "Keelson Partners VI, L.P.", 1, "Closing balance", "678250"),
		"repeat printed rounded":              rec("Another Investor", "Another Fund", 4, "Capital, end of period", "678249.60"),
		"repeat rounded up":                   rec("Another Investor", "Another Fund", 4, "Ending capital", "678251"),
	}
	for name, r := range same {
		if samePosition(base, r) == "" || samePosition(r, base) == "" {
			t.Errorf("%s: treated as a distinct position", name)
		}
	}
	distinct := map[string]*statement.Record{
		"other investor":         rec("Example Holdings V, LLC", "Keelson Partners VI, L.P.", 2, "Ending capital", "1000"),
		"other fund":             rec("Example Holdings IV, LLC", "Keelson Partners VII, L.P.", 2, "Ending capital", "1000"),
		"beyond rounding":        rec("Another Investor", "Another Fund", 1, "Ending capital", "678251.01"),
		"beyond rounding, below": rec("Another Investor", "Another Fund", 1, "Ending capital", "678248.99"),
		"digit kept":             rec("Example Holdings IV, LLC", "Keelson Partners 6, L.P.", 2, "Ending capital", "1000"),
	}
	for name, r := range distinct {
		if why := samePosition(base, r); why != "" {
			t.Errorf("%s: treated as the same position: %s", name, why)
		}
	}
}
