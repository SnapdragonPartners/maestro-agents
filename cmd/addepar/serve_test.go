package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/intake"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/review"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/ui"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
	"github.com/SnapdragonPartners/maestro-agents/standalone"
)

const testToken = "0123456789abcdef0123456789abcdef"

// served is a serve process under test and a browser's client for it.
type served struct {
	t      *testing.T
	base   string
	client *http.Client
	csrf   string
	cancel context.CancelFunc
	done   chan int
	stderr *bytes.Buffer
}

// startServe runs serve on ws until the test stops it, and signs in.
func startServe(t *testing.T, p standalone.Program, ws string, env serveEnv) *served { //nolint:gocritic // hugeParam: tests
	t.Helper()
	links := make(chan string, 1)
	env.ready = func(l string) { links <- l }
	env.open = func(string) error { return nil }
	if env.getenv == nil {
		env.getenv = func(k string) string {
			if k == tokenEnv {
				return testToken
			}
			return ""
		}
	}
	if env.home == nil {
		env.home = func() (string, error) { return t.TempDir(), nil }
	}
	if env.configDir == nil {
		env.configDir = func() (string, error) { return t.TempDir(), nil }
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &served{t: t, cancel: cancel, done: make(chan int, 1), stderr: &bytes.Buffer{}}
	var stdout bytes.Buffer
	go func() {
		s.done <- serveCommand(ctx, p, []string{"-workspace", ws, "-addr", "127.0.0.1:0"}, &stdout, s.stderr, env)
	}()
	var link string
	select {
	case link = <-links:
	case code := <-s.done:
		t.Fatalf("serve exited %d: %s", code, s.stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not start")
	}
	t.Cleanup(func() { s.stop() })
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	s.base = "http://" + u.Host
	jar, _ := cookiejar.New(nil)
	s.client = &http.Client{Jar: jar, Timeout: 30 * time.Second}
	// Without the cookie, nothing opens.
	if resp := s.get("/"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no session: %d", resp.StatusCode)
	}
	// A wrong token opens nothing; the right one signs in.
	if resp := s.get("/auth/session?token=nope"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	// The printed link, its token encoded, signs in.
	resp := s.get(u.RequestURI())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sign in: %d", resp.StatusCode)
	}
	s.csrf = csrfOf(t, resp.body)
	return s
}

func (s *served) stop() int {
	if s.cancel == nil {
		return -1
	}
	s.cancel()
	s.cancel = nil
	select {
	case code := <-s.done:
		return code
	case <-time.After(20 * time.Second):
		s.t.Fatal("serve did not stop")
	}
	return -1
}

type response struct {
	*http.Response
	body string
}

func (s *served) do(req *http.Request) response {
	s.t.Helper()
	resp, err := s.client.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{Response: resp, body: string(b)}
}

func (s *served) get(path string) response {
	s.t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, s.base+path, nil)
	return s.do(req)
}

// post submits a form with the session's CSRF token.
func (s *served) post(path string, form url.Values) response {
	s.t.Helper()
	if form.Get("csrf") == "" {
		form.Set("csrf", s.csrf)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, s.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return s.do(req)
}

// api calls a JSON route with the session's cookie and CSRF header.
func (s *served) api(method, path string, body any, out any) int {
	s.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, s.base+path, r)
	req.Header.Set("X-CSRF-Token", s.csrf)
	resp := s.do(req)
	if out != nil {
		if err := json.Unmarshal([]byte(resp.body), out); err != nil {
			s.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, resp.body)
		}
	}
	return resp.StatusCode
}

var csrfPattern = regexp.MustCompile(`name="csrf" value="([0-9a-f]{64})"`)

func csrfOf(t *testing.T, html string) string {
	t.Helper()
	m := csrfPattern.FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("no CSRF token in the page:\n%s", html)
	}
	return m[1]
}

