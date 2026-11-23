package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/agent/agenttest"
	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/memstore"
)

const srcName = "morris"

var (
	searchDef = llms.ToolDefinition{Name: "search", Description: "Search the documents.", InputSchema: json.RawMessage(`{"type": "object", "properties": {"q": {"type": "string"}}}`)}
	createDef = llms.ToolDefinition{Name: "create", Description: "Create a document.", InputSchema: json.RawMessage(`{"type":"object"}`)}
)

// servedType is the ledger type granting the served tools search (a read)
// and create (an idempotent write) beside its own.
func servedType() *agent.Type[ledgerData] {
	var execs execLog
	tools := append(ledgerTools(&execs),
		agent.ServedTool[ledgerData](srcName, "search", agent.EffectRead, ""),
		agent.ServedTool[ledgerData](srcName, "create", agent.EffectWrite, agent.ContractIdempotent),
	)
	return ledgerType(&execs, tools...)
}

func servedSource(search, create agenttest.Handler) *agenttest.Source {
	return new(agenttest.Source).Add(searchDef, search).Add(createDef, create).
		Add(llms.ToolDefinition{Name: "ungranted", InputSchema: json.RawMessage(`{"type":"object"}`)}, nil)
}

type servedRun struct {
	t       *testing.T
	s       store.Store
	reg     *agent.Registry
	c       *agent.Controller
	sources map[string]agent.ToolSource
	client  *testllm.FakeChatClient
}

func newServedRun(t *testing.T, src agent.ToolSource, script ...llms.ChatResponse) *servedRun {
	t.Helper()
	s := memstore.New()
	createLedgerRun(t, s)
	reg := registry(t, servedType())
	r := &servedRun{t: t, s: s, reg: reg, c: agent.NewController(s, reg), client: scripted(script)}
	if src != nil {
		r.sources = map[string]agent.ToolSource{srcName: src}
	}
	return r
}

func (r *servedRun) activate(maxSteps int) agent.Outcome {
	r.t.Helper()
	return r.activateCtx(context.Background(), maxSteps)
}

func (r *servedRun) activateCtx(ctx context.Context, maxSteps int) agent.Outcome {
	r.t.Helper()
	fence := claim(r.t, r.s) //nolint:contextcheck // claiming is the test's setup, outside the activation
	out, err := r.c.Activate(ctx, agent.ActivationInput{
		Fence: fence, Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}, Namespace: ns, Principal: "alice",
		Client: modelClient(r.client), MaxSteps: maxSteps, ToolSources: r.sources,
	})
	if err != nil {
		r.t.Fatalf("Activate: %v", err)
	}
	return out
}

func (r *servedRun) results() []agent.ResultEntry {
	r.t.Helper()
	entries, err := r.s.Entries(context.Background(), "r1", 0)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []agent.ResultEntry
	for _, e := range entries {
		if e.Kind == agent.EntryResult {
			out = append(out, payload[agent.ResultEntry](r.t, e))
		}
	}
	return out
}

func (r *servedRun) resultContent(e *agent.ResultEntry) string {
	r.t.Helper()
	_, raw, err := r.s.Artifact(context.Background(), e.Result)
	if err != nil {
		r.t.Fatal(err)
	}
	var v struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		r.t.Fatal(err)
	}
	return v.Content
}

