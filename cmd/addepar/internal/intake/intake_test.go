package intake_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/document"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/intake"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/memstore"
)

// run runs the agent once on a synthetic statement, in a memory store, with
// the scripted model, and returns the store, the document's hash and the
// outcome.
func run(t *testing.T, name string, sc *scripted, cfg json.RawMessage, b agent.Budgets, o intake.Options) (store.Store, string, agent.Outcome) {
	t.Helper()
	needPoppler(t)
	s := memstore.New()
	t.Cleanup(func() { _ = s.Close() })
	pdf := writePDF(t, t.TempDir(), synthetic(t, name))
	sha := createRun(t, s, pdf)
	out, err := activate(t, s, s, registry(t, o), sha, ownerA, sc, cfg, b)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	return s, sha, out
}

func wantSucceeded(t *testing.T, out agent.Outcome, reason string) {
	t.Helper()
	if out.Status != store.StatusSucceeded || out.Reason.Code != reason {
		t.Fatalf("outcome = %+v, want succeeded/%s", out, reason)
	}
}

// lastResult returns the content of the tool result the model received
// last in the request for turn n.
func toolResults(req llms.ChatRequest) []llms.ToolResult {
	var out []llms.ToolResult
	for _, m := range req.Messages {
		for _, p := range m.Content {
			if p.ToolResult != nil {
				out = append(out, *p.ToolResult)
			}
		}
	}
	return out
}