type apiDocs struct {
	Documents []struct {
		SHA256  string `json:"sha256"`
		Name    string `json:"name"`
		State   string `json:"state"`
		Records []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"records"`
	} `json:"documents"`
}

type apiRecord struct {
	Record struct {
		State string `json:"state"`
	} `json:"record"`
	Current struct {
		Result verify.Result `json:"result"`
	} `json:"current"`
	Blockers []string `json:"blockers"`
	Accepted []string `json:"accepted_warnings"`
	Seen     int      `json:"seen"`
}

// waitDocuments polls the documents until every one has left the queue.
func (s *served) waitDocuments(n int) apiDocs {
	s.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var d apiDocs
		if code := s.api(http.MethodGet, "/api/documents", nil, &d); code != http.StatusOK {
			s.t.Fatalf("documents: %d", code)
		}
		done := 0
		for _, doc := range d.Documents {
			if doc.State != "queued" && doc.State != "reading" {
				done++
			}
		}
		if done == n && len(d.Documents) == n {
			return d
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("documents not read: %+v\n%s", d, s.stderr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// folderConfig writes a workspace configuration for the scripted model.
func writeServeConfig(t *testing.T, ws string) {
	t.Helper()
	w := newWorkspace(ws)
	if err := w.create(); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, w.config, map[string]any{
		"agent_type": intake.TypeName,
		"namespace":  "intake",
		"store":      map[string]any{"kind": "file", "dir": "store"},
		"trigger":    map[string]any{"kind": "folder", "dir": "../Inbox", "poll_interval": "100ms"},
		"model":      map[string]any{"provider": "fake", "name": "scripted"},
		"log":        map[string]any{"level": "error"},
	})
}

// TestServe drives the UI as an operator would (ADR 0022): two statements
// dropped into the inbox, one reviewed and approved through the pages,
// one entered by hand through the JSON routes, both exported; and the
// request checks.
func TestServe(t *testing.T) {
	requirePoppler(t)
	if runtime.GOOS == "windows" {
		t.Skip("the file store does not support Windows")
	}
	ws := filepath.Join(t.TempDir(), "Addepar Intake")
	writeServeConfig(t, ws)
	w := newWorkspace(ws)
	stage := t.TempDir()
	halyard, halyardRec := synthStatement(t, stage, "halyard-ii")
	keelson, keelsonRec := synthStatement(t, stage, "keelson-vi")
	args, err := json.Marshal(map[string]any{"records": []json.RawMessage{halyardRec}})
	if err != nil {
		t.Fatal(err)
	}
	p := scriptedProgram(map[string]llms.ChatResponse{
		"Halyard Growth Fund II": toolCall("2", "submit_extraction", string(args)),
		"Keelson Partners VI":    toolCall("2", "request_manual_review", `{"reason": "the columns are unclear"}`),
	})
	s := startServe(t, p, ws, serveEnv{})

	// The name comes first.
	if resp := s.get("/"); !strings.Contains(resp.body, "Who is reviewing?") {
		t.Fatalf("no name form: %s", resp.body)
	}
	if resp := s.post("/name", url.Values{"name": {"  "}, "next": {"/"}}); !strings.Contains(resp.body, "Type your name") {
		t.Fatalf("a blank name was taken: %s", resp.body)
	}
	if resp := s.post("/name", url.Values{"name": {"Jordan Lee"}, "next": {"//evil.example/"}}); resp.Request.URL.Host != strings.TrimPrefix(s.base, "http://") || !strings.Contains(resp.body, "No statements yet") {
		t.Fatalf("after the name: %s %s", resp.Request.URL, resp.body)
	}

	for _, name := range []string{"halyard-ii", "keelson-vi"} {
		b, err := os.ReadFile(filepath.Join(stage, name+".pdf"))
		if err != nil {
			t.Fatal(err)
		}
		// Stage as a dotfile and rename in, as an operator's copy should
		// land: the watch skips dotfiles, and the rename is atomic, so it
		// never sees a half-written file under load.
		tmp := filepath.Join(w.inbox, "."+name+".tmp")
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(w.inbox, name+".pdf")); err != nil {
			t.Fatal(err)
		}
	}
	docs := s.waitDocuments(2)
	states := map[string]string{}
	for _, d := range docs.Documents {
		states[d.SHA256] = d.State
	}
	if states[halyard] != intake.DispositionVerified || states[keelson] != intake.DispositionManual {
		t.Fatalf("documents %+v\n%s", docs, s.stderr.String())
	}
	page := s.get("/")
	for _, want := range []string{"halyard-ii.pdf", "Needs manual entry", "Needs review", "/records/" + halyard + "/01"} {
		if !strings.Contains(page.body, want) {
			t.Fatalf("documents page lacks %q:\n%s", want, page.body)
		}
	}

	// The extracted record: its evidence on the page, its warning
	// accepted through the page's form, then approved.
	hy := intake.RecordID(halyard, 1)
	rp := s.get("/records/" + hy)
	for _, want := range []string{"Halyard Growth Fund II", `class="mark mark-value" data-field="ending"`, "/documents/" + halyard + "/pages/1", "Before it can be approved"} {
		if !strings.Contains(rp.body, want) {
			t.Fatalf("record page lacks %q:\n%s", want, rp.body)
		}
	}
	img := s.get("/documents/" + halyard + "/pages/1")
	if img.StatusCode != http.StatusOK || img.Header.Get("Content-Type") != "image/png" || !strings.HasPrefix(img.body, "\x89PNG") {
		t.Fatalf("page image: %d %s", img.StatusCode, img.Header.Get("Content-Type"))
	}
	var rec apiRecord
	s.api(http.MethodGet, "/api/records/"+hy, nil, &rec)
	warnings := rec.Current.Result.Findings(verify.Warning)
	if rec.Record.State != "needs_review" || len(warnings) != 1 || rec.Seen != 0 {
		t.Fatalf("extracted %+v", rec)
	}
	act := func(kind string, extra url.Values) response {
		extra.Set("kind", kind)
		if extra.Get("seen") == "" {
			var cur apiRecord
			s.api(http.MethodGet, "/api/records/"+hy, nil, &cur)
			extra.Set("seen", fmt.Sprint(cur.Seen))
		}
		return s.post("/records/"+hy+"/actions", extra)
	}
	if resp := act("accept_warning", url.Values{"finding": {warnings[0].ID()}, "reason": {"USD per the cover letter"}, "seen": {"5"}}); !strings.Contains(resp.body, "reload it") {
		t.Fatalf("stale action not refused: %s", resp.body)
	}
	if resp := act("accept_warning", url.Values{"finding": {warnings[0].ID()}, "reason": {"USD per the cover letter"}}); !strings.Contains(resp.body, "Warning accepted") {
		t.Fatalf("accept: %s", resp.body)
	}
	if resp := act("set_entities", url.Values{"owner.id": {" 1 "}, "owner.name": {"Example Family Trust"}, "asset.id": {"30"}, "reason": {"from Addepar"}}); !strings.Contains(resp.body, "Owner and asset saved") {
		t.Fatalf("set entities: %s", resp.body)
	}
	if resp := act("approve", url.Values{}); !strings.Contains(resp.body, "Approved.") || !strings.Contains(resp.body, "Approved by <strong>Jordan Lee</strong>") {
		t.Fatalf("approve: %s", resp.body)
	}
	// A correction through the form: the same values with a wrong ending
	// fail the checks, and void the approval.
	var form url.Values
	{
		cur := s.get("/records/" + hy)
		form = formValues(t, cur.body, "correct")
	}
	form.Set("ending.amount", "714,760.66")
	form.Set("reason", "typo")
	if resp := s.post("/records/"+hy+"/actions", form); !strings.Contains(resp.body, "Correction saved") {
		t.Fatalf("correct: %s", resp.body)
	}
	s.api(http.MethodGet, "/api/records/"+hy, nil, &rec)
	if rec.Record.State != "needs_review" || rec.Current.Result.Verdict != verify.Fail {
		t.Fatalf("after a wrong correction %+v", rec)
	}
	// The voided approval stays in the history: the log, not the fold.
	if hist := s.get("/records/" + hy).body; !strings.Contains(hist, "<strong>Jordan Lee</strong> approve") || strings.Contains(hist, "Approved by") {
		t.Fatalf("history after a voiding correction:\n%s", hist)
	}
	form = formValues(t, s.get("/records/"+hy).body, "correct")
	form.Set("ending.amount", "714760.65")
	form.Set("reason", "back to the printed value")
	// On a stale view the draft comes back with the record's current
	// length, so saving it again, after checking, works.
	stale := url.Values{}
	for k, v := range form {
		stale[k] = v
	}
	stale.Set("seen", "1")
	again := s.post("/records/"+hy+"/actions", stale)
	if !strings.Contains(again.body, "changed while you were editing") || !strings.Contains(again.body, `value="back to the printed value"`) {
		t.Fatalf("stale correction:\n%s", again.body)
	}
	form = formValues(t, again.body, "correct")
	if form.Get("ending.amount") != "714760.65" || form.Get("seen") == "1" {
		t.Fatalf("the kept draft %v", form)
	}
	if resp := s.post("/records/"+hy+"/actions", form); !strings.Contains(resp.body, "Correction saved") {
		t.Fatalf("correct back: %s", resp.body)
	}
	s.api(http.MethodGet, "/api/records/"+hy, nil, &rec)
	for _, f := range rec.Current.Result.Findings(verify.Warning) {
		act("accept_warning", url.Values{"finding": {f.ID()}, "reason": {"checked"}})
	}
	if resp := act("approve", url.Values{}); !strings.Contains(resp.body, "Approved.") {
		t.Fatalf("approve again: %s", resp.body)
	}

	// The statement sent to manual review, entered through the JSON routes.
	var out map[string]any
	if code := s.api(http.MethodPost, "/api/documents/"+keelson+"/manual", map[string]any{"kind": "manual", "seen": 0, "extraction": keelsonRec, "reason": "read off page 1"}, &out); code != http.StatusOK {
		t.Fatalf("manual: %d %v", code, out)
	}
	ke := intake.RecordID(keelson, 1)
	var nf map[string]any
	// A manual entry for a well-formed but unknown document is a 404 (a
	// no_document refusal), never a failed run (buggrit f-6424dd).
	if code := s.api(http.MethodPost, "/api/documents/"+strings.Repeat("0", 64)+"/manual", map[string]any{"kind": "manual", "seen": 0, "extraction": keelsonRec, "reason": "x"}, &nf); code != http.StatusNotFound {
		t.Fatalf("manual for an unknown document: %d %v", code, nf)
	}
	// A page the PDF does not have is a 404, not a failure.
	if pg := s.get("/documents/" + halyard + "/pages/99"); pg.StatusCode != http.StatusNotFound {
		t.Fatalf("page past the end: %d", pg.StatusCode)
	}
	// An unknown record is 404 on a POST too, as on a GET.
	if code := s.api(http.MethodPost, "/api/records/"+keelson+"/09/actions", map[string]any{"kind": "approve", "seen": 0}, &nf); code != http.StatusNotFound {
		t.Fatalf("POST to an unknown record: %d %v", code, nf)
	}
	// More rows for a manual draft keeps what was typed (no JavaScript).
	draft := s.post("/documents/"+keelson+"/manual", url.Values{
		"seen": {"1"}, "more": {"1"}, "investor.text": {"Typed Investor"},
		"lines.0.label": {"Contributions"}, "lines.0.amount": {"5"}, "lines.0.kind": {"movement"},
		"lines.1.label": {""}, "lines.2.label": {""},
	})
	if !strings.Contains(draft.body, `value="Typed Investor"`) || !strings.Contains(draft.body, `name="lines.5.label"`) || !strings.Contains(draft.body, `value="Contributions"`) {
		t.Fatalf("more rows:\n%s", draft.body)
	}
	// A manual entry that fails (a page that is not a number) keeps the
	// draft and says why (buggrit f-94c4c4).
	bad := s.post("/documents/"+keelson+"/manual", url.Values{
		"seen": {"1"}, "investor.text": {"Typed Investor"}, "investor.page": {"one"}, "reason": {"x"},
		"lines.0.label": {""},
	})
	if !strings.Contains(bad.body, `value="Typed Investor"`) || !strings.Contains(bad.body, "must be a page number") {
		t.Fatalf("failed manual entry:\n%s", bad.body)
	}
	bad = s.post("/documents/"+keelson+"/manual", url.Values{
		"seen": {"1"}, "investor.text": {"Typed Investor"}, "investor.page": {"one"}, "reason": {"read off page 1"},
		"lines.0.label": {""},
	})
	if !strings.Contains(bad.body, `value="read off page 1"`) {
		t.Fatalf("the note was not kept:\n%s", bad.body)
	}
	// An oversized submission is refused, never shown truncated.
	huge := url.Values{"seen": {"1"}, "investor.text": {"Typed Investor"}, "reason": {"x"}}
	for i := 0; i <= ui.MaxRows; i++ {
		huge.Set(fmt.Sprintf("lines.%d.label", i), fmt.Sprint("row ", i))
	}
	if over := s.post("/documents/"+keelson+"/manual", huge); !strings.Contains(over.body, "more than") || strings.Contains(over.body, `value="Typed Investor"`) {
		t.Fatalf("oversized submission: %s", over.body[:min(len(over.body), 2000)])
	}
	var apiErr struct {
		Error   string          `json:"error"`
		Refusal *review.Refusal `json:"refusal"`
	}
	if code := s.api(http.MethodPost, "/api/records/"+ke+"/actions", map[string]any{"kind": "approve", "seen": 0}, &apiErr); code != http.StatusConflict || apiErr.Refusal == nil || apiErr.Refusal.Code != review.RefuseStale {
		t.Fatalf("stale approve: %d %+v", code, apiErr)
	}
	s.api(http.MethodGet, "/api/records/"+ke, nil, &rec)
	for _, f := range rec.Current.Result.Findings(verify.Warning) {
		var cur apiRecord
		s.api(http.MethodGet, "/api/records/"+ke, nil, &cur)
		if code := s.api(http.MethodPost, "/api/records/"+ke+"/actions", map[string]any{"kind": "accept_warning", "seen": cur.Seen, "finding": f.ID(), "reason": "checked against the PDF"}, &out); code != http.StatusOK {
			t.Fatalf("accept %s: %d %v", f.ID(), code, out)
		}
	}
	s.api(http.MethodGet, "/api/records/"+ke, nil, &rec)
	if code := s.api(http.MethodPost, "/api/records/"+ke+"/actions", map[string]any{"kind": "approve", "seen": rec.Seen}, &out); code != http.StatusOK {
		t.Fatalf("approve manual: %d %v", code, out)
	}

	// Export both from the page: the CSV in Exports/, the manifest hidden.
	ex := s.get("/exports")
	if !strings.Contains(ex.body, hy) || !strings.Contains(ex.body, ke) {
		t.Fatalf("exports page: %s", ex.body)
	}
	if resp := s.post("/exports", url.Values{"record": {hy, ke}}); !strings.Contains(resp.body, "Exported 2 record(s)") {
		t.Fatalf("export: %s", resp.body)
	}
	csvs, _ := filepath.Glob(filepath.Join(w.exports, "addepar-*.csv"))
	mans, _ := filepath.Glob(filepath.Join(w.manifests, "*.json"))
	if len(csvs) != 1 || len(mans) != 1 {
		t.Fatalf("files: %v %v", csvs, mans)
	}
	var exps struct {
		Waiting []any `json:"waiting"`
		Exports []struct {
			Run string `json:"run"`
		} `json:"exports"`
	}
	s.api(http.MethodGet, "/api/exports", nil, &exps)
	if len(exps.Waiting) != 0 || len(exps.Exports) != 1 {
		t.Fatalf("exports %+v", exps)
	}
	csvBytes, _ := os.ReadFile(csvs[0])
	if dl := s.get("/exports/" + exps.Exports[0].Run + "/csv"); dl.body != string(csvBytes) {
		t.Fatal("the downloaded CSV differs from the written one")
	}
	if err := os.Remove(csvs[0]); err != nil {
		t.Fatal(err)
	}
	if resp := s.post("/exports/"+exps.Exports[0].Run+"/rewrite", url.Values{}); !strings.Contains(resp.body, "Written:") {
		t.Fatalf("rewrite: %s", resp.body)
	}
	if again, _ := os.ReadFile(csvs[0]); !bytes.Equal(again, csvBytes) {
		t.Fatal("the rewritten CSV differs")
	}

	// The request checks (ADR 0022, item 8).
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, s.base+"/", nil)
	req.Host = "evil.example:80"
	if resp := s.do(req); resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("foreign Host: %d", resp.StatusCode)
	}
	if resp := s.post("/records/"+hy+"/actions", url.Values{"csrf": {"0"}, "kind": {"approve"}, "seen": {"0"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong CSRF: %d", resp.StatusCode)
	}
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodPost, s.base+"/exports", strings.NewReader(url.Values{"csrf": {s.csrf}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.example")
	if resp := s.do(req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST: %d", resp.StatusCode)
	}
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodPost, s.base+"/api/exports", strings.NewReader(`{"records":["`+hy+`"]}`))
	if resp := s.do(req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("API POST without the CSRF header: %d", resp.StatusCode)
	}
	bearer := &http.Client{Timeout: 10 * time.Second}
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, s.base+"/api/documents", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	if resp, err := bearer.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodPost, s.base+"/api/records/"+hy+"/actions", strings.NewReader(`{"kind":"approve","seen":0}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	if resp, err := bearer.Do(req); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bearer action without by: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}
	// A malformed record ID is a bad request, not a refusal (ADR 0022,
	// item 13).
	for _, body := range []string{`{"records":["nope"],"by":"Q"}`, `{"records":[],"by":"Q"}`} {
		req, _ = http.NewRequestWithContext(context.Background(), http.MethodPost, s.base+"/api/exports", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testToken)
		if resp, err := bearer.Do(req); err != nil || resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("API export %s: %v %v", body, resp, err)
		} else {
			resp.Body.Close()
		}
	}
	// Under no-referrer a browser sends "Origin: null" on its own form
	// posts, which the Origin check refuses (found in manual QA).
	if got := page.Header.Get("Referrer-Policy"); got != "same-origin" {
		t.Fatalf("Referrer-Policy %q", got)
	}
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options"} {
		if page.Header.Get(h) == "" {
			t.Fatalf("no %s header", h)
		}
	}
	if code := s.stop(); code != exitOK {
		t.Fatalf("serve exited %d: %s", code, s.stderr.String())
	}
}