func TestServedToolRegistration(t *testing.T) {
	var execs execLog
	served := func(mod func(*agent.Tool[ledgerData])) *agent.Type[ledgerData] {
		tool := agent.ServedTool[ledgerData](srcName, "search", agent.EffectRead, "")
		mod(&tool)
		return ledgerType(&execs, append(ledgerTools(&execs), tool)...)
	}
	cases := map[string]struct {
		typ  *agent.Type[ledgerData]
		want string
	}{
		"execute": {served(func(tl *agent.Tool[ledgerData]) {
			tl.Execute = func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
				return agent.Result[ledgerData]{}, nil
			}
		}), "sets Execute"},
		"description": {served(func(tl *agent.Tool[ledgerData]) { tl.Definition.Description = "x" }), "description or schema"},
		"schema":      {served(func(tl *agent.Tool[ledgerData]) { tl.Definition.InputSchema = json.RawMessage(`{}`) }), "description or schema"},
		"atomic local": {served(func(tl *agent.Tool[ledgerData]) {
			tl.Effect, tl.Contract = agent.EffectWrite, agent.ContractAtomicLocal
		}), "returns no object operations"},
		"no contract": {served(func(tl *agent.Tool[ledgerData]) { tl.Effect = agent.EffectWrite }), "no write contract"},
		"bad source":  {served(func(tl *agent.Tool[ledgerData]) { tl.Source = "\xff" }), "source is not valid UTF-8"},
		"wrap-up": {func() *agent.Type[ledgerData] {
			typ := served(func(*agent.Tool[ledgerData]) {})
			typ.States[0].Model.WrapUp.Tools = []string{"finish", "search"}
			return typ
		}(), "whose result cannot transition"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := agent.NewRegistry().Register(tc.typ)
			if !errors.Is(err, agent.ErrInvalidType) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Register = %v, want ErrInvalidType containing %q", err, tc.want)
			}
		})
	}

	// A valid type's manifest lists a served tool with its source and no
	// schema, its identity covering the source.
	r := registry(t, servedType())
	m, ok := r.Manifest("ledger")
	if !ok {
		t.Fatal("no manifest")
	}
	var search *agent.ToolManifest
	for i := range m.Tools {
		if m.Tools[i].Name == "search" {
			search = &m.Tools[i]
		}
	}
	if search == nil || search.Source != srcName || search.InputSchema != nil || search.Description != "" || search.Identity == "" {
		t.Fatalf("manifest search = %+v", search)
	}
	other := ledgerType(&execs, append(ledgerTools(&execs), agent.ServedTool[ledgerData]("elsewhere", "search", agent.EffectRead, ""))...)
	m2, _ := registry(t, other).Manifest("ledger")
	for i := range m2.Tools {
		if m2.Tools[i].Name == "search" && m2.Tools[i].Identity == search.Identity {
			t.Fatal("a served tool's identity does not cover its source")
		}
	}
}

func TestServedToolStartChecks(t *testing.T) {
	ok := servedSource(agenttest.Text("found"), agenttest.Text("created"))
	lacking := new(agenttest.Source).Add(searchDef, nil)
	failing := new(agenttest.Source)
	failing.FailList(errors.New("connection refused"))
	badSchema := new(agenttest.Source).Add(createDef, nil).Add(llms.ToolDefinition{Name: "search", InputSchema: json.RawMessage(`{`)}, nil)
	badText := new(agenttest.Source).Add(createDef, nil).Add(llms.ToolDefinition{Name: "search", Description: "\xff"}, nil)
	dup := listing{ok, []llms.ToolDefinition{createDef, searchDef, searchDef}}
	cases := map[string]struct {
		src  agent.ToolSource
		want string
	}{
		"no source":     {nil, `provides no tool source "morris"`},
		"tool missing":  {lacking, `does not offer tool "create"`},
		"list fails":    {failing, "connection refused"},
		"invalid input": {badSchema, `tool "search" input schema`},
		"invalid text":  {badText, `tool "search" description is not valid UTF-8`},
		"listed twice":  {dup, `lists tool "search" twice`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newServedRun(t, tc.src, callsResp(tc2("c1", "finish")))
			ps := r.reg.CheckToolSources(context.Background(), "ledger", r.sources)
			if len(ps) == 0 || ps[0].Check != agent.CheckToolSources || ps[0].Reason != agent.ReasonConfigError || !strings.Contains(ps[0].Detail, tc.want) {
				t.Fatalf("CheckToolSources = %+v, want %q", ps, tc.want)
			}
			out := r.activate(0)
			if out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonConfigError || !strings.Contains(out.Detail, tc.want) {
				t.Fatalf("outcome = %+v, want config_error containing %q", out, tc.want)
			}
		})
	}
	r := newServedRun(t, ok)
	if ps := r.reg.CheckToolSources(context.Background(), "ledger", r.sources); ps != nil {
		t.Fatalf("CheckToolSources(good) = %+v", ps)
	}
	if ps := r.reg.CheckToolSources(context.Background(), "nope", nil); len(ps) != 1 || ps[0].Check != agent.CheckAgentType {
		t.Fatalf("CheckToolSources(unregistered) = %+v", ps)
	}
	// A type with no served tools needs no source.
	var execs execLog
	if ps := registry(t, ledgerType(&execs)).CheckToolSources(context.Background(), "ledger", nil); ps != nil {
		t.Fatalf("CheckToolSources(no served tools) = %+v", ps)
	}
}

