package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-cms/extract/pdf"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/decimal"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/export"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/review"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
)

func TestFormatAmount(t *testing.T) {
	for in, want := range map[string]string{
		"0": "0", "714760.65": "714,760.65", "-1234": "(1,234)", "100": "100", "1000": "1,000", "1234567.5": "1,234,567.5",
	} {
		if got := formatAmount(in); got != want {
			t.Errorf("formatAmount(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeAmount(t *testing.T) {
	for in, want := range map[string]string{
		"$1,234.56": "1234.56", "(1,234)": "-1234", "-12": "-12", " 7 ": "7",
	} {
		if got := normalizeAmount(in); got != want {
			t.Errorf("normalizeAmount(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafeNextAndNames(t *testing.T) {
	for in, want := range map[string]string{
		"/records/x": "/records/x", "//evil.example/": "/", "https://evil.example": "/", "/\\evil": "/", "": "/",
		"/\t/evil.example/": "/", "/\n/evil.example": "/", "/a\x7fb": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q", in, got)
		}
	}
	if n, ok := cleanName("  Jordan Lee "); !ok || n != "Jordan Lee" {
		t.Fatalf("cleanName: %q %v", n, ok)
	}
	for _, bad := range []string{"", "   ", "a\nb", "a\x00", strings.Repeat("x", maxName+1), "\xff"} {
		if _, ok := cleanName(bad); ok {
			t.Errorf("cleanName(%q) accepted", bad)
		}
	}
}

func TestAllowedHosts(t *testing.T) {
	h, err := allowedHosts("127.0.0.1:8741")
	if err != nil || !h["127.0.0.1:8741"] || !h["localhost:8741"] || h["example.com:8741"] || h["127.0.0.1:80"] {
		t.Fatalf("hosts %v %v", h, err)
	}
	if h, err = allowedHosts("[::1]:9000"); err != nil || !h["[::1]:9000"] || !h["localhost:9000"] {
		t.Fatalf("v6 hosts %v %v", h, err)
	}
	for _, bad := range []string{"0.0.0.0:80", "10.0.0.1:80", "example.com:80", "127.0.0.1", "127.0.0.1:0"} {
		if _, err := allowedHosts(bad); err == nil {
			t.Errorf("allowedHosts(%q) accepted", bad)
		}
	}
}

func TestLRU(t *testing.T) {
	c := newLRU[int](2)
	c.put("a", 1)
	c.put("b", 2)
	c.get("a")
	c.put("c", 3) // evicts b, the least recently used
	if _, ok := c.get("b"); ok {
		t.Fatal("b was kept")
	}
	if v, ok := c.get("a"); !ok || v != 1 {
		t.Fatal("a was evicted")
	}
}

func word(text string, x float64) pdf.Word {
	return pdf.Word{Text: text, Box: pdf.Rect{X0: x, Y0: 10, X1: x + 10, Y1: 20}}
}

// TestMarks: every copy of an amount on the page is boxed, however it is
// printed, with its label's words; a text field's words in order.
func TestMarks(t *testing.T) {
	pl := &pdf.PageLayout{Number: 1, Words: []pdf.Word{
		word("Ending", 0), word("balance:", 10), word("$714,760.65", 20), word("714,760.65", 30),
		word("(714,760.65)", 40), word("714,760.66", 50), word("Ending", 60), word("note", 70),
		word("Halyard", 80), word("Growth", 90), word("Fund", 100), word("II", 110),
	}}
	f := FieldView{Key: "ending", Page: 1, amount: decimal.MustParse("714760.65"), text: "Ending balance"}
	var values, labels int
	for _, m := range marks(pl, &f) {
		switch m.Kind {
		case "value":
			values++
		case "label":
			labels++
		}
	}
	if values != 3 || labels != 2 {
		t.Fatalf("amount marks: %d values, %d labels", values, labels)
	}
	name := FieldView{Key: "fund", Page: 1, text: "Halyard Growth Fund II"}
	if got := marks(pl, &name); len(got) != 4 || got[0].X != 80 {
		t.Fatalf("text marks %+v", got)
	}
	if got := marks(pl, &FieldView{Key: "x", Page: 1, text: "Growth Partners"}); len(got) != 0 {
		t.Fatalf("a partial phrase was boxed: %+v", got)
	}
}

func TestFields(t *testing.T) {
	r := &statement.Record{
		Investor: statement.Text{Text: "Example Family Trust", Page: 1},
		Fund:     statement.Text{Text: "Halyard Growth Fund II", Page: 1},
		Period:   statement.Period{End: "2026-03-31", Label: "March 31, 2026", Page: 2},
		Ending:   statement.Amount{Label: "Ending balance", Amount: decimal.MustParse("10"), Page: 2},
		Lines:    []statement.Line{{Kind: statement.Movement, Amount: statement.Amount{Label: "Contributions", Amount: decimal.MustParse("5"), Page: 2}}},
	}
	fs := fields(r)
	keys := make([]string, len(fs))
	for i := range fs {
		keys[i] = fs[i].Key
	}
	if got := strings.Join(keys, ","); got != "investor,fund,period,lines.1,ending" {
		t.Fatalf("fields %s", got)
	}
	if !fs[4].IsAmount() || fs[0].IsAmount() {
		t.Fatal("amount detection")
	}
}

// TestParseRecordForm: blank optional amounts are absent, a partly filled
// one is sent for the record's validation to refuse, and a page that is
// not a number is the person's error.
func TestParseRecordForm(t *testing.T) {
	base := url.Values{
		"investor.text": {"Example Family Trust"}, "investor.page": {"1"},
		"fund.text": {"Halyard Growth Fund II"}, "fund.page": {"1"},
		"period.end": {"2026-03-31"}, "period.label": {"March 31, 2026"}, "period.page": {"1"},
		"currency.code": {"usd"}, "currency.evidence": {""}, "currency.page": {""},
		"precision": {"cents"}, "column.label": {"QTD"}, "column.section": {""}, "column.reason": {"the quarter"}, "column.page": {"1"},
		"ending.label": {"Ending balance"}, "ending.amount": {"$714,760.65"}, "ending.page": {"1"},
		"beginning.label": {"Beginning balance"}, "beginning.amount": {"714,765.65"}, "beginning.page": {"1"},
		"fund_total.label": {""}, "fund_total.amount": {""}, "fund_total.page": {""},
		"lines.0.kind": {"movement"}, "lines.0.label": {"Contributions"}, "lines.0.amount": {"(5)"}, "lines.0.page": {"1"},
		"lines.1.kind": {"movement"}, "lines.1.label": {""}, "lines.1.amount": {""}, "lines.1.page": {""},
		"repeats.0.label": {""}, "repeats.0.amount": {""}, "repeats.0.page": {""},
	}
	raw, err := parseRecordForm(base)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := statement.DecodeBytes(raw)
	if err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if rec.FundTotal != nil || rec.Beginning == nil || len(rec.Lines) != 1 || rec.Lines[0].Amount.Amount.String() != "-5" || rec.Currency.Code != "USD" || rec.Ending.Amount.String() != "714760.65" || len(rec.Repeats) != 0 {
		t.Fatalf("record %+v", rec)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if _, ok := m["fund_total"]; ok {
		t.Fatal("a blank fund total was sent")
	}

	partial := url.Values{}
	for k, v := range base {
		partial[k] = v
	}
	partial.Set("fund_total.label", "Total fund")
	raw, err = parseRecordForm(partial)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = statement.DecodeBytes(raw); err == nil {
		t.Fatal("a fund total without an amount decoded")
	}

	many := url.Values{}
	for k, v := range base {
		many[k] = v
	}
	many.Set(fmt.Sprintf("lines.%d.label", maxRows), "too far")
	var tooMany *formError
	if _, err = parseRecordForm(many); !errors.As(err, &tooMany) {
		t.Fatalf("excess rows: %v", err)
	}

	partial.Set("ending.page", "one")
	var fe *formError
	if _, err = parseRecordForm(partial); !errors.As(err, &fe) || !strings.Contains(fe.msg, "page") {
		t.Fatalf("bad page: %v", err)
	}
}

// TestInboxFilters: the inbox lists only what the watch would take (the
// pattern) and shows a file over the host's bound as refused, not queued.
func TestInboxFilters(t *testing.T) {
	dir := t.TempDir()
	for name, size := range map[string]int{"notes.txt": 10, "big.pdf": 2048, ".hidden.pdf": 10} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := New(Options{Token: "0123456789abcdef", Addr: "127.0.0.1:8741"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetBackend(nil, Inbox{Dir: dir, Pattern: "*.pdf", MaxBytes: 1024})
	docs, err := srv.inboxWaiting(context.Background(), nil, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Name != "big.pdf" || docs[0].State != stateFailed || docs[0].Label != "Too large to read" {
		t.Fatalf("inbox %+v", docs)
	}
}

// TestFormFromValues: a draft's values come back with more empty rows.
func TestFormFromValues(t *testing.T) {
	v := url.Values{"investor.text": {"A"}, "lines.0.label": {"x"}, "lines.0.amount": {"1"}, "lines.0.kind": {"subtotal"},
		"lines.1.label": {""}, "repeats.0.label": {"r"}}
	if tooManyRows(v) {
		t.Fatal("a small draft is too many rows")
	}
	v.Set(fmt.Sprintf("repeats.%d.label", maxRows), "too far")
	if !tooManyRows(v) {
		t.Fatal("an oversized draft passes")
	}
	v.Del(fmt.Sprintf("repeats.%d.label", maxRows))
	f := formFromValues(v)
	if f.InvestorText != "A" || len(f.Lines) != 2+blankRows || f.Lines[0].Kind != "subtotal" || f.Lines[0].Amount != "1" || len(f.Repeats) != 1+blankRows {
		t.Fatalf("form %+v", f)
	}
}

// TestServerGuards: without a session nothing is served, an unknown Host
// is refused, and without a backend the pages send the person to setup.
func TestServerGuards(t *testing.T) {
	srv, err := New(Options{Token: "0123456789abcdef", Addr: "127.0.0.1:8741",
		Setup: func(context.Context, SetupInput) (Backend, Inbox, error) { return nil, Inbox{}, errors.New("no") }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path, host string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = host
		for _, c := range cookies {
			req.AddCookie(c)
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w
	}
	if w := get("/", "127.0.0.1:8741"); w.Code != http.StatusUnauthorized {
		t.Fatalf("no session: %d", w.Code)
	}
	if w := get("/", "attacker.example:8741"); w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("foreign host: %d", w.Code)
	}
	if w := get("/api/documents", "localhost:8741"); w.Code != http.StatusUnauthorized {
		t.Fatalf("API without a session: %d", w.Code)
	}
	w := get("/auth/session?token=0123456789abcdef", "localhost:8741")
	if w.Code != http.StatusSeeOther || len(w.Result().Cookies()) != 1 {
		t.Fatalf("sign in: %d", w.Code)
	}
	c := w.Result().Cookies()[0]
	// Named by port: browsers scope cookies by host, not port.
	if c.Name != "addepar_session_8741" || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie %+v", c)
	}
	if w := get("/", "localhost:8741", c); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/setup" {
		t.Fatalf("before setup: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := get("/setup", "localhost:8741", c); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "API key") ||
		!strings.Contains(w.Body.String(), `value="claude-opus-5-5"`) {
		t.Fatalf("setup page (the default model in the page, for no JavaScript): %d", w.Code)
	}
	if _, err := New(Options{Token: "short", Addr: "127.0.0.1:1"}, nil); err == nil {
		t.Fatal("a short token was accepted")
	}
}

func TestPastExportsNewestFirst(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	ex := func(run string, at time.Time) review.Exported {
		return review.Exported{Run: run, At: at, Export: &export.Export{CSVSHA256: strings.Repeat("a", 64)}}
	}
	// Key order (run IDs) is unrelated to time.
	got := pastExports([]review.Exported{
		ex("export-a.1", t0.Add(time.Hour)),
		{Run: "export-b.1", At: t0.Add(3 * time.Hour)}, // no export: skipped
		ex("export-c.1", t0),
		ex("export-d.1", t0.Add(2*time.Hour)),
		ex("export-e.1", t0.Add(2*time.Hour)),
	})
	var runs []string
	for _, e := range got {
		runs = append(runs, e.Run)
	}
	if want := "export-d.1 export-e.1 export-a.1 export-c.1"; strings.Join(runs, " ") != want {
		t.Errorf("pastExports order = %v, want %s", runs, want)
	}
}