var inputPattern = regexp.MustCompile(`<input name="([a-z_.0-9]+)" value="([^"]*)"`)
var selectedPattern = regexp.MustCompile(`<select name="([a-z_.0-9]+)">(?:<option value="[^"]*">[^<]*</option>)*<option value="([^"]*)" selected>`)

// formValues reads the record form of the action kind from a page.
func formValues(t *testing.T, html, kind string) url.Values {
	t.Helper()
	i := strings.Index(html, `name="kind" value="`+kind+`"`)
	if i < 0 {
		t.Fatalf("no %s form", kind)
	}
	end := strings.Index(html[i:], "</form>")
	part := html[i : i+end]
	v := url.Values{"kind": {kind}}
	for _, m := range inputPattern.FindAllStringSubmatch(part, -1) {
		v.Set(m[1], unescape(m[2]))
	}
	for _, m := range selectedPattern.FindAllStringSubmatch(part, -1) {
		v.Set(m[1], m[2])
	}
	start := strings.LastIndex(html[:i], `name="seen" value="`)
	seen := html[start+len(`name="seen" value="`):]
	v.Set("seen", seen[:strings.Index(seen, `"`)])
	return v
}

func unescape(s string) string {
	return strings.NewReplacer("&amp;", "&", "&#34;", `"`, "&#39;", "'", "&lt;", "<", "&gt;", ">", "&quot;", `"`).Replace(s)
}

