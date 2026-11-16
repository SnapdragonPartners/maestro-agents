package intake_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/document"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/export"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/intake"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/synthpdf"
	"github.com/SnapdragonPartners/maestro-agents/standalone"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

const (
	syntheticDir = "../verify/testdata/synthetic"
	namespace    = "intake"
	lease        = time.Minute
	ownerA       = "owner-a"
	ownerB       = "owner-b"
)

var fixedNow = time.Date(2026, 4, 15, 9, 0, 0, 0, time.UTC)

func needPoppler(t testing.TB) {
	t.Helper()
	for _, bin := range []string{"pdftotext", "pdfinfo"} {
		if _, err := exec.LookPath(bin); err != nil {
			if os.Getenv("ADDEPAR_REQUIRE_POPPLER") != "" {
				t.Fatalf("%s not installed (poppler-utils), and ADDEPAR_REQUIRE_POPPLER is set", bin)
			}
			t.Skipf("%s not installed (poppler-utils)", bin)
		}
	}
}

// caseFile is the part of a synthetic case file these tests read.
type caseFile struct {
	Statement      string          `json:"statement"`
	CopyRestricted bool            `json:"copy_restricted,omitempty"`
	Record         json.RawMessage `json:"record"`
	Mutations      []struct {
		Set  map[string]any `json:"set"`
		Name string         `json:"name"`
	} `json:"mutations"`
}

func readCase(t testing.TB, path string) caseFile {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test data path
	if err != nil {
		t.Fatal(err)
	}
	var c caseFile
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return c
}

func synthetic(t testing.TB, name string) caseFile {
	t.Helper()
	return readCase(t, filepath.Join(syntheticDir, name+".json"))
}

