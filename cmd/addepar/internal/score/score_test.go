package score_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/decimal"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/export"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/intake"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/score"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
	"github.com/SnapdragonPartners/maestro-agents/scoring"
	"github.com/SnapdragonPartners/maestro-agents/standalone"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// keyCSV builds an answer key as Excel saves the template sheet: a title
// row, a help row, the header, then rows.
func keyCSV(rows ...[]string) string {
	var b strings.Builder
	b.WriteString("\xef\xbb\xbfValuations and snapshots import template" + strings.Repeat(",", 18) + "\n")
	b.WriteString(`"Require one` + "\n\n" + `The direct owner"` + strings.Repeat(",", 18) + "\n")
	b.WriteString(strings.Join(export.Columns(), ",") + "\n")
	w := csv.NewWriter(&b)
	if err := w.WriteAll(rows); err != nil {
		panic(err)
	}
	b.WriteString(strings.Repeat(",", 18) + "\n") // a trailing blank row
	return b.String()
}

// keyRow is a template row: owner, asset, date, value, currency.
func keyRow(ownerID, owner, assetID, asset, date, value, currency string) []string {
	r := make([]string, 19)
	r[0], r[1], r[3], r[4], r[10], r[11], r[12], r[17] = ownerID, owner, assetID, asset, "Value Based", date, value, currency
	return r
}

func TestLoadKey(t *testing.T) {
	k, err := score.LoadKey(strings.NewReader(keyCSV(
		keyRow("1", "EXAMPLE HOLDINGS IV, LLC", "10", "Keelson Partners VI, LP", "3/31/2026", "604,211.40", "USD"),
		keyRow("1", "EXAMPLE HOLDINGS IV, LLC", "11", "Tributary Fund III", "2026-03-31", "2038338", "USD"),
	)))
	if err != nil {
		t.Fatal(err)
	}
	r, err := k.Lookup("1", "10")
	if err != nil {
		t.Fatal(err)
	}
	if r.String() != "2026-03-31 604211.40 USD" || r.Name() != "EXAMPLE HOLDINGS IV, LLC / Keelson Partners VI, LP" || r.Line != 6 {
		t.Fatalf("row %+v (%s)", r, r)
	}
	if r, err = k.Lookup("1", "11"); err != nil || !r.Value.Equal(decimal.MustParse("2038338.00")) {
		t.Fatalf("row %+v, %v", r, err)
	}
	if _, err := k.Lookup("1", "12"); !errors.Is(err, score.ErrInvalid) {
		t.Fatalf("missing row: %v", err)
	}
}