// TestServeSetup: a workspace without a configuration opens on setup,
// which writes the configuration and the key outside the workspace and
// starts the agent without a restart (ADR 0022, items 4 and 5).
func TestServeSetup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the file store does not support Windows")
	}
	ws := filepath.Join(t.TempDir(), "ws")
	cfgDir := t.TempDir()
	s := startServe(t, program(nil), ws, serveEnv{configDir: func() (string, error) { return cfgDir, nil }})
	if resp := s.get("/"); !strings.Contains(resp.body, "Set up Addepar Intake") {
		t.Fatalf("not on setup: %s", resp.body)
	}
	if resp := s.post("/setup", url.Values{"provider": {"nope"}, "model": {"m"}, "key": {""}}); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(resp.body, "Paste the API key") {
		t.Fatalf("bad setup: %d %s", resp.StatusCode, resp.body)
	}
	if _, err := os.Stat(newWorkspace(ws).config); err == nil {
		t.Fatal("a refused setup wrote a configuration")
	}
	resp := s.post("/setup", url.Values{"provider": {"anthropic"}, "model": {"claude-opus-5-5"}, "key": {"sk-test-key"}})
	if !strings.Contains(resp.body, "Who is reviewing?") {
		t.Fatalf("after setup: %s", resp.body)
	}
	cfg, err := standalone.LoadConfig(newWorkspace(ws).config)
	if err != nil {
		t.Fatal(err)
	}
	secrets := filepath.Join(cfgDir, "addepar", cfg.Model.Credential+".json")
	if cfg.Trigger.Kind != "folder" || cfg.Trigger.Dir != newWorkspace(ws).inbox || cfg.Credentials.File != secrets || cfg.Model.Provider != "anthropic" {
		t.Fatalf("config %+v", cfg)
	}
	fi, err := os.Stat(secrets)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("secrets file: %v %v", fi, err)
	}
	b, _ := os.ReadFile(secrets)
	if !strings.HasPrefix(cfg.Model.Credential, "workspace-") || !strings.Contains(string(b), `"`+cfg.Model.Credential+`": "sk-test-key"`) {
		t.Fatalf("credential %q, secrets %s", cfg.Model.Credential, b)
	}
	if strings.Contains(readAll(t, ws), "sk-test-key") {
		t.Fatal("the key is in the workspace")
	}
	s.post("/name", url.Values{"name": {"Jordan Lee"}, "next": {"/"}})
	if resp := s.get("/"); !strings.Contains(resp.body, "No statements yet") {
		t.Fatalf("documents after setup: %s", resp.body)
	}
	if resp := s.post("/setup", url.Values{"provider": {"openai"}, "model": {"gpt-5"}, "key": {"other"}}); !strings.Contains(resp.body, "No statements yet") {
		t.Fatalf("setup again: %s", resp.body)
	}
	if code := s.stop(); code != exitOK {
		t.Fatalf("serve exited %d: %s", code, s.stderr.String())
	}
	// The next start opens the configured workspace directly.
	s = startServe(t, program(nil), ws, serveEnv{configDir: func() (string, error) { return cfgDir, nil }})
	s.post("/name", url.Values{"name": {"Jordan Lee"}, "next": {"/"}})
	if resp := s.get("/"); !strings.Contains(resp.body, "No statements yet") {
		t.Fatalf("restart: %s", resp.body)
	}
	s.stop()

	// A second workspace gets its own credential and secrets file: the
	// first workspace's key is kept.
	ws2 := filepath.Join(t.TempDir(), "ws2")
	s = startServe(t, program(nil), ws2, serveEnv{configDir: func() (string, error) { return cfgDir, nil }})
	if resp := s.post("/setup", url.Values{"provider": {"openai"}, "model": {"gpt-5"}, "key": {"sk-second-key"}}); !strings.Contains(resp.body, "Who is reviewing?") {
		t.Fatalf("second setup: %s", resp.body)
	}
	s.stop()
	cfg2, err := standalone.LoadConfig(newWorkspace(ws2).config)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Model.Credential == cfg.Model.Credential || cfg2.Credentials.File == cfg.Credentials.File ||
		secretOf(t, cfg.Credentials.File, cfg.Model.Credential) != "sk-test-key" || secretOf(t, cfg2.Credentials.File, cfg2.Model.Credential) != "sk-second-key" {
		t.Fatalf("credentials %q %q", cfg.Model.Credential, cfg2.Model.Credential)
	}
}