// writePDF renders a synthetic statement to a PDF in dir and returns its
// path.
func writePDF(t testing.TB, dir string, c caseFile) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(syntheticDir, c.Statement)) //nolint:gosec // test data path
	if err != nil {
		t.Fatal(err)
	}
	pages := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n<<<page>>>\n")
	pdf, err := synthpdf.Build(pages, synthpdf.Options{CopyRestricted: c.CopyRestricted, FileID: c.Statement})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, strings.TrimSuffix(c.Statement, ".txt")+".pdf")
	if err := os.WriteFile(path, pdf, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// mutated returns the case's record with the named mutation applied.
func mutated(t testing.TB, c caseFile, name string) json.RawMessage {
	t.Helper()
	for _, m := range c.Mutations {
		if m.Name != name {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(c.Record, &rec); err != nil {
			t.Fatal(err)
		}
		keys := make([]string, 0, len(m.Set))
		for k := range m.Set {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			setPath(t, rec, strings.Split(k, "."), m.Set[k])
		}
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	t.Fatalf("no mutation %q", name)
	return nil
}

func setPath(t testing.TB, node any, path []string, val any) {
	t.Helper()
	switch n := node.(type) {
	case map[string]any:
		if len(path) == 1 {
			if val == nil {
				delete(n, path[0])
			} else {
				n[path[0]] = val
			}
			return
		}
		setPath(t, n[path[0]], path[1:], val)
	case []any:
		i, err := strconv.Atoi(path[0])
		if err != nil || i < 0 || i >= len(n) {
			t.Fatalf("bad index %q", path[0])
		}
		if len(path) == 1 {
			n[i] = val
			return
		}
		setPath(t, n[i], path[1:], val)
	default:
		t.Fatalf("cannot set %q in %T", path[0], node)
	}
}

// --- the scripted model ---

func toolCall(id, name, args string) llms.ToolCall {
	return llms.ToolCall{ID: id, Name: name, Parameters: json.RawMessage(args)}
}

func turn(calls ...llms.ToolCall) llms.ChatResponse {
	parts := make([]llms.ContentPart, len(calls))
	for i := range calls {
		c := calls[i]
		parts[i] = llms.ContentPart{Type: llms.ContentToolCall, ToolCall: &c}
	}
	return llms.ChatResponse{Message: llms.Message{Role: llms.RoleAssistant, Content: parts}, ToolCalls: calls}
}

func getDocument(id string) llms.ChatResponse { return turn(toolCall(id, "get_document", `{}`)) }

func submit(id string, recs ...json.RawMessage) llms.ChatResponse {
	b, err := json.Marshal(map[string]any{"records": recs})
	if err != nil {
		panic(err)
	}
	return turn(toolCall(id, "submit_extraction", string(b)))
}

// scripted answers each request by the number of assistant messages in its
// transcript, so a turn asked again gets the same answer, and records the
// requests it saw.
type scripted struct {
	script []llms.ChatResponse
	seen   []llms.ChatRequest
}

func (s *scripted) client() *testllm.FakeChatClient {
	return &testllm.FakeChatClient{
		ModelRef: llms.ModelRef{Provider: "fake", Name: "scripted"},
		Func: func(_ context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
			s.seen = append(s.seen, req)
			n := 0
			for _, m := range req.Messages {
				if m.Role == llms.RoleAssistant {
					n++
				}
			}
			if n >= len(s.script) {
				return llms.ChatResponse{}, fmt.Errorf("the script has no turn %d", n)
			}
			return s.script[n], nil
		},
	}
}

func modelClient(t testing.TB, s *scripted) *agent.ModelClient {
	t.Helper()
	mc, err := agent.NewModelClient(s.client(), agent.ModelClientConfig{Retry: &middleware.RetryConfig{MaxAttempts: 1}})
	if err != nil {
		t.Fatal(err)
	}
	return mc
}

// --- running the agent ---

type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func registry(t testing.TB, o intake.Options) *agent.Registry {
	t.Helper()
	if o.Now == nil {
		o.Now = func() time.Time { return fixedNow }
	}
	reg := agent.NewRegistry()
	if err := reg.Register(intake.Type(o)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

// createRun creates the run for pdf, as the standalone host does: instance
// and run named by the file's SHA-256, with its file trigger.
func createRun(t testing.TB, s store.Store, pdf string) string {
	t.Helper()
	b, err := os.ReadFile(pdf) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	sha := document.Hash(b)
	abs, err := filepath.Abs(pdf)
	if err != nil {
		t.Fatal(err)
	}
	trig, err := json.Marshal(standalone.FileTrigger{Path: abs, Name: filepath.Base(pdf), SHA256: sha, Size: int64(len(b))})
	if err != nil {
		t.Fatal(err)
	}
	id := "sha256-" + sha
	if _, _, err := s.CreateRun(context.Background(), store.Run{
		ID: id, Instance: id, AgentType: intake.TypeName, TriggerID: id, TriggerKind: agent.TriggerOneShot, Trigger: trig,
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return sha
}

func runID(sha string) string { return "sha256-" + sha }

// activate claims the run's instance for owner and runs an activation on c
// (the store, or a wrapper of it).
func activate(t testing.TB, c agent.Store, s store.Store, reg *agent.Registry, sha, owner string, sc *scripted, cfg json.RawMessage, b agent.Budgets) (agent.Outcome, error) {
	t.Helper()
	own, err := s.Claim(context.Background(), runID(sha), owner, lease)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	ctrl := agent.NewController(c, reg)
	return ctrl.Activate(context.Background(), agent.ActivationInput{ //nolint:wrapcheck // the caller checks the class
		Scope: store.Scope{Kind: store.ScopeRun, ID: runID(sha)}, Fence: own.Fence(),
		Namespace: namespace, Client: modelClient(t, sc), Config: cfg, Budgets: b,
	})
}

// --- reading what the run stored ---

func getJSON(t testing.TB, s store.Store, key string, v any) bool {
	t.Helper()
	obj, ok, err := s.Object(context.Background(), namespace, key)
	if err != nil {
		t.Fatalf("Object %s: %v", key, err)
	}
	if !ok {
		return false
	}
	if err := json.Unmarshal(obj.JSON, v); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
	return true
}

func listKeys(t testing.TB, s store.Store, prefix string) []string {
	t.Helper()
	infos, err := s.ListObjects(context.Background(), namespace, prefix)
	if err != nil {
		t.Fatalf("ListObjects %s: %v", prefix, err)
	}
	keys := make([]string, len(infos))
	for i, o := range infos {
		keys[i] = o.Key
	}
	return keys
}

func attempts(t testing.TB, s store.Store, sha string) []intake.Attempt {
	t.Helper()
	var out []intake.Attempt
	for _, k := range listKeys(t, s, intake.AttemptPrefix(sha)) {
		var a intake.Attempt
		getJSON(t, s, k, &a)
		out = append(out, a)
	}
	return out
}

func records(t testing.TB, s store.Store, sha string) []export.Record {
	t.Helper()
	var out []export.Record
	for _, k := range listKeys(t, s, intake.RecordPrefix(sha)) {
		var r export.Record
		getJSON(t, s, k, &r)
		out = append(out, r)
	}
	return out
}

func outcome(t testing.TB, s store.Store, sha string) (intake.Outcome, bool) {
	t.Helper()
	var o intake.Outcome
	ok := getJSON(t, s, intake.OutcomeKey(sha), &o)
	return o, ok
}

// canonical re-encodes JSON so two encodings of one value compare equal.
func canonical(t testing.TB, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