// listing is a tool source whose listing is fixed, duplicates included,
// which agenttest.Source cannot list.
type listing struct {
	agent.ToolSource
	defs []llms.ToolDefinition
}

func (l listing) Tools(context.Context) ([]llms.ToolDefinition, error) { return l.defs, nil }

// tc2 is a call with empty arguments.
func tc2(id, name string) llms.ToolCall { return tc(id, name, `{}`) }

func TestServedToolCalls(t *testing.T) {
	src := servedSource(agenttest.Text("found it"), agenttest.Text("created it"))
	r := newServedRun(t, src,
		callsResp(tc("c1", "search", `{"q":"x"}`), tc("c2", "create", `{"title":"t"}`)),
		callsResp(tc2("c3", "finish")),
	)
	out := r.activate(0)
	if !out.Status.Terminal() || out.Reason.Code == agent.ReasonToolError || out.Reason.Code == agent.ReasonConfigError {
		t.Fatalf("outcome = %+v", out)
	}
	calls := src.Calls()
	if len(calls) != 2 {
		t.Fatalf("source calls = %+v", calls)
	}
	wantRC := agent.RunContext{Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}, Instance: inst, AgentType: "ledger", Principal: "alice"}
	for i, c := range calls {
		if c.Run != wantRC || c.Action.Scope != wantRC.Scope || c.Action.Call != i {
			t.Fatalf("call %d = %+v", i, c)
		}
	}
	if calls[0].IdempotencyKey != "" || string(calls[0].ToolCall.Parameters) != `{"q":"x"}` {
		t.Fatalf("read call = %+v", calls[0])
	}
	if calls[1].IdempotencyKey != calls[1].Action.IdempotencyKey() || calls[1].IdempotencyKey == "" {
		t.Fatalf("write call key = %q, want %q", calls[1].IdempotencyKey, calls[1].Action.IdempotencyKey())
	}
	// The model was offered the source's definitions, and not the
	// ungranted tool.
	reqs := r.client.Calls()
	var offered *llms.ToolDefinition
	for i := range reqs[0].Tools {
		if reqs[0].Tools[i].Name == "ungranted" {
			t.Fatal("an ungranted tool was offered")
		}
		if reqs[0].Tools[i].Name == "search" {
			offered = &reqs[0].Tools[i]
		}
	}
	if offered == nil || offered.Description != searchDef.Description || string(offered.InputSchema) != `{"type":"object","properties":{"q":{"type":"string"}}}` {
		t.Fatalf("offered search = %+v", offered)
	}
	// The start records the definitions.
	started := runStarted(t, r.s, "r1")
	if started.Served == "" {
		t.Fatal("run_started records no served definitions")
	}
	_, raw, err := r.s.Artifact(context.Background(), started.Served)
	if err != nil {
		t.Fatal(err)
	}
	var defs []llms.ToolDefinition
	if err := json.Unmarshal(raw, &defs); err != nil || len(defs) != 2 || defs[0].Name != "create" || defs[1].Name != "search" {
		t.Fatalf("served artifact = %s (%v)", raw, err)
	}
	res := r.results()
	if len(res) < 2 || res[0].Status != agent.ResultExecuted || res[0].Failure != "" || !strings.HasPrefix(r.resultContent(&res[0]), "found it") {
		t.Fatalf("results = %+v", res)
	}
}