func secretOf(t *testing.T, path, name string) string {
	t.Helper()
	var vals map[string]string
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &vals); err != nil {
		t.Fatal(err)
	}
	return vals[name]
}

// TestSetupConcurrent: setups of many workspaces at once keep every key,
// and concurrent setups of one workspace leave exactly one configuration,
// whose key is its own (ADR 0022, item 5).
func TestSetupConcurrent(t *testing.T) {
	cfgDir := t.TempDir()
	env := serveEnv{configDir: func() (string, error) { return cfgDir, nil }}
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	wss := make([]workspace, n)
	for i := range n {
		wss[i] = newWorkspace(filepath.Join(t.TempDir(), fmt.Sprint("ws", i)))
		if err := wss[i].create(); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := &server{ws: wss[i], env: env}
			errs[i] = s.setup(ui.SetupInput{Provider: "openai", Model: "gpt-5", Key: fmt.Sprint("key-", i)})
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("setup %d: %v", i, errs[i])
		}
		cfg, err := standalone.LoadConfig(wss[i].config)
		if err != nil {
			t.Fatal(err)
		}
		if got := secretOf(t, cfg.Credentials.File, cfg.Model.Credential); got != fmt.Sprint("key-", i) {
			t.Fatalf("workspace %d holds %q", i, got)
		}
	}

	one := newWorkspace(filepath.Join(t.TempDir(), "one"))
	if err := one.create(); err != nil {
		t.Fatal(err)
	}
	won := make(chan int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := &server{ws: one, env: env}
			if s.setup(ui.SetupInput{Provider: "openai", Model: "gpt-5", Key: fmt.Sprint("one-", i)}) == nil {
				won <- i
			}
		}()
	}
	wg.Wait()
	close(won)
	var winners []int
	for i := range won {
		winners = append(winners, i)
	}
	if len(winners) != 1 {
		t.Fatalf("%d setups of one workspace succeeded", len(winners))
	}
	cfg, err := standalone.LoadConfig(one.config)
	if err != nil {
		t.Fatal(err)
	}
	if got := secretOf(t, cfg.Credentials.File, cfg.Model.Credential); got != fmt.Sprint("one-", winners[0]) {
		t.Fatalf("the configuration's key is %q, its setup wrote one-%d", got, winners[0])
	}
}