// TestExtractVerifies is steps 1 and 2 of the slice: the statement read
// through its layout-mode text, a record extracted, verified in code and
// committed. The first submission takes the total-fund column, which only
// check 4 catches; the model receives the reason, resubmits, and the record
// is stored for review (E-6, V-1).
func TestExtractVerifies(t *testing.T) {
	c := synthetic(t, "keelson-vi")
	sc := &scripted{script: []llms.ChatResponse{
		getDocument("1"),
		submit("2", mutated(t, c, "total-fund column chosen")),
		submit("3", c.Record),
	}}
	s, sha, out := run(t, "keelson-vi", sc, nil, agent.Budgets{}, intake.Options{})
	wantSucceeded(t, out, agent.ReasonChanged)

	// The model read the page's text, with each label beside its numbers.
	res := toolResults(sc.seen[1])
	if len(res) != 1 || !strings.Contains(res[0].Content, "Partners' capital account, end of period") || !strings.Contains(res[0].Content, "678,250") {
		t.Fatalf("get_document result = %+v", res)
	}
	// The failed submission's reasons name check 4.
	res = toolResults(sc.seen[2])
	if len(res) != 2 || !res[1].IsError || !strings.Contains(res[1].Content, "check 4") || !strings.Contains(res[1].Content, "Attempt 1 of 3: fail") {
		t.Fatalf("first submission's result = %+v", res[len(res)-1])
	}

	atts := attempts(t, s, sha)
	if len(atts) != 2 {
		t.Fatalf("%d attempts stored, want 2", len(atts))
	}
	if a := atts[0]; a.N != 1 || a.Verdict != verify.Fail || len(a.Records) != 1 || !slices.Equal(a.Records[0].Result.Failed(), []int{verify.CheckInvestor}) {
		t.Fatalf("attempt 1 = %+v", a)
	}
	if a := atts[1]; a.N != 2 || a.Verdict != verify.Warn || a.Run != runID(sha) || a.Step == 0 {
		t.Fatalf("attempt 2 = %+v", a)
	}
	if canonical(t, atts[1].Submitted) != canonical(t, mustJSON(t, map[string]any{"records": []json.RawMessage{c.Record}})) {
		t.Fatalf("attempt 2 not stored as submitted: %s", atts[1].Submitted)
	}

	recs := records(t, s, sha)
	if len(recs) != 1 {
		t.Fatalf("%d records stored, want 1", len(recs))
	}
	r := recs[0]
	if r.ID != intake.RecordID(sha, 1) || r.Source.SHA256 != sha || r.Approval != nil || r.Result.Verdict != verify.Warn {
		t.Fatalf("record = %+v", r)
	}
	if canonical(t, mustJSON(t, r.Record)) != canonical(t, c.Record) {
		t.Fatalf("stored record differs from the submission:\n%s\n%s", mustJSON(t, r.Record), c.Record)
	}
	o, ok := outcome(t, s, sha)
	if !ok || o.Disposition != intake.DispositionVerified || o.Attempts != 2 || !slices.Equal(o.Records, []string{r.ID}) {
		t.Fatalf("outcome = %+v, %v", o, ok)
	}
	var doc intake.Document
	if !getJSON(t, s, intake.DocumentKey(sha), &doc) || doc.Document.SHA256 != sha || len(doc.Text) != 1 || !doc.Received.Equal(fixedNow) {
		t.Fatalf("document = %+v", doc)
	}
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAttemptCap: the third failed submission sends the statement to manual
// review, storing every attempt and no record (E-6).
func TestAttemptCap(t *testing.T) {
	c := synthetic(t, "keelson-vi")
	wrong := mutated(t, c, "print timestamp as the period")
	sc := &scripted{script: []llms.ChatResponse{submit("1", wrong), submit("2", wrong), submit("3", wrong)}}
	s, sha, out := run(t, "keelson-vi", sc, nil, agent.Budgets{}, intake.Options{})
	wantSucceeded(t, out, agent.ReasonChanged)
	if n := len(attempts(t, s, sha)); n != 3 {
		t.Fatalf("%d attempts, want 3", n)
	}
	if n := len(records(t, s, sha)); n != 0 {
		t.Fatalf("%d records stored from failed attempts", n)
	}
	o, _ := outcome(t, s, sha)
	if o.Disposition != intake.DispositionManual || o.Attempts != 3 || !strings.Contains(o.Reason, "attempt 3 of 3") {
		t.Fatalf("outcome = %+v", o)
	}
}

// TestConfigLowersCap: the configuration may lower the cap, never raise it.
func TestConfigLowersCap(t *testing.T) {
	c := synthetic(t, "keelson-vi")
	sc := &scripted{script: []llms.ChatResponse{submit("1", mutated(t, c, "print timestamp as the period"))}}
	s, sha, out := run(t, "keelson-vi", sc, json.RawMessage(`{"max_attempts":1}`), agent.Budgets{}, intake.Options{})
	wantSucceeded(t, out, agent.ReasonChanged)
	if o, _ := outcome(t, s, sha); o.Disposition != intake.DispositionManual || o.Attempts != 1 {
		t.Fatalf("outcome = %+v", o)
	}

	needPoppler(t)
	ms := memstore.New()
	defer ms.Close()
	sha = createRun(t, ms, writePDF(t, t.TempDir(), c))
	out, err := activate(t, ms, ms, registry(t, intake.Options{}), sha, ownerA, &scripted{}, json.RawMessage(`{"max_attempts":4}`), agent.Budgets{})
	if err != nil || out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonConfigError {
		t.Fatalf("max_attempts 4 = %+v, %v; want config_error", out, err)
	}
}

// TestMalformedSubmission: a record that does not decode is reported for
// the model to fix, without hiding the others' results, and counts as an
// attempt.
func TestMalformedSubmission(t *testing.T) {
	c := synthetic(t, "keelson-vi")
	sc := &scripted{script: []llms.ChatResponse{
		submit("1", c.Record, json.RawMessage(`{"investor":{"text":"x","page":1},"bogus":true}`)),
		turn(toolCall("2", "submit_extraction", `{"records":[]}`)),
		submit("3", c.Record, c.Record),
	}}
	s, sha, out := run(t, "keelson-vi", sc, nil, agent.Budgets{}, intake.Options{})
	wantSucceeded(t, out, agent.ReasonChanged)
	atts := attempts(t, s, sha)
	if len(atts) != 3 {
		t.Fatalf("%d attempts, want 3", len(atts))
	}
	a := atts[0]
	if a.Verdict != verify.Fail || len(a.Records) != 2 || a.Records[0].Result == nil || a.Records[0].Result.Verdict != verify.Warn || a.Records[1].Problem == "" {
		t.Fatalf("attempt 1 = %+v", a)
	}
	if atts[1].Problem == "" || atts[1].Verdict != verify.Fail {
		t.Fatalf("attempt 2 = %+v", atts[1])
	}
	if a := atts[2]; a.Records[1].Problem == "" || a.Records[1].Result != nil {
		t.Fatalf("attempt 3's duplicate record = %+v", a.Records[1])
	}
	if o, _ := outcome(t, s, sha); o.Disposition != intake.DispositionManual {
		t.Fatalf("outcome = %+v", o)
	}
}

// TestSetAside and TestManualReview: the model's two other exits store the
// document's outcome and end the run.
func TestSetAside(t *testing.T) {
	sc := &scripted{script: []llms.ChatResponse{
		turn(toolCall("1", "set_aside", `{"document_type":"invoice","reason":"x"}`)),
		turn(toolCall("2", "set_aside", `{"document_type":"capital_call","reason":"a call for capital, not a statement"}`)),
	}}
	s, sha, out := run(t, "keelson-vi", sc, nil, agent.Budgets{}, intake.Options{})
	wantSucceeded(t, out, agent.ReasonChanged)
	o, _ := outcome(t, s, sha)
	if o.Disposition != intake.DispositionSetAside || o.DocumentType != "capital_call" || o.Attempts != 0 {
		t.Fatalf("outcome = %+v", o)
	}
	if res := toolResults(sc.seen[1]); !res[0].IsError {
		t.Fatalf("unknown document type accepted: %+v", res[0])
	}
}

func TestManualReview(t *testing.T) {
	sc := &scripted{script: []llms.ChatResponse{
		getDocument("1"),
		turn(toolCall("2", "request_manual_review", `{"reason":"the roll-forward's labels wrap onto two lines"}`)),
	}}
	s, sha, out := run(t, "keelson-vi", sc, nil, agent.Budgets{}, intake.Options{})
	wantSucceeded(t, out, agent.ReasonChanged)
	if o, _ := outcome(t, s, sha); o.Disposition != intake.DispositionManual || !strings.Contains(o.Reason, "wrap") {
		t.Fatalf("outcome = %+v", o)
	}
}

// TestWrapUpSendsFailureToManualReview: a submission that fails in a
// wrap-up turn ends the run in manual review rather than leaving the state
// without a transition.
func TestWrapUpSendsFailureToManualReview(t *testing.T) {
	c := synthetic(t, "keelson-vi")
	sc := &scripted{script: []llms.ChatResponse{submit("1", mutated(t, c, "print timestamp as the period"))}}
	s, sha, out := run(t, "keelson-vi", sc, nil, agent.Budgets{Turns: 3}, intake.Options{})
	wantSucceeded(t, out, agent.ReasonChanged)
	atts := attempts(t, s, sha)
	if len(atts) != 1 || !atts[0].WrapUp {
		t.Fatalf("attempts = %+v", atts)
	}
	if o, _ := outcome(t, s, sha); o.Disposition != intake.DispositionManual || !strings.Contains(o.Reason, "wrap-up") {
		t.Fatalf("outcome = %+v", o)
	}
}

// TestCopyRestrictedImplicitAcceptance: until per-document acceptance is
// built (#17), running the agent accepts reading a copy-restricted
// statement; the acceptance is recorded on the document and the record.
func TestCopyRestrictedImplicitAcceptance(t *testing.T) {
	c := synthetic(t, "brightwater-iv")
	if !c.CopyRestricted {
		t.Fatal("brightwater-iv is no longer copy-restricted")
	}
	sc := &scripted{script: []llms.ChatResponse{submit("1", c.Record)}}
	s, sha, out := run(t, "brightwater-iv", sc, nil, agent.Budgets{}, intake.Options{})
	wantSucceeded(t, out, agent.ReasonChanged)
	var doc intake.Document
	getJSON(t, s, intake.DocumentKey(sha), &doc)
	ov := doc.Document.Override
	if ov == nil || ov.By != "operator" || !ov.At.Equal(fixedNow) || !strings.Contains(ov.Reason, "#17") || doc.Document.TextLayer != document.TextUsable {
		t.Fatalf("document = %+v, override %+v", doc.Document, ov)
	}
	recs := records(t, s, sha)
	if len(recs) != 1 || recs[0].Source.Override == nil || *recs[0].Source.Override != *ov {
		t.Fatalf("records = %+v", recs)
	}
}

// TestUnrestrictedHasNoOverride: the implicit acceptance applies only to a
// restricted PDF.
func TestUnrestrictedHasNoOverride(t *testing.T) {
	c := synthetic(t, "keelson-vi")
	s, sha, _ := run(t, "keelson-vi", &scripted{script: []llms.ChatResponse{submit("1", c.Record)}}, nil, agent.Budgets{}, intake.Options{})
	var doc intake.Document
	getJSON(t, s, intake.DocumentKey(sha), &doc)
	if doc.Document.Override != nil {
		t.Fatalf("override on an unrestricted document: %+v", doc.Document.Override)
	}
}

// TestNoTextLayer: a PDF with no usable text goes to manual review without
// a model call (I-3).
func TestNoTextLayer(t *testing.T) {
	scan := func(_ context.Context, name string, data []byte, _ document.Options) (*document.Document, error) {
		return &document.Document{SHA256: document.Hash(data), Name: name, PageCount: 1, TextLayer: document.TextNone,
			Pages: []document.Page{{Number: 1}}}, nil
	}
	sc := &scripted{}
	s, sha, out := run(t, "keelson-vi", sc, nil, agent.Budgets{}, intake.Options{Load: scan})
	wantSucceeded(t, out, agent.ReasonChanged)
	if len(sc.seen) != 0 {
		t.Fatalf("the model was called %d times", len(sc.seen))
	}
	if o, _ := outcome(t, s, sha); o.Disposition != intake.DispositionManual || !strings.Contains(o.Reason, "text layer") {
		t.Fatalf("outcome = %+v", o)
	}
}

// TestLoadFailure: a file that cannot be read as a PDF fails the run.
func TestLoadFailure(t *testing.T) {
	bad := func(context.Context, string, []byte, document.Options) (*document.Document, error) {
		return nil, errors.New("not a PDF")
	}
	_, _, out := run(t, "keelson-vi", &scripted{}, nil, agent.Budgets{}, intake.Options{Load: bad})
	if out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonStateError {
		t.Fatalf("outcome = %+v", out)
	}
}

// TestDuplicateDocument: the same file under another instance (a host that
// does not deduplicate by hash) is flagged, not processed again (I-2).
func TestDuplicateDocument(t *testing.T) {
	c := synthetic(t, "keelson-vi")
	s, sha, _ := run(t, "keelson-vi", &scripted{script: []llms.ChatResponse{submit("1", c.Record)}}, nil, agent.Budgets{}, intake.Options{})
	before := listKeys(t, s, "")
	reg := registry(t, intake.Options{})
	var doc intake.Document
	getJSON(t, s, intake.DocumentKey(sha), &doc)
	trig, err := s.Run(context.Background(), runID(sha))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, cerr := s.CreateRun(context.Background(), store.Run{
		ID: "again", Instance: "again", AgentType: intake.TypeName, TriggerID: "again", TriggerKind: agent.TriggerFolder, Trigger: trig.Trigger,
	}); cerr != nil {
		t.Fatal(cerr)
	}
	own, err := s.Claim(context.Background(), "again", ownerA, lease)
	if err != nil {
		t.Fatal(err)
	}
	sc := &scripted{}
	out, err := agent.NewController(s, reg).Activate(context.Background(), agent.ActivationInput{
		Scope: store.Scope{Kind: store.ScopeRun, ID: "again"}, Fence: own.Fence(), Namespace: namespace, Client: modelClient(t, sc),
	})
	if err != nil {
		t.Fatal(err)
	}
	wantSucceeded(t, out, agent.ReasonNoChange)
	if after := listKeys(t, s, ""); !slices.Equal(before, after) || len(sc.seen) != 0 {
		t.Fatalf("objects before %v, after %v; model calls %d", before, after, len(sc.seen))
	}
}

// TestManifest: the type registers, accepts both file triggers, and every
// state's exits are its wrap-up tools.
func TestManifest(t *testing.T) {
	reg := registry(t, intake.Options{})
	m, ok := reg.Manifest(intake.TypeName)
	if !ok {
		t.Fatal("not registered")
	}
	if !slices.Equal(m.Triggers, []string{agent.TriggerOneShot, agent.TriggerFolder}) || len(m.Templates) != 2 {
		t.Fatalf("manifest = %+v", m)
	}
	var tools []string
	for _, tl := range m.Tools {
		tools = append(tools, tl.Name)
		if tl.Name != "get_document" && (tl.Effect != agent.EffectWrite || tl.Contract != agent.ContractAtomicLocal) {
			t.Errorf("tool %s is %s/%s, want an atomic local write", tl.Name, tl.Effect, tl.Contract)
		}
	}
	slices.Sort(tools)
	if want := []string{"get_document", "request_manual_review", "set_aside", "submit_extraction"}; !slices.Equal(tools, want) {
		t.Fatalf("tools = %v, want %v", tools, want)
	}
}
