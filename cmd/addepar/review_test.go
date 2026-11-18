package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/document"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/export"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/intake"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/review"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
	"github.com/SnapdragonPartners/maestro-agents/standalone"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// reviewer submits review actions on an opened host, as the UI will.
type reviewer struct {
	t   *testing.T
	h   *standalone.Host
	now time.Time
}

// act submits the action on instance and returns the refusal's code, ""
// when the action applied.
func (r *reviewer) act(instance string, a review.Action) string {
	r.t.Helper()
	r.now = r.now.Add(time.Minute)
	if a.At.IsZero() {
		a.At = r.now
	}
	if a.By == "" {
		a.By = "Jordan Lee"
	}
	payload, err := json.Marshal(a)
	if err != nil {
		r.t.Fatal(err)
	}
	rep, err := r.h.Submit(context.Background(), standalone.Submission{AgentType: review.TypeName, Instance: instance, Payload: payload})
	if err != nil {
		r.t.Fatalf("submit %s: %v", a.Kind, err)
	}
	if rep.Status != store.StatusSucceeded {
		r.t.Fatalf("%s run %+v", a.Kind, rep)
	}
	if rep.Reason.Code == agent.ReasonChanged {
		return ""
	}
	o, ok, err := r.h.Store().Object(context.Background(), r.h.Namespace(), review.RefusalKey(rep.Run))
	if err != nil || !ok {
		r.t.Fatalf("%s refused without a refusal: %v", a.Kind, err)
	}
	var ref review.Refusal
	if err := json.Unmarshal(o.JSON, &ref); err != nil {
		r.t.Fatal(err)
	}
	if ref.By != a.By || ref.Kind != a.Kind || ref.Message == "" {
		r.t.Fatalf("refusal %+v", ref)
	}
	return ref.Code
}

func (r *reviewer) state(id string) review.State {
	r.t.Helper()
	s, err := review.Load(context.Background(), review.StoreObjects{Reader: r.h.Store(), Namespace: r.h.Namespace()}, id)
	if err != nil {
		r.t.Fatalf("load %s: %v", id, err)
	}
	return s
}

// acceptAll accepts every current warning of the record.
func (r *reviewer) acceptAll(id string) {
	r.t.Helper()
	s := r.state(id)
	for _, f := range s.Record.Result.Findings(verify.Warning) {
		if code := r.act(review.DocumentInstance(id[:64]), review.Action{Kind: review.AcceptWarning, Record: id, Seen: s.Entries, Finding: f.ID(), Reason: "checked against the PDF"}); code != "" {
			r.t.Fatalf("accept %s: %s", f.ID(), code)
		}
		s = r.state(id)
	}
}