// TestServeTokenEncoded: a session token with characters a URL query
// changes ('+', '&', '#') still signs in from the printed link.
func TestServeTokenEncoded(t *testing.T) {
	tok := "abc+def&ghi#jkl/mno=0123456789"
	s := startServe(t, program(nil), filepath.Join(t.TempDir(), "ws"), serveEnv{getenv: func(k string) string {
		if k == tokenEnv {
			return tok
		}
		return ""
	}})
	if resp := s.get("/"); !strings.Contains(resp.body, "Set up Addepar Intake") {
		t.Fatalf("not signed in: %s", resp.body)
	}
}

// TestServeCancelDuringOpen: a signal that ends the watch before serve
// waits on it is a clean exit, not a failed watch.
func TestServeCancelDuringOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the file store does not support Windows")
	}
	ws := filepath.Join(t.TempDir(), "ws")
	writeServeConfig(t, ws)
	ctx, cancel := context.WithCancel(context.Background())
	env := defaultServeEnv()
	env.getenv = func(string) string { return testToken }
	env.open = func(string) error {
		cancel()
		time.Sleep(300 * time.Millisecond) // the watch ends meanwhile
		return nil
	}
	var out, errb bytes.Buffer
	if code := serveCommand(ctx, scriptedProgram(nil), []string{"-workspace", ws, "-addr", "127.0.0.1:0"}, &out, &errb, env); code != exitOK {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

// TestServeSignalKeepsRequests: a request in flight at the signal keeps a
// live context through the grace period and completes, while the watch
// stops (buggrit f-9cf446).
func TestServeSignalKeepsRequests(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the file store does not support Windows")
	}
	ws := filepath.Join(t.TempDir(), "ws")
	writeServeConfig(t, ws)
	ctx, cancel := context.WithCancel(context.Background())
	inside, release := make(chan struct{}), make(chan struct{})
	var ctxErr error
	links := make(chan string, 1)
	env := defaultServeEnv()
	env.getenv = func(string) string { return testToken }
	env.open = func(string) error { return nil }
	env.ready = func(l string) { links <- l }
	env.wrap = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/slow" {
				close(inside)
				<-release
				ctxErr = r.Context().Err()
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	done := make(chan int, 1)
	var out, errb bytes.Buffer
	go func() {
		done <- serveCommand(ctx, scriptedProgram(nil), []string{"-workspace", ws, "-addr", "127.0.0.1:0"}, &out, &errb, env)
	}()
	u, _ := url.Parse(<-links)
	got := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+u.Host+"/slow", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			got <- 0
			return
		}
		resp.Body.Close()
		got <- resp.StatusCode
	}()
	<-inside
	cancel() // the signal
	time.Sleep(200 * time.Millisecond)
	close(release)
	if code := <-got; code != http.StatusNoContent || ctxErr != nil {
		t.Fatalf("request at the signal: %d, context %v", code, ctxErr)
	}
	if code := <-done; code != exitOK {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

// failingListener accepts nothing: Serve fails at once, a permanent error.
type failingListener struct{ net.Listener }

func (failingListener) Accept() (net.Conn, error) { return nil, errors.New("listener broken") }

// TestServeServerFailureStopsWatch: when the HTTP server fails on its own,
// serve stops the watch and returns, without waiting for a signal.
func TestServeServerFailureStopsWatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the file store does not support Windows")
	}
	ws := filepath.Join(t.TempDir(), "ws")
	writeServeConfig(t, ws)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	env := defaultServeEnv()
	env.open = func(string) error { return nil }
	var errb bytes.Buffer
	s := &server{p: scriptedProgram(nil), ws: newWorkspace(ws), stderr: &errb, env: env, watchDone: make(chan int, 1), grace: time.Second}
	done := make(chan int, 1)
	go func() { done <- s.run(context.Background(), failingListener{ln}, testToken, false, io.Discard) }()
	select {
	case code := <-done:
		if code != exitFail || !strings.Contains(errb.String(), "the server stopped") {
			t.Fatalf("exit %d: %s", code, errb.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve hung after its server failed")
	}
}

// TestServeShutdownJoinsHandlers: a request still running after the grace
// period has its context cancelled and is joined before shutdown returns,
// so it never outlives the host.
func TestServeShutdownJoinsHandlers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started, finished := make(chan struct{}), make(chan struct{})
	g := newGate()
	hs := &http.Server{
		Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			if !g.enter() {
				return
			}
			defer g.leave()
			close(started)
			<-r.Context().Done()
			time.Sleep(100 * time.Millisecond)
			close(finished)
		}),
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = hs.Serve(ln) }()
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/") //nolint:noctx // test
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	s := &server{grace: 50 * time.Millisecond}
	s.shutdown(context.Background(), hs, cancel, g)
	select {
	case <-finished:
	default:
		t.Fatal("shutdown returned before the handler finished")
	}
	if g.enter() {
		t.Fatal("the gate admits a request after shutdown")
	}
}