func TestLoadKeyDuplicateRowIsRefusedOnLookup(t *testing.T) {
	k, err := score.LoadKey(strings.NewReader(keyCSV(
		keyRow("1", "A", "10", "F", "2026-03-31", "1", "USD"),
		keyRow("1", "A", "10", "F", "2026-03-31", "2", "USD"),
	)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Lookup("1", "10"); !errors.Is(err, score.ErrInvalid) || !strings.Contains(err.Error(), "2 rows") {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestLoadKeyRejects(t *testing.T) {
	good := keyRow("1", "A", "10", "F", "2026-03-31", "1", "USD")
	short := strings.Join(good[:18], ",")
	for name, in := range map[string]string{
		"no header":        "a,b,c\n1,2,3\n",
		"short row":        keyCSV() + short + "\n",
		"bad date":         keyCSV(keyRow("1", "A", "10", "F", "31.03.2026", "1", "USD")),
		"bad value":        keyCSV(keyRow("1", "A", "10", "F", "2026-03-31", "$1", "USD")),
		"misgrouped value": keyCSV(keyRow("1", "A", "10", "F", "2026-03-31", "1,00", "USD")),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := score.LoadKey(strings.NewReader(in)); !errors.Is(err, score.ErrInvalid) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

const sha1 = "1111111111111111111111111111111111111111111111111111111111111111"
const sha2 = "2222222222222222222222222222222222222222222222222222222222222222"

func TestLoadMappingRejects(t *testing.T) {
	pos := `{"investor": "Example Holdings IV, LLC", "fund": "Keelson VI", "owner_id": "1", "asset_id": "10"}`
	for name, in := range map[string]string{
		"unknown field":     `{"statements": [], "extra": 1}`,
		"trailing data":     `{"statements": []} {}`,
		"no statements":     `{}`,
		"bad sha":           `{"statements": [{"sha256": "ABC", "positions": []}]}`,
		"statement twice":   `{"statements": [{"sha256": "` + sha1 + `", "positions": []}, {"sha256": "` + sha1 + `", "positions": []}]}`,
		"no positions list": `{"statements": [{"sha256": "` + sha1 + `"}]}`,
		"blank investor":    `{"statements": [{"sha256": "` + sha1 + `", "positions": [{"investor": " ", "fund": "F", "owner_id": "1", "asset_id": "10"}]}]}`,
		"same names twice":  `{"statements": [{"sha256": "` + sha1 + `", "positions": [` + pos + `, {"investor": "EXAMPLE HOLDINGS IV LLC", "fund": "keelson vi", "owner_id": "1", "asset_id": "11"}]}]}`,
		"same row twice":    `{"statements": [{"sha256": "` + sha1 + `", "positions": [` + pos + `, {"investor": "Other", "fund": "F", "owner_id": "1", "asset_id": "10"}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := score.LoadMapping(strings.NewReader(in)); !errors.Is(err, score.ErrInvalid) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// objects is a store.Reader holding objects in one namespace.
type objects struct {
	store.Reader
	m map[string]any
}

func (o *objects) Object(_ context.Context, ns, key string) (store.Object, bool, error) {
	if ns != "intake" {
		return store.Object{}, false, fmt.Errorf("namespace %q", ns)
	}
	v, ok := o.m[key]
	if !ok {
		return store.Object{}, false, nil
	}
	raw, err := json.Marshal(v)
	return store.Object{JSON: raw}, true, err
}

func record(id, investor, fund, end, amount, currency string) export.Record {
	return export.Record{ID: id, Record: &statement.Record{
		Investor: statement.Text{Text: investor, Page: 1}, Fund: statement.Text{Text: fund, Page: 1},
		Period: statement.Period{End: end, Page: 1}, Currency: statement.Currency{Code: currency},
		Ending: statement.Amount{Label: "Ending", Amount: decimal.MustParse(amount), Page: 1},
	}}
}

func comparator(t *testing.T) *score.Comparator {
	t.Helper()
	k, err := score.LoadKey(strings.NewReader(keyCSV(
		keyRow("1", "EXAMPLE HOLDINGS IV, LLC", "10", "Keelson Partners VI, LP", "3/31/2026", "604211.40", "USD"),
		keyRow("1", "EXAMPLE HOLDINGS IV, LLC", "11", "Tributary Fund III", "3/31/2026", "2038338", "USD"),
		keyRow("2", "SAMPLE PARTNERS VII, LLC", "11", "Tributary Fund III", "3/31/2026", "100", "USD"),
	)))
	if err != nil {
		t.Fatal(err)
	}
	m, err := score.LoadMapping(strings.NewReader(`{"statements": [
		{"sha256": "` + sha1 + `", "name": "two positions", "positions": [
			{"investor": "Example Holdings IV, LLC", "fund": "Keelson Partners VI, L.P.", "owner_id": "1", "asset_id": "10"},
			{"investor": "Example Holdings IV, LLC", "fund": "Tributary Fund 3, LP", "owner_id": "1", "asset_id": "11"}
		]},
		{"sha256": "` + sha2 + `", "name": "a K-1", "positions": []}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	c, err := score.NewComparator(k, m)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func verdicts(items []scoring.Item) string {
	vs := make([]string, len(items))
	for i, it := range items {
		vs[i] = string(it.Verdict)
	}
	return strings.Join(vs, ",")
}

func TestCompare(t *testing.T) {
	c := comparator(t)
	ok := standalone.Report{SHA256: sha1, Status: store.StatusSucceeded}
	verified := func(recs ...export.Record) map[string]any {
		m := map[string]any{}
		o := intake.Outcome{Disposition: intake.DispositionVerified}
		for _, r := range recs {
			o.Records = append(o.Records, r.ID)
			m[intake.RecordKey(r.ID)] = r
		}
		m[intake.OutcomeKey(sha1)] = o
		return m
	}
	keelson := record(sha1+"/01", "EXAMPLE HOLDINGS IV LLC", "Keelson Partners VI, L.P.", "2026-03-31", "604211.40", "USD")
	tributary := record(sha1+"/02", "Example Holdings IV, LLC", "Tributary Fund 3, LP", "2026-03-31", "2038338.00", "USD")
	for name, tc := range map[string]struct {
		objs   map[string]any
		report standalone.Report
		want   string
		reason string
	}{
		"both match": {verified(keelson, tributary), ok, "match,match", ""},
		"wrong value": {verified(keelson, record(sha1+"/02", "Example Holdings IV, LLC", "Tributary Fund 3, LP", "2026-03-31", "2038339", "USD")), ok,
			"match,mismatch", "differs in value"},
		"wrong date and currency": {verified(keelson, record(sha1+"/02", "Example Holdings IV, LLC", "Tributary Fund 3, LP", "2025-12-31", "2038338", "EUR")), ok,
			"match,mismatch", "differs in date, currency"},
		"one missed": {verified(keelson), ok, "match,missed", "no verified record"},
		"unknown position": {verified(keelson, tributary, record(sha1+"/03", "Someone Else", "Keelson Partners VI, L.P.", "2026-03-31", "1", "USD")), ok,
			"match,match,hallucination", "matches no position"},
		"repeated position": {verified(keelson, tributary, record(sha1+"/03", "Example Holdings IV, LLC", "Keelson Partners VI, L.P.", "2026-03-31", "604211.40", "USD")), ok,
			"match,match,hallucination", "repeats a position"},
		"manual review": {map[string]any{intake.OutcomeKey(sha1): intake.Outcome{Disposition: intake.DispositionManual, Reason: "attempts spent"}}, ok,
			"exception,exception", "manual_review: attempts spent"},
		"run failed": {nil, standalone.Report{SHA256: sha1, Status: store.StatusFailed, Reason: store.Reason{Code: "budget_exhausted"}}, "exception,exception",
			"the run ended failed (budget_exhausted)"},
		"run not ended": {nil, standalone.Report{SHA256: sha1, Error: "store unreachable"}, "exception,exception", "did not end: store unreachable"},
	} {
		t.Run(name, func(t *testing.T) {
			items, err := c.Compare(context.Background(), &scoring.Output{Store: &objects{m: tc.objs}, Namespace: "intake", Report: tc.report})
			if err != nil {
				t.Fatal(err)
			}
			if got := verdicts(items); got != tc.want {
				t.Fatalf("verdicts %s, want %s: %+v", got, tc.want, items)
			}
			if tc.reason != "" && !strings.Contains(items[len(items)-1].Reason, tc.reason) {
				t.Fatalf("reason %q lacks %q", items[len(items)-1].Reason, tc.reason)
			}
		})
	}
}

func TestCompareItemsShowAnswerAndOutput(t *testing.T) {
	c := comparator(t)
	rec := record(sha1+"/01", "Example Holdings IV, LLC", "Keelson Partners VI, L.P.", "2026-03-31", "604211.39", "USD")
	objs := map[string]any{
		intake.OutcomeKey(sha1):  intake.Outcome{Disposition: intake.DispositionVerified, Records: []string{rec.ID}},
		intake.RecordKey(rec.ID): rec,
	}
	items, err := c.Compare(context.Background(), &scoring.Output{Store: &objects{m: objs}, Namespace: "intake", Report: standalone.Report{SHA256: sha1, Status: store.StatusSucceeded}})
	if err != nil {
		t.Fatal(err)
	}
	it := items[0]
	if it.Key != "EXAMPLE HOLDINGS IV, LLC / Keelson Partners VI, LP" || it.Want != "2026-03-31 604211.40 USD" || it.Got != "2026-03-31 604211.39 USD" || it.Verdict != scoring.Mismatch {
		t.Fatalf("item %+v", it)
	}
}

func TestCompareNoPositions(t *testing.T) {
	c := comparator(t)
	out := func(objs map[string]any, rep standalone.Report) *scoring.Output {
		rep.SHA256 = sha2
		return &scoring.Output{Store: &objects{m: objs}, Namespace: "intake", Report: rep}
	}
	ok := standalone.Report{Status: store.StatusSucceeded}
	items, err := c.Compare(context.Background(), out(map[string]any{intake.OutcomeKey(sha2): intake.Outcome{Disposition: intake.DispositionSetAside}}, ok))
	if err != nil || len(items) != 0 {
		t.Fatalf("set aside, nothing expected: %+v, %v", items, err)
	}
	rec := record(sha2+"/01", "A", "F", "2026-03-31", "1", "USD")
	items, err = c.Compare(context.Background(), out(map[string]any{
		intake.OutcomeKey(sha2):  intake.Outcome{Disposition: intake.DispositionVerified, Records: []string{rec.ID}},
		intake.RecordKey(rec.ID): rec,
	}, ok))
	if err != nil || verdicts(items) != "hallucination" {
		t.Fatalf("a record where none is expected: %+v, %v", items, err)
	}
	items, err = c.Compare(context.Background(), out(nil, standalone.Report{Status: store.StatusFailed}))
	if err != nil || verdicts(items) != "exception" || items[0].Key != "(statement)" {
		t.Fatalf("a failed run: %+v, %v", items, err)
	}
}

func TestCompareErrors(t *testing.T) {
	c := comparator(t)
	ok := standalone.Report{SHA256: sha1, Status: store.StatusSucceeded}
	if _, err := c.Compare(context.Background(), &scoring.Output{Report: standalone.Report{SHA256: strings.Repeat("3", 64)}}); !errors.Is(err, scoring.ErrNoAnswer) {
		t.Fatalf("unmapped: %v", err)
	}
	if _, err := c.Compare(context.Background(), &scoring.Output{Store: &objects{}, Namespace: "intake", Report: ok}); err == nil {
		t.Fatal("a succeeded run without an outcome compared")
	}
	objs := map[string]any{intake.OutcomeKey(sha1): intake.Outcome{Disposition: intake.DispositionVerified, Records: []string{sha1 + "/01"}}}
	if _, err := c.Compare(context.Background(), &scoring.Output{Store: &objects{m: objs}, Namespace: "intake", Report: ok}); err == nil {
		t.Fatal("a missing record compared")
	}
}

func TestNewComparatorChecksTheMapping(t *testing.T) {
	k, err := score.LoadKey(strings.NewReader(keyCSV(keyRow("1", "A", "10", "F", "2026-03-31", "1", "USD"))))
	if err != nil {
		t.Fatal(err)
	}
	m, err := score.LoadMapping(strings.NewReader(`{"statements": [{"sha256": "` + sha1 + `", "positions": [{"investor": "A", "fund": "F", "owner_id": "1", "asset_id": "99"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := score.NewComparator(k, m); !errors.Is(err, score.ErrInvalid) {
		t.Fatalf("a position naming no row: %v", err)
	}
}
