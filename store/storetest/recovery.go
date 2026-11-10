package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// TEST-5: the harness stops an activation at every durable boundary, before
// the commit lands and after it lands but before its result is returned,
// continues the run in a new activation, and checks that no effect is
// duplicated and no work is lost. The agent is a model state whose tools
// read, write as atomic local transactions (each appending an item to a
// shared object, so a duplicated effect shows as a repeated item), are
// denied by policy, name no offered tool, and transition to a terminal
// state; its model answers once without a call, so it is nudged.

const (
	recoveryNS   = "recovery"
	recoveryType = "recovery"
)

type recoveryData struct {
	Done bool `json:"done"`
}

func recoveryAgent() *agent.Type[recoveryData] {
	write := func(ctx context.Context, c *agent.Call[recoveryData]) (agent.Result[recoveryData], error) {
		var args struct {
			Item string `json:"item"`
		}
		if err := json.Unmarshal(c.ToolCall.Parameters, &args); err != nil {
			return agent.ErrorResult[recoveryData](err.Error()), nil
		}
		obj, ok, err := c.Env.Object(ctx, recoveryNS, c.Env.RunID())
		if err != nil {
			return agent.Result[recoveryData]{}, err
		}
		var items []string
		if ok {
			if uerr := json.Unmarshal(obj.JSON, &items); uerr != nil {
				return agent.Result[recoveryData]{}, uerr
			}
		}
		raw, err := json.Marshal(append(items, args.Item))
		if err != nil {
			return agent.Result[recoveryData]{}, err
		}
		return agent.Result[recoveryData]{
			Content: "wrote " + args.Item,
			Ops:     []store.Op{store.PutObjectJSON(recoveryNS, c.Env.RunID(), obj.Version, raw)},
		}, nil
	}
	return &agent.Type[recoveryData]{
		Name:    recoveryType,
		Version: "1",
		Initial: "work",
		States: []agent.State[recoveryData]{
			{Name: "work", Next: []string{"done"}, Model: &agent.ModelState[recoveryData]{
				Prompt: func(context.Context, *agent.Env, recoveryData) (llms.ChatRequest, error) {
					return llms.ChatRequest{Messages: []llms.Message{llms.UserText("write a, b and c")}}, nil
				},
				Tools: []agent.Tool[recoveryData]{
					{
						Definition: llms.ToolDefinition{Name: "read"},
						Effect:     agent.EffectRead,
						Execute: func(context.Context, *agent.Call[recoveryData]) (agent.Result[recoveryData], error) {
							return agent.TextResult[recoveryData]("nothing written yet"), nil
						},
					},
					{
						Definition: llms.ToolDefinition{Name: "write"},
						Effect:     agent.EffectWrite,
						Contract:   agent.ContractAtomicLocal,
						Execute:    write,
					},
					{
						Definition: llms.ToolDefinition{Name: "finish"},
						Effect:     agent.EffectRead,
						Execute: func(context.Context, *agent.Call[recoveryData]) (agent.Result[recoveryData], error) {
							return agent.Result[recoveryData]{Content: "done", To: "done", Data: recoveryData{Done: true}}, nil
						},
					},
				},
			}},
			{Name: "done", End: agent.EndChanged},
		},
	}
}

func recoveryCall(id, name, args string) llms.ToolCall {
	return llms.ToolCall{ID: id, Name: name, Parameters: json.RawMessage(args)}
}

func recoveryTurn(calls ...llms.ToolCall) llms.ChatResponse {
	parts := make([]llms.ContentPart, len(calls))
	for i := range calls {
		c := calls[i]
		parts[i] = llms.ContentPart{Type: llms.ContentToolCall, ToolCall: &c}
	}
	return llms.ChatResponse{Message: llms.Message{Role: llms.RoleAssistant, Content: parts}, ToolCalls: calls}
}