func TestServedToolFailures(t *testing.T) {
	unreachable := fmt.Errorf("dial: %w", agent.ErrSourceUnreachable)
	srcErr := fmt.Errorf("jsonrpc: %w", agent.ErrSourceError)
	lost := errors.New("read: connection reset")
	malformed := func(context.Context, *agent.ServedCall) (toolloop.ToolResult, error) {
		return toolloop.ToolResult{Content: "\xff"}, nil
	}
	cases := []struct {
		name    string
		tool    string
		handler agenttest.Handler
		failure agent.Failure
		status  agent.ResultStatus
		fails   bool // the run fails with tool_error
	}{
		{"read unreachable", "search", agenttest.Fail(unreachable), agent.FailureUnreachable, agent.ResultExecuted, false},
		{"read source error", "search", agenttest.Fail(srcErr), agent.FailureSourceError, agent.ResultExecuted, false},
		{"read unknown", "search", agenttest.Fail(lost), agent.FailureUnknown, agent.ResultExecuted, false},
		{"read malformed", "search", malformed, agent.FailureSourceError, agent.ResultExecuted, false},
		{"write unreachable", "create", agenttest.Fail(unreachable), agent.FailureUnreachable, agent.ResultExecuted, false},
		{"write source error", "create", agenttest.Fail(srcErr), agent.FailureSourceError, agent.ResultExecuted, true},
		{"write unknown", "create", agenttest.Fail(lost), agent.FailureUnknown, agent.ResultUnknown, true},
		{"write malformed", "create", malformed, agent.FailureSourceError, agent.ResultExecuted, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := map[string]agenttest.Handler{"search": agenttest.Text("ok"), "create": agenttest.Text("ok")}
			h[tc.tool] = tc.handler
			r := newServedRun(t, servedSource(h["search"], h["create"]),
				callsResp(tc2("c1", tc.tool)),
				callsResp(tc2("c2", "finish")),
			)
			out := r.activate(0)
			res := r.results()
			if len(res) == 0 || res[0].Failure != tc.failure || res[0].Status != tc.status || !res[0].IsError {
				t.Fatalf("results = %+v", res)
			}
			content := r.resultContent(&res[0])
			if strings.Contains(content, "connection reset") || strings.Contains(content, "jsonrpc") {
				t.Fatalf("model-visible content leaks the error: %q", content)
			}
			if tc.fails {
				if out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonToolError {
					t.Fatalf("outcome = %+v, want tool_error", out)
				}
				return
			}
			// The model received the failure and went on to finish.
			if out.Reason.Code == agent.ReasonToolError || len(res) < 2 || res[1].Tool != "finish" {
				t.Fatalf("outcome = %+v, results = %+v", out, res)
			}
		})
	}
}

func TestServedToolLaterActivation(t *testing.T) {
	script := []llms.ChatResponse{callsResp(tc2("c1", "search")), callsResp(tc2("c2", "finish"))}
	t.Run("unchanged", func(t *testing.T) {
		r := newServedRun(t, servedSource(agenttest.Text("ok"), nil), script...)
		if out := r.activate(1); out.Status != store.StatusRunnable {
			t.Fatalf("first = %+v", out)
		}
		if out := r.activate(0); !out.Status.Terminal() || out.Reason.Code == agent.ReasonToolError {
			t.Fatalf("second = %+v", out)
		}
	})
	t.Run("changed definition", func(t *testing.T) {
		src := servedSource(agenttest.Text("ok"), nil)
		r := newServedRun(t, src, script...)
		if out := r.activate(1); out.Status != store.StatusRunnable {
			t.Fatalf("first = %+v", out)
		}
		changed := searchDef
		changed.Description = "Search, differently."
		src.Add(changed, agenttest.Text("ok"))
		out := r.activate(0)
		if out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonToolError || !strings.Contains(out.Detail, "changed the definition") {
			t.Fatalf("second = %+v", out)
		}
	})
	t.Run("schema formatting is not a change", func(t *testing.T) {
		src := servedSource(agenttest.Text("ok"), nil)
		r := newServedRun(t, src, script...)
		if out := r.activate(1); out.Status != store.StatusRunnable {
			t.Fatalf("first = %+v", out)
		}
		reformatted := searchDef
		reformatted.InputSchema = json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
		src.Add(reformatted, agenttest.Text("ok"))
		if out := r.activate(0); out.Reason.Code == agent.ReasonToolError {
			t.Fatalf("second = %+v", out)
		}
	})
	// A definition the record cannot hold byte for byte (a nil schema,
	// written as null; <, > and & in a schema, escaped) restores equal to
	// the unchanged listing (Codex round 1).
	for name, schema := range map[string]json.RawMessage{
		"nil schema":          nil,
		"HTML characters":     json.RawMessage(`{"type":"object","properties":{"q":{"type":"string","pattern":"<a>&b"}}}`),
		"description escapes": json.RawMessage(`{"type":"object"}`),
	} {
		t.Run("unchanged "+name, func(t *testing.T) {
			def := searchDef
			def.InputSchema = schema
			def.Description = "Find <docs> & more."
			src := servedSource(agenttest.Text("ok"), nil).Add(def, agenttest.Text("ok"))
			r := newServedRun(t, src, script...)
			if out := r.activate(1); out.Status != store.StatusRunnable {
				t.Fatalf("first = %+v", out)
			}
			if out := r.activate(0); !out.Status.Terminal() || out.Reason.Code == agent.ReasonToolError {
				t.Fatalf("second = %+v", out)
			}
		})
	}
	t.Run("source missing", func(t *testing.T) {
		r := newServedRun(t, servedSource(agenttest.Text("ok"), nil), script...)
		if out := r.activate(1); out.Status != store.StatusRunnable {
			t.Fatalf("first = %+v", out)
		}
		r.sources = nil
		out := r.activate(0)
		if out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonToolError {
			t.Fatalf("second = %+v", out)
		}
	})
}