// readAll concatenates every regular file under dir.
func readAll(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			c, _ := os.ReadFile(p)
			b.Write(c)
		}
		return nil
	})
	return b.String()
}

func TestServeUsage(t *testing.T) {
	ws := t.TempDir()
	env := defaultServeEnv()
	env.getenv = func(string) string { return "" }
	for _, args := range [][]string{
		{"-workspace", ws, "-addr", "0.0.0.0:0"},
		{"-workspace", ws, "-addr", "example.com:80"},
		{"-workspace", ws, "extra"},
		{"-nope"},
	} {
		var out, errb bytes.Buffer
		if code := serveCommand(context.Background(), program(nil), args, &out, &errb, env); code != exitUsage {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
	env.goos = "windows"
	var wout, werr bytes.Buffer
	if code := serveCommand(context.Background(), program(nil), []string{"-workspace", ws, "-addr", "127.0.0.1:0"}, &wout, &werr, env); code != exitUsage || !strings.Contains(werr.String(), "Windows") {
		t.Fatalf("windows: %d %s", code, werr.String())
	}
	env.goos = ""
	env.getenv = func(string) string { return "short" }
	var out, errb bytes.Buffer
	if code := serveCommand(context.Background(), program(nil), []string{"-workspace", ws, "-addr", "127.0.0.1:0"}, &out, &errb, env); code != exitUsage || !strings.Contains(errb.String(), tokenEnv) {
		t.Fatalf("short token: %d %s", code, errb.String())
	}
}