// recoveryScript is the model's answers; the client answers each turn by the
// number of assistant messages in the transcript, so a turn asked again gets
// the same answer.
func recoveryScript() []llms.ChatResponse {
	return []llms.ChatResponse{
		recoveryTurn(recoveryCall("1", "read", `{}`), recoveryCall("2", "write", `{"item":"a"}`), recoveryCall("3", "erase", `{}`)),
		recoveryTurn(recoveryCall("4", "write", `{"item":"b"}`), recoveryCall("5", "write", `{"item":"denied"}`), recoveryCall("6", "write", `{"item":"c"}`)),
		{Message: llms.AssistantText("finished"), Text: "finished"},
		recoveryTurn(recoveryCall("7", "finish", `{}`), recoveryCall("8", "write", `{"item":"late"}`)),
	}
}

func recoveryClient() *testllm.FakeChatClient {
	script := recoveryScript()
	return &testllm.FakeChatClient{
		ModelRef: llms.ModelRef{Provider: "storetest", Name: "script"},
		Func: func(_ context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
			n := 0
			for _, m := range req.Messages {
				if m.Role == llms.RoleAssistant {
					n++
				}
			}
			if n >= len(script) {
				return llms.ChatResponse{}, fmt.Errorf("storetest: the script has no turn %d", n)
			}
			return script[n], nil
		},
	}
}

func recoveryPolicy(_ context.Context, req *agent.PolicyRequest) (agent.Decision, error) {
	if strings.Contains(string(req.Call.Parameters), "denied") {
		return agent.Decision{Verdict: agent.VerdictDeny, Reason: "not this item"}, nil
	}
	return agent.Decision{Verdict: agent.VerdictAllow}, nil
}

// crashingStore fails one commit, the at'th from 1, as a process dying there
// would: before it lands, or after it landed and before its result returns.
type crashingStore struct {
	store.Store
	n, at   int
	mu      sync.Mutex
	landing bool
}

var errCrash = errors.New("storetest: injected crash")

func (c *crashingStore) Commit(ctx context.Context, f store.Fence, ops []store.Op) error {
	c.mu.Lock()
	c.n++
	hit := c.n == c.at
	c.mu.Unlock()
	if !hit {
		return c.Store.Commit(ctx, f, ops) //nolint:wrapcheck // a transparent wrapper
	}
	if c.landing {
		if err := c.Store.Commit(ctx, f, ops); err != nil {
			return err //nolint:wrapcheck // a transparent wrapper
		}
	}
	return errCrash
}