// A served call interrupted by shutdown is resolved by the next
// activation as STM-6 says: a read is issued again under the same action;
// an idempotent write's outcome is unknown and fails the run (TOOL-12).
func TestServedToolRecovery(t *testing.T) {
	for _, tool := range []string{"search", "create"} {
		t.Run(tool, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			interrupt := func(ctx context.Context, _ *agent.ServedCall) (toolloop.ToolResult, error) {
				cancel(errors.New("SIGTERM"))
				<-ctx.Done()
				return toolloop.ToolResult{}, ctx.Err()
			}
			h := agenttest.Sequence(interrupt, agenttest.Text("ok"))
			src := servedSource(h, h)
			r := newServedRun(t, src, callsResp(tc2("c1", tool)), callsResp(tc2("c2", "finish")))
			if out := r.activateCtx(ctx, 0); out.Status != store.StatusRunnable || out.Reason.Code != agent.ReasonShutdown {
				t.Fatalf("first = %+v", out)
			}
			out := r.activate(0)
			calls := src.Calls()
			if tool == "search" {
				if len(calls) != 2 || calls[0].Action != calls[1].Action || out.Reason.Code == agent.ReasonToolError {
					t.Fatalf("outcome = %+v, calls = %+v", out, calls)
				}
				return
			}
			res := r.results()
			if len(calls) != 1 || out.Reason.Code != agent.ReasonToolError || len(res) == 0 || res[0].Status != agent.ResultUnknown {
				t.Fatalf("outcome = %+v, calls = %+v, results = %+v", out, calls, res)
			}
		})
	}
}

type eventLog struct {
	mu     sync.Mutex
	events []agent.Event
}

func (l *eventLog) Emit(_ context.Context, evs []agent.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, evs...)
	return nil
}

// A served call's failure class reaches telemetry as the result event's
// error kind (ADR 0025, item 6).
func TestServedToolTelemetry(t *testing.T) {
	log := &eventLog{}
	r := newServedRun(t, servedSource(agenttest.Fail(fmt.Errorf("x: %w", agent.ErrSourceUnreachable)), nil),
		callsResp(tc2("c1", "search")), callsResp(tc2("c2", "finish")))
	r.c = agent.NewController(r.s, r.reg, agent.WithTelemetry(log))
	r.activate(0)
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, ev := range log.events {
		if ev.Kind == agent.EntryResult && ev.Tool == "search" {
			if ev.ErrorKind != string(agent.FailureUnreachable) || !ev.IsError {
				t.Fatalf("result event = %+v", ev)
			}
			return
		}
	}
	t.Fatalf("no result event for search in %+v", log.events)
}