func withEnding(t *testing.T, rec json.RawMessage, amount string) json.RawMessage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec, &m); err != nil {
		t.Fatal(err)
	}
	ending, ok := m["ending"].(map[string]any)
	if !ok {
		t.Fatal("the record has no ending")
	}
	ending["amount"] = amount
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestReviewAndExport takes two statements from intake through review to
// the CSV (ADR 0021): one extracted with a warning to accept, corrected
// wrongly and back; one sent to manual review and entered by hand with a
// slip the checks flag; then export, re-export and a voided approval.
func TestReviewAndExport(t *testing.T) {
	requirePoppler(t)
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	for _, d := range []string{inbox, filepath.Join(dir, "Exports")} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	halyard, halyardRec := synthStatement(t, inbox, "halyard-ii")
	keelson, keelsonRec := synthStatement(t, inbox, "keelson-vi")
	args, err := json.Marshal(map[string]any{"records": []json.RawMessage{halyardRec}})
	if err != nil {
		t.Fatal(err)
	}
	p := scriptedProgram(map[string]llms.ChatResponse{
		"Halyard Growth Fund II": toolCall("2", "submit_extraction", string(args)),
		"Keelson Partners VI":    toolCall("2", "request_manual_review", `{"reason": "the columns are unclear"}`),
	})
	cfg := scoreConfig(t, dir, "state")
	h, err := standalone.Open(context.Background(), p, cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"halyard-ii", "keelson-vi"} {
		if rep, rerr := h.RunInput(context.Background(), filepath.Join(inbox, name+".pdf")); rerr != nil || rep.Status != store.StatusSucceeded {
			t.Fatalf("intake %s: %+v %v", name, rep, rerr)
		}
	}
	r := &reviewer{t: t, h: h, now: time.Date(2026, 4, 14, 15, 0, 0, 0, time.UTC)}
	hy, hyInst := intake.RecordID(halyard, 1), review.DocumentInstance(halyard)
	keInst := review.DocumentInstance(keelson)

	// The extracted record: its currency warning blocks approval until
	// accepted, and an action on a stale view is refused.
	s := r.state(hy)
	if s.Entries != 0 || s.Approved() || len(s.Record.Result.Findings(verify.Warning)) != 1 {
		t.Fatalf("extracted state %+v", s)
	}
	cur := s.Record.Result.Findings(verify.Warning)[0].ID()
	steps := []struct {
		inst string
		a    review.Action
		want string
	}{
		{hyInst, review.Action{Kind: review.Approve, Record: hy, Seen: 0}, review.RefuseNotApprovable},
		{hyInst, review.Action{Kind: review.AcceptWarning, Record: hy, Seen: 0, Finding: "9/nothing", Reason: "x"}, review.RefuseNoSuchWarning},
		{hyInst, review.Action{Kind: review.AcceptWarning, Record: hy, Seen: 0, Finding: cur}, review.RefuseIncomplete},
		{hyInst, review.Action{Kind: review.AcceptWarning, Record: hy, Seen: 0, Finding: cur, Reason: "USD per the cover letter"}, ""},
		{hyInst, review.Action{Kind: review.AcceptWarning, Record: hy, Seen: 0, Finding: cur, Reason: "again"}, review.RefuseStale},
		{hyInst, review.Action{Kind: review.AcceptWarning, Record: hy, Seen: 1, Finding: cur, Reason: "again"}, review.RefuseAccepted},
		{keInst, review.Action{Kind: review.AcceptWarning, Record: hy, Seen: 1, Finding: cur, Reason: "x"}, review.RefuseWrongInstance},
		{hyInst, review.Action{Kind: review.Correct, Record: hy, Seen: 1, Extraction: halyardRec, Reason: "no-op"}, review.RefuseNoChange},
		{hyInst, review.Action{Kind: review.Correct, Record: hy, Seen: 1, Extraction: json.RawMessage(`{"ending":{},"ending":{}}`), Reason: "x"}, review.RefuseMalformed},
		{hyInst, review.Action{Kind: "delete", Record: hy, Seen: 1, Reason: "x"}, review.RefuseUnknownKind},
		{hyInst, review.Action{Kind: review.AcceptWarning, Record: hy, Seen: 1, Finding: cur, Reason: "x", At: r.now.Add(-time.Hour)}, review.RefuseClock},
	}
	for i, st := range steps {
		if got := r.act(st.inst, st.a); got != st.want {
			t.Fatalf("step %d (%s): refusal %q, want %q", i, st.a.Kind, got, st.want)
		}
	}

	// A wrong correction is stored, fails its checks and cannot be
	// approved; correcting it back needs the warning accepted again.
	if code := r.act(hyInst, review.Action{Kind: review.Correct, Record: hy, Seen: 1, Extraction: withEnding(t, halyardRec, "714760.66"), Reason: "typo in the ending"}); code != "" {
		t.Fatalf("correct: %s", code)
	}
	s = r.state(hy)
	if s.Record.Result.Verdict != verify.Fail || len(s.Record.Result.Findings(verify.Error)) == 0 || !strings.Contains(s.Record.Reviews[1].Change, "ending.amount") {
		t.Fatalf("wrong correction %+v", s.Record)
	}
	if code := r.act(hyInst, review.Action{Kind: review.Approve, Record: hy, Seen: 2}); code != review.RefuseNotApprovable {
		t.Fatalf("approve a failing record: %q", code)
	}
	if code := r.act(hyInst, review.Action{Kind: review.Correct, Record: hy, Seen: 2, Extraction: halyardRec, Reason: "back to the printed value"}); code != "" {
		t.Fatalf("correct back: %s", code)
	}
	if code := r.act(hyInst, review.Action{Kind: review.Approve, Record: hy, Seen: 3}); code != review.RefuseNotApprovable {
		t.Fatalf("an acceptance before the correction counted: %q", code)
	}
	r.acceptAll(hy)
	s = r.state(hy)
	if code := r.act(hyInst, review.Action{Kind: review.SetEntities, Record: hy, Seen: s.Entries, Owner: &export.Entity{ID: " 1"}, Reason: "x"}); code != review.RefuseInvalidEntity {
		t.Fatalf("padded entity: %q", code)
	}
	if code := r.act(hyInst, review.Action{Kind: review.SetEntities, Record: hy, Seen: s.Entries, Owner: &export.Entity{ID: "1", Name: "Example Family Trust"}, Asset: &export.Entity{ID: "30"}, Reason: "from Addepar"}); code != "" {
		t.Fatalf("set entities: %s", code)
	}
	s = r.state(hy)
	if code := r.act(hyInst, review.Action{Kind: review.Approve, Record: hy, Seen: s.Entries}); code != "" {
		t.Fatalf("approve: %s", code)
	}
	s = r.state(hy)
	if !s.Approved() || s.Snapshot == nil || s.Snapshot.Owner.ID != "1" || s.Record.Approval == nil {
		t.Fatalf("approved state %+v", s)
	}
	if code := r.act(hyInst, review.Action{Kind: review.Approve, Record: hy, Seen: s.Entries}); code != review.RefuseApproved {
		t.Fatalf("approve twice: %q", code)
	}

	// The statement sent to manual review: a person enters it, with a
	// slip in the ending. The checks still run; their failures are
	// warnings to accept (ADR 0021, item 10).
	if code := r.act(keInst, review.Action{Kind: review.Manual, Document: keelson, Seen: 1, Extraction: keelsonRec, Reason: "x"}); code != review.RefuseStale {
		t.Fatalf("manual on a stale view: %q", code)
	}
	if code := r.act(keInst, review.Action{Kind: review.Manual, Document: keelson, Seen: 0, Extraction: withEnding(t, keelsonRec, "678520"), Reason: "read off page 1"}); code != "" {
		t.Fatalf("manual: %s", code)
	}
	ke := intake.RecordID(keelson, 1)
	s = r.state(ke)
	var downgraded []string
	for _, f := range s.Record.Result.Findings(verify.Warning) {
		if f.Downgraded {
			downgraded = append(downgraded, f.ID())
		}
	}
	if s.Record.Origin != export.OriginManual || s.Entries != 1 || len(downgraded) == 0 || len(s.Record.Result.Findings(verify.Error)) != 0 {
		t.Fatalf("manual record %+v, downgraded %v", s.Record, downgraded)
	}
	if !strings.Contains(strings.Join(downgraded, " "), "2/") {
		t.Fatalf("the roll-forward slip is not flagged: %v", downgraded)
	}
	if code := r.act(keInst, review.Action{Kind: review.Approve, Record: ke, Seen: 1}); code != review.RefuseNotApprovable {
		t.Fatalf("approve with unaccepted flags: %q", code)
	}
	r.acceptAll(ke)
	if code := r.act(keInst, review.Action{Kind: review.Approve, Record: ke, Seen: r.state(ke).Entries}); code != "" {
		t.Fatalf("approve manual: %s", code)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}

	// Export every approved record: the CSV in sight, the manifest in the
	// hidden folder under the CSV's hash.
	out, man := filepath.Join(dir, "Exports"), filepath.Join(dir, ".agent", "exports")
	exportArgs := func(extra ...string) []string {
		return append([]string{"export", "-config", cfg, "-by", "Jordan Lee", "-out", out, "-manifests", man}, extra...)
	}
	stdout := runOK(t, p, exportArgs("-approved")...)
	firstOut := stdout
	csvs, _ := filepath.Glob(filepath.Join(out, "addepar-*.csv"))
	mans, _ := filepath.Glob(filepath.Join(man, "*.json"))
	if len(csvs) != 1 || len(mans) != 1 || !strings.Contains(stdout, "exported 2 record(s)") {
		t.Fatalf("files %v %v; %s", csvs, mans, stdout)
	}
	csvBytes, err := os.ReadFile(csvs[0])
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(csvBytes)).ReadAll()
	if err != nil || len(rows) != 3 {
		t.Fatalf("csv %q %v", csvBytes, err)
	}
	if !strings.HasPrefix(filepath.Base(mans[0]), document256(csvBytes)+"-") {
		t.Fatalf("manifest %s is not named by the CSV's hash", mans[0])
	}
	var m export.Manifest
	mb, err := os.ReadFile(mans[0])
	if err != nil || json.Unmarshal(mb, &m) != nil || m.Version != export.ManifestVersion || len(m.Rows) != 2 {
		t.Fatalf("manifest %s %v", mb, err)
	}
	origins := map[string]bool{}
	for _, row := range m.Rows {
		origins[row.Origin] = true
		if row.SnapshotSHA256 == "" || row.Approval == nil {
			t.Fatalf("row %+v", row)
		}
	}
	if !origins[export.OriginManual] || !origins[export.OriginExtracted] {
		t.Fatalf("origins %v", origins)
	}

	// Nothing waits now; naming a record exports it again, deliberately.
	if stdout = runOK(t, p, exportArgs("-approved")...); !strings.Contains(stdout, "nothing to export") {
		t.Fatalf("second export: %s", stdout)
	}
	runOK(t, p, exportArgs("-record", hy)...)
	if mans, _ = filepath.Glob(filepath.Join(man, "*.json")); len(mans) != 2 {
		t.Fatalf("re-export manifests %v", mans)
	}

	// A write that fails after the export committed (PR #22): the export
	// is stored and counts, and -rewrite writes its files from the store.
	if err = os.Chmod(man, 0o500); err != nil {
		t.Fatal(err)
	}
	var fo, fe bytes.Buffer
	code := run(context.Background(), p, exportArgs("-record", hy), &fo, &fe)
	if err = os.Chmod(man, 0o700); err != nil {
		t.Fatal(err)
	}
	if code != exitFail || !strings.Contains(fe.String(), "-rewrite ") {
		t.Fatalf("failed write: exit %d %s", code, fe.String())
	}
	failedRun := strings.TrimSpace(fe.String()[strings.LastIndex(fe.String(), "-rewrite ")+len("-rewrite "):])
	stdout = runOK(t, p, "export", "-config", cfg, "-out", out, "-manifests", man, "-rewrite", failedRun)
	if mans, _ = filepath.Glob(filepath.Join(man, "*.json")); len(mans) != 3 || !strings.Contains(stdout, failedRun) {
		t.Fatalf("rewrite: manifests %v; %s", mans, stdout)
	}
	// Rewriting again, with the CSV gone, restores it byte for byte and
	// keeps the identical manifest.
	if err = os.Remove(csvs[0]); err != nil {
		t.Fatal(err)
	}
	_, after, _ := strings.Cut(firstOut, "(run ")
	first, _, _ := strings.Cut(after, ")")
	runOK(t, p, "export", "-config", cfg, "-out", out, "-manifests", man, "-rewrite", first)
	if again, rerr := os.ReadFile(csvs[0]); rerr != nil || !bytes.Equal(again, csvBytes) {
		t.Fatalf("rewritten CSV differs: %v", rerr)
	}
	var no, ne bytes.Buffer
	if code := run(context.Background(), p, []string{"export", "-config", cfg, "-out", out, "-manifests", man, "-rewrite", "no-such-run"}, &no, &ne); code != exitFail {
		t.Fatalf("rewrite of an unknown run: exit %d", code)
	}

	// A later action voids the approval: the record is refused.
	h, err = standalone.Open(context.Background(), p, cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	r.h = h
	if code := r.act(hyInst, review.Action{Kind: review.SetEntities, Record: hy, Seen: r.state(hy).Entries, Reason: "cleared"}); code != "" {
		t.Fatalf("clear entities: %s", code)
	}
	if s := r.state(hy); s.Approved() {
		t.Fatal("a later action left the approval standing")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	var o, e bytes.Buffer
	if code := run(context.Background(), p, exportArgs("-record", hy), &o, &e); code != exitFail || !strings.Contains(e.String(), review.RefuseNotApproved) {
		t.Fatalf("export of a voided approval: exit %d %s", code, e.String())
	}
}

func TestExportUsage(t *testing.T) {
	dir := t.TempDir()
	cfg := scoreConfig(t, dir, "state")
	p := program(nil)
	for name, args := range map[string][]string{
		"no config":        {"export", "-by", "a", "-out", dir, "-manifests", dir, "-approved"},
		"no by":            {"export", "-config", cfg, "-out", dir, "-manifests", dir, "-approved"},
		"blank by":         {"export", "-config", cfg, "-by", " ", "-out", dir, "-manifests", dir, "-approved"},
		"no selection":     {"export", "-config", cfg, "-by", "a", "-out", dir, "-manifests", dir},
		"rewrite and more": {"export", "-config", cfg, "-out", dir, "-manifests", dir, "-rewrite", "r", "-approved"},
		"both selections":  {"export", "-config", cfg, "-by", "a", "-out", dir, "-manifests", dir, "-approved", "-record", "x"},
		"out missing":      {"export", "-config", cfg, "-by", "a", "-out", filepath.Join(dir, "none"), "-manifests", dir, "-approved"},
		"extra argument":   {"export", "-config", cfg, "-by", "a", "-out", dir, "-manifests", dir, "-approved", "x"},
		"bad config":       {"export", "-config", filepath.Join(dir, "none.json"), "-by", "a", "-out", dir, "-manifests", dir, "-approved"},
	} {
		t.Run(name, func(t *testing.T) {
			var o, e bytes.Buffer
			if code := run(context.Background(), p, args, &o, &e); code != exitUsage {
				t.Fatalf("exit %d, want %d: %s", code, exitUsage, e.String())
			}
		})
	}
}

func runOK(t *testing.T, p standalone.Program, args ...string) string {
	t.Helper()
	var o, e bytes.Buffer
	if code := run(context.Background(), p, args, &o, &e); code != exitOK {
		t.Fatalf("%v: exit %d: %s %s", args, code, o.String(), e.String())
	}
	return o.String()
}

func document256(b []byte) string { return document.Hash(b) }