func recoveryActivate(t testing.TB, s agent.Store, ctrl store.ControlPlane, clk *Clock) (agent.Outcome, error) {
	t.Helper()
	reg := agent.NewRegistry()
	if err := reg.Register(recoveryAgent()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	own, err := ctrl.Claim(context.Background(), instance, ownerA, lease)
	if err != nil {
		t.Fatalf("Claim at %v: %v", clk.Now(), err)
	}
	c := agent.NewController(s, reg, agent.WithPolicy(recoveryPolicy))
	return c.Activate(context.Background(), agent.ActivationInput{ //nolint:wrapcheck // the caller checks the error's class
		Fence: own.Fence(), Scope: store.Scope{Kind: store.ScopeRun, ID: runID}, Client: recoveryClient(),
	})
}

func recoveryRun(t testing.TB, s store.Store) {
	t.Helper()
	_, _, err := s.CreateRun(context.Background(), store.Run{
		ID: runID, Instance: instance, AgentType: recoveryType, TriggerID: "trigger-1", Trigger: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
}

// countingCommits counts commits.
type countingCommits struct {
	store.Store
	n  int
	mu sync.Mutex
}

func (c *countingCommits) Commit(ctx context.Context, f store.Fence, ops []store.Op) error {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.Store.Commit(ctx, f, ops) //nolint:wrapcheck // a transparent wrapper
}

func crashRecovery(t *testing.T, h Harness) {
	clk := NewClock()
	s := h.Open(t, Options{Now: clk.Now})
	counted := &countingCommits{Store: s}
	recoveryRun(t, s)
	out, err := recoveryActivate(t, counted, s, clk)
	if err != nil || out.Status != store.StatusSucceeded {
		t.Fatalf("uninterrupted run = %+v, %v", out, err)
	}
	checkRecovered(t, s)
	_ = s.Close()
	total := counted.n
	for at := 1; at <= total; at++ {
		for _, landing := range []bool{false, true} {
			t.Run(fmt.Sprintf("commit-%d/landing-%v", at, landing), func(t *testing.T) {
				crashAt(t, h, at, landing, at == total)
			})
		}
	}
}

func crashAt(t testing.TB, h Harness, at int, landing, last bool) {
	clk := NewClock()
	o := Options{Now: clk.Now}
	s := h.Open(t, o)
	recoveryRun(t, s)
	_, err := recoveryActivate(t, &crashingStore{Store: s, at: at, landing: landing}, s, clk)
	if !errors.Is(err, agent.ErrStoreFailure) {
		_ = s.Close()
		t.Fatalf("activation stopped at commit %d = %v, want a store failure", at, err)
	}
	// The stopped worker never returns: the process restarts (a durable
	// store is reopened) and its lease runs out.
	if h.Reopen != nil {
		s = h.Reopen(t, s, o)
	}
	defer s.Close()
	clk.Advance(lease + 1)
	// The host rereads the run record before acting (ADR 0003, item 14).
	run, err := s.Run(context.Background(), runID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status.Terminal() {
		if !last || !landing || run.Status != store.StatusSucceeded {
			t.Fatalf("run is %s after commit %d of the run stopped (landed %v)", run.Status, at, landing)
		}
	} else {
		out, err := recoveryActivate(t, s, s, clk)
		if err != nil {
			t.Fatalf("continuing activation: %v", err)
		}
		if out.Status != store.StatusSucceeded || out.Reason.Code != agent.ReasonChanged {
			t.Fatalf("continuing activation = %+v", out)
		}
	}
	checkRecovered(t, s)
}

// checkRecovered checks the finished run: each item written exactly once,
// no denied or late write, every turn recorded once (no turn asked of the
// model again once recorded), and every call resolved.
func checkRecovered(t testing.TB, s store.Store) {
	t.Helper()
	ctx := context.Background()
	obj, ok, err := s.Object(ctx, recoveryNS, runID)
	if err != nil || !ok {
		t.Fatalf("Object = %v, %v", ok, err)
	}
	var items []string
	if uerr := json.Unmarshal(obj.JSON, &items); uerr != nil {
		t.Fatal(uerr)
	}
	if !slices.Equal(items, []string{"a", "b", "c"}) {
		t.Fatalf("written items = %v, want [a b c] exactly once each", items)
	}
	es, err := s.Entries(ctx, runID, 0)
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	type call struct {
		perm     agent.Permission
		resolved bool
	}
	var turns []map[int]*call
	for _, e := range es {
		switch e.Kind {
		case agent.EntryTurn:
			var tu agent.TurnEntry
			mustDecode(t, e, &tu)
			calls := map[int]*call{}
			for i := range tu.Calls {
				calls[i] = &call{}
			}
			turns = append(turns, calls)
		case agent.EntryPermission:
			var p agent.PermissionEntry
			mustDecode(t, e, &p)
			turns[len(turns)-1][p.Call].perm = p.Permission
		case agent.EntryResult:
			var r agent.ResultEntry
			mustDecode(t, e, &r)
			turns[len(turns)-1][r.Call].resolved = true
		}
	}
	if want := len(recoveryScript()); len(turns) != want {
		t.Fatalf("%d turns recorded, want %d", len(turns), want)
	}
	for n, calls := range turns {
		for i, c := range calls {
			if !c.resolved && c.perm.Sendable() || !c.resolved && c.perm == "" {
				t.Fatalf("turn %d call %d is unresolved (permission %q)", n, i, c.perm)
			}
		}
	}
}

func mustDecode(t testing.TB, e store.Entry, v any) {
	t.Helper()
	if err := json.Unmarshal(e.Payload, v); err != nil {
		t.Fatalf("decode %s entry %d: %v", e.Kind, e.Seq, err)
	}
}
