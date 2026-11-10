package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

const ledgerNS = "ledger"

// ledgerData is the ledger agent's state data.
type ledgerData struct {
	Finished bool `json:"finished"`
}

// ledgerTools are the ledger agent's tools: lookup reads, record appends an
// item to the run's ledger object as an atomic local transaction (so a
// duplicated effect would show as a repeated item), and finish transitions
// to the review state.
func ledgerTools(execs *execLog) []agent.Tool[ledgerData] {
	return []agent.Tool[ledgerData]{
		{
			Definition: llms.ToolDefinition{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Effect:     agent.EffectRead,
			Execute: func(_ context.Context, c *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
				execs.add("lookup", c)
				return agent.TextResult[ledgerData]("value of " + string(c.ToolCall.Parameters)), nil
			},
		},
		{
			Definition: llms.ToolDefinition{Name: "record", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Effect:     agent.EffectWrite,
			Contract:   agent.ContractAtomicLocal,
			Execute:    recordItem(execs),
		},
		{
			Definition: llms.ToolDefinition{Name: "finish", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Effect:     agent.EffectRead,
			Execute: func(_ context.Context, c *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
				execs.add("finish", c)
				return agent.Result[ledgerData]{Content: "finished", To: "review", Data: ledgerData{Finished: true}}, nil
			},
		},
	}
}

func recordItem(execs *execLog) func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
	return func(ctx context.Context, c *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
		execs.add("record", c)
		var args struct {
			Item string `json:"item"`
		}
		if err := json.Unmarshal(c.ToolCall.Parameters, &args); err != nil {
			return agent.ErrorResult[ledgerData]("bad arguments: " + err.Error()), nil //nolint:nilerr // the model receives the error
		}
		items, version, err := ledger(ctx, c.Env)
		if err != nil {
			return agent.Result[ledgerData]{}, err
		}
		items = append(items, args.Item)
		raw, _ := json.Marshal(items)
		return agent.Result[ledgerData]{
			Content: "recorded " + args.Item,
			Ops:     []store.Op{store.PutObjectJSON(ledgerNS, c.Env.RunID(), version, raw)},
		}, nil
	}
}

func ledger(ctx context.Context, env *agent.Env) ([]string, uint64, error) {
	obj, ok, err := env.Object(ctx, ledgerNS, env.RunID())
	if err != nil {
		return nil, 0, err
	}
	var items []string
	if ok {
		if err := json.Unmarshal(obj.JSON, &items); err != nil {
			return nil, 0, err
		}
	}
	return items, obj.Version, nil
}

// execLog records tool executions.
type execLog struct {
	calls []string
	keys  map[string]string
	mu    sync.Mutex
}

func (l *execLog) add(tool string, c *agent.Call[ledgerData]) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, fmt.Sprintf("%s#%d.%d", tool, c.Action.Step, c.Action.Call))
	if l.keys == nil {
		l.keys = map[string]string{}
	}
	if c.IdempotencyKey != "" {
		l.keys[fmt.Sprintf("%d.%d", c.Action.Step, c.Action.Call)] = c.IdempotencyKey
	}
}

func ledgerType(execs *execLog, tools ...agent.Tool[ledgerData]) *agent.Type[ledgerData] {
	if tools == nil {
		tools = ledgerTools(execs)
	}
	return &agent.Type[ledgerData]{
		Name:    "ledger",
		Version: "1",
		Initial: "work",
		States: []agent.State[ledgerData]{
			{Name: "work", Next: []string{"review", "work"}, Model: &agent.ModelState[ledgerData]{
				Prompt: func(_ context.Context, env *agent.Env, _ ledgerData) (llms.ChatRequest, error) {
					return llms.ChatRequest{
						System:   []llms.ContentPart{llms.Text("Record the items.")},
						Messages: []llms.Message{llms.UserText("items for " + env.RunID())},
					}, nil
				},
				Tools:  tools,
				WrapUp: agent.WrapUp{Tools: []string{"finish"}},
			}},
			{Name: "review", Next: []string{"done", "rejected"}, Code: func(ctx context.Context, env *agent.Env, d ledgerData) (agent.Transition[ledgerData], error) {
				items, _, err := ledger(ctx, env)
				if err != nil {
					return agent.Transition[ledgerData]{}, err
				}
				if !d.Finished || !slices.Equal(items, []string{"a", "b", "c"}) {
					return agent.Goto("rejected", d), nil
				}
				return agent.Goto("done", d), nil
			}},
			{Name: "done", End: agent.EndChanged},
			{Name: "rejected", End: agent.EndInvalidOutput},
		},
	}
}

func tc(id, name, args string) llms.ToolCall {
	return llms.ToolCall{ID: id, Name: name, Parameters: json.RawMessage(args)}
}

func callsResp(calls ...llms.ToolCall) llms.ChatResponse {
	parts := make([]llms.ContentPart, len(calls))
	for i := range calls {
		c := calls[i]
		parts[i] = llms.ContentPart{Type: llms.ContentToolCall, ToolCall: &c}
	}
	return llms.ChatResponse{Message: llms.Message{Role: llms.RoleAssistant, Content: parts}, ToolCalls: calls, Usage: llms.Usage{InputTokens: 10, OutputTokens: 5}}
}

func textResp(s string) llms.ChatResponse {
	return llms.ChatResponse{Message: llms.AssistantText(s), Text: s}
}

// ledgerScript is the model's side: the turn it answers is chosen by the
// number of assistant messages in the transcript, so a re-asked turn gets
// the same answer.
var ledgerScript = []llms.ChatResponse{
	callsResp(tc("c1", "lookup", `{"key":"a"}`), tc("c2", "record", `{"item":"a"}`), tc("c3", "bogus", `{}`)),
	callsResp(tc("c4", "record", `{"item":"b"}`), tc("c5", "record", `{"item":"c"}`)),
	textResp("All recorded."),
	callsResp(tc("c6", "finish", `{}`), tc("c7", "record", `{"item":"z"}`)),
}

func scripted(script []llms.ChatResponse) *testllm.FakeChatClient {
	return &testllm.FakeChatClient{
		ModelRef: llms.ModelRef{Provider: "fake", Name: "scripted"},
		Func: func(_ context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
			n, prompts := 0, 0
			for _, m := range req.Messages {
				if m.Role == llms.RoleAssistant {
					n++
				}
				if m.Role == llms.RoleUser && strings.HasPrefix(m.Content[0].Text, "items for ") {
					prompts++
				}
			}
			// Every request carries the prompt exactly once: the transcript
			// restored from checkpoints matches what was sent.
			if prompts != 1 {
				return llms.ChatResponse{}, fmt.Errorf("request carries the prompt %d times", prompts)
			}
			if n >= len(script) {
				return llms.ChatResponse{}, fmt.Errorf("script has no turn %d", n)
			}
			return script[n], nil
		},
	}
}

func createLedgerRun(t testing.TB, s store.Store) {
	const id = "r1"
	t.Helper()
	_, _, err := s.CreateRun(context.Background(), store.Run{
		ID: id, Instance: inst, AgentType: "ledger", TriggerID: "trig-" + id, Trigger: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
}

func activateWith(ctx context.Context, c *agent.Controller, f store.Fence, client llms.ChatClient, maxSteps int) (agent.Outcome, error) {
	return c.Activate(ctx, agent.ActivationInput{
		Fence: f, Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}, Client: client, MaxSteps: maxSteps,
	})
}

func ledgerItems(t testing.TB, s store.Store) []string {
	t.Helper()
	obj, ok, err := s.Object(context.Background(), ledgerNS, "r1")
	if err != nil {
		t.Fatalf("Object: %v", err)
	}
	var items []string
	if ok {
		if err := json.Unmarshal(obj.JSON, &items); err != nil {
			t.Fatal(err)
		}
	}
	return items
}

func payload[T any](t testing.TB, e store.Entry) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(e.Payload, &v); err != nil {
		t.Fatalf("decode %s entry: %v", e.Kind, err)
	}
	return v
}

func TestModelStateRun(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createLedgerRun(t, s)
		execs := &execLog{}
		client := scripted(ledgerScript)
		out, err := activateWith(context.Background(), agent.NewController(s, registry(t, ledgerType(execs))), claim(t, s), client, 0)
		if err != nil {
			t.Fatal(err)
		}
		if out.Status != store.StatusSucceeded || out.Reason.Code != agent.ReasonChanged || out.Steps != 5 {
			t.Fatalf("outcome = %+v, want succeeded/changed after 5 steps (4 turns and the review)", out)
		}
		if got := ledgerItems(t, s); !slices.Equal(got, []string{"a", "b", "c"}) {
			t.Fatalf("ledger = %v", got)
		}
		wantExecs := []string{"lookup#1.0", "record#1.1", "record#2.0", "record#2.1", "finish#4.0"}
		if !slices.Equal(execs.calls, wantExecs) {
			t.Fatalf("executions = %v, want %v", execs.calls, wantExecs)
		}
		wantReleased(t, s)

		es := entries(t, s, "r1")
		want := []string{
			"activation_started", "run_started",
			"model_attempt", "turn", "permission", "attempt", "result", "permission", "attempt", "result",
			"result", "model_attempt", "turn", "permission", "attempt", "result", "permission", "attempt", "result",
			"model_attempt", "turn",
			"model_attempt", "turn", "permission", "attempt", "result", "result", "transition",
			"transition", "outcome",
		}
		if got := kinds(es); !slices.Equal(got, want) {
			t.Fatalf("record =\n%v\nwant\n%v", got, want)
		}
		// The refused call's result commits with the next attempt; the
		// finish call's later sibling is not executed (STM-3).
		refused := payload[agent.ResultEntry](t, es[10])
		notExec := payload[agent.ResultEntry](t, es[26])
		if refused.Status != agent.ResultRefused || refused.Call != 2 || notExec.Status != agent.ResultNotExecuted || notExec.Call != 1 {
			t.Fatalf("refused = %+v, not executed = %+v", refused, notExec)
		}
		at := payload[agent.ModelAttemptEntry](t, es[2])
		if at.Model.Name != "scripted" || at.Base != 0 || len(at.Messages) != 1 || at.Params == "" {
			t.Fatalf("first attempt = %+v", at)
		}
		write := payload[agent.AttemptEntry](t, es[8])
		if write.Key == "" || write.Key != write.Action.IdempotencyKey() || payload[agent.AttemptEntry](t, es[5]).Key != "" {
			t.Fatalf("write attempt %+v carries no key, or the read carries one", write)
		}

		// What the model saw: the refused call's toolloop text, then the
		// nudge after the text-only answer.
		calls := client.Calls()
		if len(calls) != 4 {
			t.Fatalf("model asked %d times, want 4", len(calls))
		}
		second := calls[1].Messages
		results := second[len(second)-1]
		if r := results.Content[2].ToolResult; r == nil || !r.IsError || !strings.Contains(r.Content, `"bogus" is not available`) {
			t.Fatalf("refused result = %+v", results.Content[2])
		}
		fourth := calls[3].Messages
		if last := fourth[len(fourth)-1]; last.Role != llms.RoleUser || last.Content[0].Text != agent.DefaultNudge {
			t.Fatalf("fourth request ends with %+v, want the nudge", last)
		}
		// The prompt, two turns with their results, the text answer and the
		// nudge.
		if len(fourth) != 7 {
			t.Fatalf("fourth request has %d messages, want 7", len(fourth))
		}
		if len(calls[0].Tools) != 3 || calls[0].System[0].Text != "Record the items." {
			t.Fatalf("first request tools %d, system %+v", len(calls[0].Tools), calls[0].System)
		}
	})
}

// TestModelStateCrashAtEveryCommit is TEST-5 on the agent's own fixture,
// whose finishing tool leads to a code state: the activation dies at each commit in turn, before
// it lands or after, and a new activation continues the run. No effect is
// duplicated, no work is lost, and no turn is asked of the model again
// once it is recorded.
func TestModelStateCrashAtEveryCommit(t *testing.T) {
	var want []string
	total := func() int {
		s := &countingStore{Store: newBackend(t, "memstore").open()}
		createLedgerRun(t, s)
		if _, err := activateWith(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s), scripted(ledgerScript), 0); err != nil {
			t.Fatal(err)
		}
		want = lastTranscript(t, s)
		return s.n
	}()
	// The memory store only: storetest's TEST-5 runs the same procedure
	// against every store, the file store included.
	for _, name := range []string{"memstore"} {
		for at := 1; at <= total; at++ {
			for _, landing := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/commit-%d/landing-%v", name, at, landing), func(t *testing.T) {
					t.Parallel()
					b := newBackend(t, name)
					s := b.open()
					createLedgerRun(t, s)
					reg := registry(t, ledgerType(&execLog{}))
					cs := &crashStore{Store: s, at: at, landing: landing}
					_, err := activateWith(context.Background(), agent.NewController(cs, reg), claim(t, s), scripted(ledgerScript), 0)
					if !errors.Is(err, agent.ErrStoreFailure) {
						t.Fatalf("crashed activation = %v, want a store failure", err)
					}
					if b.durable {
						s.Close()
						s = b.open()
					}
					defer s.Close()
					// The host rereads the run record (ADR 0003, item 14):
					// the final commit may have landed.
					if run := runRecord(t, s, "r1"); !run.Status.Terminal() {
						releaseIfOwned(t, s)
						out, err := activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), scripted(ledgerScript), 0)
						if err != nil {
							t.Fatal(err)
						}
						if out.Status != store.StatusSucceeded || out.Reason.Code != agent.ReasonChanged {
							t.Fatalf("outcome = %+v", out)
						}
					} else if run.Status != store.StatusSucceeded || at != total || !landing {
						t.Fatalf("run = %+v after commit %d", run, at)
					}
					if got := ledgerItems(t, s); !slices.Equal(got, []string{"a", "b", "c"}) {
						t.Fatalf("ledger = %v", got)
					}
					if n := countKind(entries(t, s, "r1"), agent.EntryTurn); n != len(ledgerScript) {
						t.Fatalf("%d turns recorded, want %d: a recorded turn was asked again", n, len(ledgerScript))
					}
					// The transcript is the uninterrupted run's, message for
					// message: a call recovery refuses or resolves reads as
					// the loop's own would (ADR 0008, item 13).
					if got := lastTranscript(t, s); !slices.Equal(got, want) {
						t.Fatalf("transcript = %v, want the uninterrupted run's %v", got, want)
					}
				})
			}
		}
	}
}

// lastTranscript returns the message artifacts of the latest checkpoint
// that holds a model state's progress.
func lastTranscript(t testing.TB, s store.Store) []string {
	t.Helper()
	ck, ok, err := s.LatestCheckpoint(context.Background(), inst)
	if err != nil || !ok {
		t.Fatalf("LatestCheckpoint = %v, %v", ok, err)
	}
	for seq := ck.Seq; seq > 0; seq-- {
		c, err := s.Checkpoint(context.Background(), inst, seq)
		if err != nil {
			t.Fatalf("Checkpoint %d: %v", seq, err)
		}
		var st struct {
			Model *struct {
				Transcript []string `json:"transcript"`
			} `json:"model"`
		}
		if err := json.Unmarshal(c.State, &st); err != nil {
			t.Fatal(err)
		}
		if st.Model != nil {
			return st.Model.Transcript
		}
	}
	t.Fatal("no checkpoint holds a model state's progress")
	return nil
}

func ownershipFence(t testing.TB, s store.Store) store.Fence {
	t.Helper()
	own, err := s.Ownership(context.Background(), inst)
	if err != nil {
		t.Fatal(err)
	}
	return own.Fence()
}

// releaseIfOwned gives up a crashed activation's claim, as its host does
// after a store failure (ADR 0003, item 14).
func releaseIfOwned(t testing.TB, s store.Store) {
	t.Helper()
	if f := ownershipFence(t, s); f.Owner != "" {
		if err := s.Release(context.Background(), f); err != nil {
			t.Fatal(err)
		}
	}
}

func countKind(es []store.Entry, kind string) int {
	n := 0
	for _, e := range es {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func TestModelStateStepLimit(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	reg := registry(t, ledgerType(&execLog{}))
	var steps []uint64
	for range 10 {
		out, err := activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), scripted(ledgerScript), 1)
		if err != nil {
			t.Fatal(err)
		}
		steps = append(steps, out.Steps)
		if out.Status.Terminal() {
			if out.Status != store.StatusSucceeded {
				t.Fatalf("outcome = %+v", out)
			}
			break
		}
		if out.Reason.Code != agent.ReasonStepLimit {
			t.Fatalf("outcome = %+v, want the step limit", out)
		}
	}
	if !slices.Equal(steps, []uint64{1, 2, 3, 4, 5}) {
		t.Fatalf("steps per activation = %v, want one each", steps)
	}
	if got := ledgerItems(t, s); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("ledger = %v", got)
	}
}

func TestModelStatePolicy(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	var asked []string
	deny := agent.WithPolicy(func(_ context.Context, req *agent.PolicyRequest) (agent.Decision, error) {
		asked = append(asked, fmt.Sprintf("%s/%s/%s", req.State, req.Tool, req.Effect))
		if req.Tool == "record" && strings.Contains(string(req.Call.Parameters), `"c"`) {
			return agent.Decision{Verdict: agent.VerdictDeny, Reason: "no c"}, nil
		}
		return agent.Decision{Verdict: agent.VerdictAllow}, nil
	})
	client := scripted(ledgerScript)
	out, err := activateWith(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{})), deny), claim(t, s), client, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Without c the review rejects the ledger.
	if out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonInvalidOutput {
		t.Fatalf("outcome = %+v", out)
	}
	if got := ledgerItems(t, s); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("ledger = %v", got)
	}
	if len(asked) != 5 || asked[0] != "work/lookup/read" {
		t.Fatalf("policy asked %v", asked)
	}
	third := client.Calls()[2].Messages
	r := third[len(third)-1].Content[1].ToolResult
	if r == nil || !r.IsError || !strings.Contains(r.Content, "denied") || !strings.Contains(r.Content, "no c") {
		t.Fatalf("denied result = %+v", r)
	}
	es := entries(t, s, "r1")
	var perms []agent.Permission
	for _, e := range es {
		if e.Kind == agent.EntryPermission {
			perms = append(perms, payload[agent.PermissionEntry](t, e).Permission)
		}
	}
	if !slices.Contains(perms, agent.PermissionDenied) {
		t.Fatalf("permissions = %v", perms)
	}
}

func TestModelStatePolicyPendingAndError(t *testing.T) {
	for name, policy := range map[string]agent.Policy{
		"pending": func(context.Context, *agent.PolicyRequest) (agent.Decision, error) {
			return agent.Decision{Verdict: agent.VerdictPending}, nil
		},
		"error": func(context.Context, *agent.PolicyRequest) (agent.Decision, error) {
			return agent.Decision{}, errors.New("policy down")
		},
		"bad verdict": func(context.Context, *agent.PolicyRequest) (agent.Decision, error) {
			return agent.Decision{Verdict: "maybe"}, nil
		},
		"invalid reason": func(context.Context, *agent.PolicyRequest) (agent.Decision, error) {
			return agent.Decision{Verdict: agent.VerdictDeny, Reason: "\xff"}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newBackend(t, "memstore").open()
			createLedgerRun(t, s)
			out, err := activateWith(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{})), agent.WithPolicy(policy)), claim(t, s), scripted(ledgerScript), 0)
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonToolError {
				t.Fatalf("outcome = %+v", out)
			}
		})
	}
}

func TestModelStateWrapUpFailed(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	client := scripted([]llms.ChatResponse{textResp("hello"), textResp("still hello"), textResp("never")})
	out, err := activateWith(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s), client, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonWrapUpFailed || out.Steps != 2 {
		t.Fatalf("outcome = %+v", out)
	}
	if n := len(client.Calls()); n != 2 {
		t.Fatalf("model asked %d times, want 2 (the answer and the nudged answer)", n)
	}
}

func TestModelStateEmptyResponseIsNudged(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	empty := llms.ChatResponse{Message: llms.Message{Role: llms.RoleAssistant}, StopReason: "max_tokens"}
	n := 0
	client := &testllm.FakeChatClient{Func: func(_ context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
		n++
		if n == 1 {
			return empty, nil
		}
		if last := req.Messages[len(req.Messages)-1]; last.Content[0].Text != agent.DefaultNudge {
			return llms.ChatResponse{}, fmt.Errorf("request %d ends with %+v", n, last)
		}
		return callsResp(tc("x", "finish", `{}`)), nil
	}}
	out, err := activateWith(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s), client, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The finish transition reaches review, which rejects the empty ledger.
	if out.Reason.Code != agent.ReasonInvalidOutput {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestModelStateModelError(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	client := &testllm.FakeChatClient{Err: &llms.ProviderError{Provider: "fake", Kind: llms.ErrorKindAuth, StatusCode: 401, Message: "bad key"}}
	out, err := activateWith(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s), client, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonModelError || !strings.Contains(out.Detail, "bad key") {
		t.Fatalf("outcome = %+v", out)
	}
	es := entries(t, s, "r1")
	if got := kinds(es); !slices.Equal(got, []string{"activation_started", "run_started", "model_attempt", "model_failure", "outcome"}) {
		t.Fatalf("record = %v", got)
	}
	if f := payload[agent.ModelFailureEntry](t, es[3]); f.Kind != "auth" || f.Status != 401 {
		t.Fatalf("failure = %+v", f)
	}
}

func TestModelStateNoClient(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	out, err := activateWith(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Reason.Code != agent.ReasonModelError || !strings.Contains(out.Detail, "no model client") {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestModelStateCancelled(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	ctx, cancel := context.WithCancelCause(context.Background())
	execs := &execLog{}
	tools := ledgerTools(execs)
	tools[1].Execute = func(ctx context.Context, c *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
		cancel(agent.ErrOperator)
		return recordItem(execs)(ctx, c)
	}
	out, err := activateWith(ctx, agent.NewController(s, registry(t, ledgerType(execs, tools...))), claim(t, s), scripted(ledgerScript), 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != store.StatusCancelled || out.Reason.Code != agent.ReasonOperator {
		t.Fatalf("outcome = %+v", out)
	}
	// The write's result after cancellation is discarded: an atomic local
	// transaction has no effect without its result.
	if got := ledgerItems(t, s); len(got) != 0 {
		t.Fatalf("ledger = %v", got)
	}
}

func TestModelStateShutdownResumes(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	reg := registry(t, ledgerType(&execLog{}))
	ctx, cancel := context.WithCancel(context.Background())
	client := scripted(ledgerScript)
	inner := client.Func
	client.Func = func(c context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
		if len(client.Calls()) == 2 {
			cancel()
			return llms.ChatResponse{}, c.Err()
		}
		return inner(c, req)
	}
	out, err := activateWith(ctx, agent.NewController(s, reg), claim(t, s), client, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != store.StatusRunnable || out.Reason.Code != agent.ReasonShutdown {
		t.Fatalf("outcome = %+v", out)
	}
	out, err = activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), scripted(ledgerScript), 0)
	if err != nil || out.Status != store.StatusSucceeded {
		t.Fatalf("resumed = %+v, %v", out, err)
	}
	if got := ledgerItems(t, s); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("ledger = %v", got)
	}
}

// TestModelStateUnknownWrite: a write that is not an atomic local
// transaction, attempted with no result, has an unknown outcome; the run
// fails rather than repeating it (ADR 0004, item 11).
func TestModelStateUnknownWrite(t *testing.T) {
	execs := &execLog{}
	tools := ledgerTools(execs)
	sent := 0
	tools[1] = agent.Tool[ledgerData]{
		Definition: llms.ToolDefinition{Name: "record"},
		Effect:     agent.EffectWrite,
		Contract:   agent.ContractIdempotent,
		Execute: func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
			sent++
			return agent.TextResult[ledgerData]("sent"), nil
		},
	}
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	reg := registry(t, ledgerType(execs, tools...))
	// Commits: activation, run start, attempt, turn, lookup's permission,
	// attempt and result, record's permission, record's attempt (9), and
	// its result (10), which dies after the effect.
	cs := &crashStore{Store: s, at: 10}
	if _, err := activateWith(context.Background(), agent.NewController(cs, reg), claim(t, s), scripted(ledgerScript), 0); !errors.Is(err, agent.ErrStoreFailure) {
		t.Fatalf("crashed activation = %v", err)
	}
	if err := s.Release(context.Background(), ownershipFence(t, s)); err != nil {
		t.Fatal(err)
	}
	out, err := activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), scripted(ledgerScript), 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonToolError || !strings.Contains(out.Detail, "unknown") {
		t.Fatalf("outcome = %+v", out)
	}
	if sent != 1 {
		t.Fatalf("write sent %d times, want once", sent)
	}
	es := entries(t, s, "r1")
	if r := payload[agent.ResultEntry](t, es[len(es)-2]); r.Status != agent.ResultUnknown {
		t.Fatalf("result = %+v", r)
	}
}

// TestModelStateReissue: a read and an atomic local transaction attempted
// with no result are issued again, with the same action identity and
// idempotency key (STM-6, TOOL-5).
func TestModelStateReissue(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	execs := &execLog{}
	reg := registry(t, ledgerType(execs))
	// Commit 10 is the record call's result: the call ran, its result did
	// not land.
	cs := &crashStore{Store: s, at: 10}
	if _, err := activateWith(context.Background(), agent.NewController(cs, reg), claim(t, s), scripted(ledgerScript), 0); !errors.Is(err, agent.ErrStoreFailure) {
		t.Fatalf("crashed activation = %v", err)
	}
	firstKey := execs.keys["1.1"]
	if err := s.Release(context.Background(), ownershipFence(t, s)); err != nil {
		t.Fatal(err)
	}
	out, err := activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), scripted(ledgerScript), 0)
	if err != nil || out.Status != store.StatusSucceeded {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
	if n := slices.Index(execs.calls, "record#1.1"); n < 0 || execs.calls[n+1] != "record#1.1" {
		t.Fatalf("executions = %v, want record#1.1 issued twice in a row", execs.calls)
	}
	if firstKey == "" || execs.keys["1.1"] != firstKey {
		t.Fatalf("idempotency key changed from %q to %q", firstKey, execs.keys["1.1"])
	}
	attempts := 0
	for _, e := range entries(t, s, "r1") {
		if e.Kind == agent.EntryAttempt && payload[agent.AttemptEntry](t, e).Action.Step == 1 && payload[agent.AttemptEntry](t, e).Call == 1 {
			attempts++
		}
	}
	if attempts != 2 {
		t.Fatalf("%d attempt entries for the reissued call, want 2", attempts)
	}
}

func TestModelStateToolDefects(t *testing.T) {
	cases := map[string]agent.Tool[ledgerData]{
		"execute error": {Effect: agent.EffectRead, Execute: func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
			return agent.Result[ledgerData]{}, errors.New("boom")
		}},
		"panic": {Effect: agent.EffectRead, Execute: func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
			panic("kaboom")
		}},
		"ops from a read": {Effect: agent.EffectRead, Execute: func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
			return agent.Result[ledgerData]{Ops: []store.Op{store.DeleteObjectOp(ledgerNS, "k", 0)}}, nil
		}},
		"undeclared transition": {Effect: agent.EffectRead, Execute: func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
			return agent.Result[ledgerData]{To: "done"}, nil
		}},
		"error transition": {Effect: agent.EffectRead, Execute: func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
			return agent.Result[ledgerData]{To: "review", IsError: true}, nil
		}},
		"content and parts": {Effect: agent.EffectRead, Execute: func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
			return agent.Result[ledgerData]{Content: "x", Parts: []llms.ContentPart{llms.Text("y")}}, nil
		}},
		"empty text part": {Effect: agent.EffectRead, Execute: func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
			return agent.Result[ledgerData]{Parts: []llms.ContentPart{llms.Text("")}}, nil
		}},
		"empty text part on a terminal path": {Effect: agent.EffectRead, Execute: func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
			return agent.Result[ledgerData]{Parts: []llms.ContentPart{llms.Text("")}, To: "review"}, nil
		}},
		"invalid media type": {Effect: agent.EffectRead, Execute: func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
			return agent.Result[ledgerData]{Parts: []llms.ContentPart{llms.Document("pdf", []byte("x"))}}, nil
		}},
		"image with a document type": {Effect: agent.EffectRead, Execute: func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
			return agent.Result[ledgerData]{Parts: []llms.ContentPart{llms.Image("application/pdf", []byte("x"))}}, nil
		}},
		"invalid UTF-8": {Effect: agent.EffectRead, Execute: func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
			return agent.Result[ledgerData]{Content: "\xff"}, nil
		}},
		"record op kind": {Effect: agent.EffectWrite, Contract: agent.ContractAtomicLocal, Execute: func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
			return agent.Result[ledgerData]{Ops: []store.Op{store.ReleaseOp()}}, nil
		}},
	}
	for name, tool := range cases {
		t.Run(name, func(t *testing.T) {
			tool.Definition = llms.ToolDefinition{Name: "lookup"}
			tools := ledgerTools(&execLog{})
			tools[0] = tool
			s := newBackend(t, "memstore").open()
			createLedgerRun(t, s)
			out, err := activateWith(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}, tools...))), claim(t, s), scripted(ledgerScript), 0)
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonToolError {
				t.Fatalf("outcome = %+v", out)
			}
		})
	}
}

// A tool whose store read fails ends the activation with a store failure,
// not a run failure (ADR 0003, item 15).
func TestModelStateToolReadFailure(t *testing.T) {
	s := failingObjects{newBackend(t, "memstore").open()}
	createLedgerRun(t, s)
	_, err := activateWith(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s), scripted(ledgerScript), 0)
	if !errors.Is(err, agent.ErrStoreFailure) || !errors.Is(err, errDisk) {
		t.Fatalf("Activate = %v, want a store failure wrapping the read's", err)
	}
	if run := runRecord(t, s, "r1"); run.Status != store.StatusRunnable {
		t.Fatalf("run = %+v, want runnable", run)
	}
}

// failingObjects fails object reads, as a broken store would.
type failingObjects struct{ store.Store }

func (failingObjects) Object(context.Context, string, string) (store.Object, bool, error) {
	return store.Object{}, false, errDisk
}

func TestModelStateMediaResult(t *testing.T) {
	tools := ledgerTools(&execLog{})
	tools[0].Execute = func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
		return agent.Result[ledgerData]{Parts: []llms.ContentPart{llms.Text("page 1"), llms.Document("application/pdf", []byte("%PDF-1.7"))}}, nil
	}
	s := newBackend(t, "filestore").open()
	defer s.Close()
	createLedgerRun(t, s)
	client := scripted(ledgerScript)
	if _, err := activateWith(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}, tools...))), claim(t, s), client, 0); err != nil {
		t.Fatal(err)
	}
	second := client.Calls()[1].Messages
	r := second[len(second)-1].Content[0].ToolResult
	if len(r.Parts) != 2 || r.Parts[1].Media == nil || string(r.Parts[1].Media.Data) != "%PDF-1.7" {
		t.Fatalf("result parts = %+v", r.Parts)
	}
}

// TestModelStateWriteConflict: an atomic local transaction whose operations
// conflict runs again against fresh reads (ADR 0005, item 10); one that
// keeps conflicting becomes a tool error the model receives.
func TestModelStateWriteConflict(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprintf("persistent-%v", persistent), func(t *testing.T) {
			s := newBackend(t, "memstore").open()
			createLedgerRun(t, s)
			execs := &execLog{}
			tools := ledgerTools(execs)
			runs := 0
			other, err := s.Claim(context.Background(), "inst-2", "other", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			// Another instance's commit lands between the tool's read and
			// its commit, each time or once.
			tools[1].Execute = func(ctx context.Context, c *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
				res, rerr := recordItem(execs)(ctx, c)
				runs++
				if persistent || runs == 1 {
					items, v, _ := ledger(ctx, c.Env)
					raw, _ := json.Marshal(append(items, "other"))
					if cerr := s.Commit(ctx, other.Fence(), []store.Op{store.PutObjectJSON(ledgerNS, c.Env.RunID(), v, raw)}); cerr != nil {
						return res, cerr
					}
				}
				return res, rerr
			}
			client := scripted(ledgerScript[:2])
			out, err := activateWith(context.Background(), agent.NewController(s, registry(t, ledgerType(execs, tools...)), agent.WithConflictRetries(2)), claim(t, s), client, 2)
			if err != nil || out.Reason.Code != agent.ReasonStepLimit {
				t.Fatalf("outcome = %+v, %v; want the step limit after two turns", out, err)
			}
			items := ledgerItems(t, s)
			if persistent {
				if slices.Contains(items, "a") {
					t.Fatalf("ledger = %v; a persistent conflict wrote", items)
				}
				second := client.Calls()[1].Messages
				r := second[len(second)-1].Content[1].ToolResult
				if !r.IsError || !strings.Contains(r.Content, "conflicted") {
					t.Fatalf("result = %+v", r)
				}
				return
			}
			if !slices.Equal(items, []string{"other", "a", "b", "c"}) {
				t.Fatalf("ledger = %v", items)
			}
		})
	}
}

func TestIdempotencyKey(t *testing.T) {
	a := agent.ActionID{Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}, Step: 2, Call: 1}
	b := a
	b.Call = 2
	c := agent.ActionID{Scope: store.Scope{Kind: store.ScopeRun, ID: "r"}, Step: 12, Call: 1}
	ka, ka2 := a.IdempotencyKey(), a.IdempotencyKey()
	if ka != ka2 || ka == b.IdempotencyKey() || ka == c.IdempotencyKey() {
		t.Fatal("keys are not stable and distinct")
	}
	if len(a.IdempotencyKey()) != 64 {
		t.Fatalf("key %q is not hex SHA-256", a.IdempotencyKey())
	}
}

func TestRegisterModelState(t *testing.T) {
	read := agent.Tool[ledgerData]{Definition: llms.ToolDefinition{Name: "t"}, Effect: agent.EffectRead,
		Execute: func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
			return agent.Result[ledgerData]{}, nil
		}}
	prompt := func(context.Context, *agent.Env, ledgerData) (llms.ChatRequest, error) {
		return llms.ChatRequest{}, nil
	}
	mk := func(ms *agent.ModelState[ledgerData], code bool) *agent.Type[ledgerData] {
		st := agent.State[ledgerData]{Name: "m", Next: []string{"end"}, Model: ms}
		if code {
			st.Code = func(context.Context, *agent.Env, ledgerData) (agent.Transition[ledgerData], error) {
				return agent.Transition[ledgerData]{}, nil
			}
		}
		return &agent.Type[ledgerData]{Name: "x", Initial: "m", States: []agent.State[ledgerData]{st, {Name: "end", End: agent.EndChanged}}}
	}
	with := func(f func(*agent.Tool[ledgerData])) *agent.ModelState[ledgerData] {
		tool := read
		f(&tool)
		return &agent.ModelState[ledgerData]{Prompt: prompt, Tools: []agent.Tool[ledgerData]{tool}, WrapUp: agent.WrapUp{Tools: []string{"t"}}}
	}
	cases := map[string]*agent.Type[ledgerData]{
		"code and model":      mk(&agent.ModelState[ledgerData]{Prompt: prompt}, true),
		"no prompt":           mk(&agent.ModelState[ledgerData]{}, false),
		"unnamed tool":        mk(with(func(t *agent.Tool[ledgerData]) { t.Definition.Name = "" }), false),
		"no execute":          mk(with(func(t *agent.Tool[ledgerData]) { t.Execute = nil }), false),
		"no effect":           mk(with(func(t *agent.Tool[ledgerData]) { t.Effect = "" }), false),
		"read with contract":  mk(with(func(t *agent.Tool[ledgerData]) { t.Contract = agent.ContractIdempotent }), false),
		"write without":       mk(with(func(t *agent.Tool[ledgerData]) { t.Effect = agent.EffectWrite }), false),
		"unknown contract":    mk(with(func(t *agent.Tool[ledgerData]) { t.Effect, t.Contract = agent.EffectWrite, "hope" }), false),
		"invalid nudge":       mk(&agent.ModelState[ledgerData]{Prompt: prompt, Nudge: "\xff"}, false),
		"invalid description": mk(with(func(t *agent.Tool[ledgerData]) { t.Definition.Description = "\xff" }), false),
		"invalid schema":      mk(with(func(t *agent.Tool[ledgerData]) { t.Definition.InputSchema = json.RawMessage(`{`) }), false),
		"duplicate tool":      mk(&agent.ModelState[ledgerData]{Prompt: prompt, Tools: []agent.Tool[ledgerData]{read, read}}, false),
	}
	for name, typ := range cases {
		if err := agent.NewRegistry().Register(typ); !errors.Is(err, agent.ErrInvalidType) {
			t.Errorf("%s: Register = %v, want ErrInvalidType", name, err)
		}
	}
	ok := mk(with(func(t *agent.Tool[ledgerData]) { t.Effect, t.Contract = agent.EffectWrite, agent.ContractUnprotected }), false)
	if err := agent.NewRegistry().Register(ok); err != nil {
		t.Fatalf("Register = %v", err)
	}
}

// Tool arguments are recorded byte for byte, so a call reissued by
// recovery has the digest its attempt recorded, whatever the arguments'
// spacing, and malformed arguments reach the tool rather than failing the
// record (ADR 0008, item 8).
func TestModelStateArgumentsRecordedExactly(t *testing.T) {
	for name, args := range map[string]string{"spaced": `{ "item" : "a" }`, "malformed": `{"item": `} {
		t.Run(name, func(t *testing.T) {
			script := slices.Clone(ledgerScript)
			script[0] = callsResp(tc("c1", "lookup", `{"key": "a"}`), tc("c2", "record", args), tc("c3", "bogus", `{}`))
			s := newBackend(t, "memstore").open()
			createLedgerRun(t, s)
			execs := &execLog{}
			reg := registry(t, ledgerType(execs))
			// Commit 10 is the record call's result.
			cs := &crashStore{Store: s, at: 10}
			if _, err := activateWith(context.Background(), agent.NewController(cs, reg), claim(t, s), scripted(script), 0); !errors.Is(err, agent.ErrStoreFailure) {
				t.Fatalf("crashed activation = %v", err)
			}
			releaseIfOwned(t, s)
			client := scripted(script)
			out, err := activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), client, 0)
			if err != nil {
				t.Fatal(err)
			}
			if n := slices.Index(execs.calls, "record#1.1"); n < 0 || execs.calls[n+1] != "record#1.1" {
				t.Fatalf("executions = %v, want record#1.1 reissued", execs.calls)
			}
			got := client.Calls()[0].Messages[1].Content[1].ToolCall.Parameters
			if string(got) != args {
				t.Fatalf("restored transcript holds arguments %q, want %q", got, args)
			}
			switch name {
			case "spaced":
				if out.Status != store.StatusSucceeded {
					t.Fatalf("outcome = %+v", out)
				}
			case "malformed":
				// The tool rejected the arguments; a is never recorded.
				if out.Reason.Code != agent.ReasonInvalidOutput || slices.Contains(ledgerItems(t, s), "a") {
					t.Fatalf("outcome = %+v, ledger %v", out, ledgerItems(t, s))
				}
			}
		})
	}
}

// A policy failure ends the run in a commit that also carries the results
// toolloop synthesized earlier in the turn (ADR 0008, item 13).
func TestModelStatePolicyFailureKeepsRefusals(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	script := []llms.ChatResponse{callsResp(tc("c1", "bogus", `{}`), tc("c2", "record", `{"item":"a"}`))}
	policy := agent.WithPolicy(func(context.Context, *agent.PolicyRequest) (agent.Decision, error) {
		return agent.Decision{}, errors.New("policy down")
	})
	out, err := activateWith(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{})), policy), claim(t, s), scripted(script), 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Reason.Code != agent.ReasonToolError {
		t.Fatalf("outcome = %+v", out)
	}
	es := entries(t, s, "r1")
	want := []string{"activation_started", "run_started", "model_attempt", "turn", "result", "outcome"}
	if got := kinds(es); !slices.Equal(got, want) {
		t.Fatalf("record = %v, want %v", got, want)
	}
	if r := payload[agent.ResultEntry](t, es[4]); r.Status != agent.ResultRefused || r.Call != 0 {
		t.Fatalf("result = %+v", r)
	}
}

// A turn's refused result commits with the next wrapper commit, and an
// activation that shuts down mid-turn leaves a tail holding it, from which
// the next activation continues the turn.
func TestModelStateShutdownWithPendingRefusal(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	reg := registry(t, ledgerType(&execLog{}))
	ctx, cancel := context.WithCancel(context.Background())
	tools := ledgerTools(&execLog{})
	// The lookup after the unknown call cancels the activation; its result
	// is discarded.
	tools[0].Execute = func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
		cancel()
		return agent.TextResult[ledgerData]("x"), nil
	}
	script := slices.Clone(ledgerScript)
	script[0] = callsResp(tc("c2", "record", `{"item":"a"}`), tc("c3", "bogus", `{}`), tc("c1", "lookup", `{}`))
	out, err := activateWith(ctx, agent.NewController(s, registry(t, ledgerType(&execLog{}, tools...))), claim(t, s), scripted(script), 0)
	if err != nil || out.Reason.Code != agent.ReasonShutdown {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
	// The refusal rides on the next commit, the lookup's permission; the
	// tail the next activation reads holds it.
	es := entries(t, s, "r1")
	if r := payload[agent.ResultEntry](t, es[7]); es[7].Kind != agent.EntryResult || r.Status != agent.ResultRefused || r.Call != 1 {
		t.Fatalf("record = %v; want the refusal before the lookup's permission", kinds(es))
	}
	out, err = activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), scripted(script), 0)
	if err != nil || out.Status != store.StatusSucceeded {
		t.Fatalf("resumed = %+v, %v", out, err)
	}
}

func TestModelStateInvalidPromptRequest(t *testing.T) {
	for name, mutate := range map[string]func(*llms.ChatRequest){
		"message": func(r *llms.ChatRequest) { r.Messages = []llms.Message{llms.UserText("\xff")} },
		"system":  func(r *llms.ChatRequest) { r.System = []llms.ContentPart{llms.Text("\xff")} },
		"role":    func(r *llms.ChatRequest) { r.Messages[0].Role = "user\xff" },
		"cyclic provider params": func(r *llms.ChatRequest) {
			m := map[string]any{}
			m["self"] = m
			r.ProviderParams = map[llms.ProviderID]map[string]any{"p": m}
		},
	} {
		t.Run(name, func(t *testing.T) {
			typ := ledgerType(&execLog{})
			typ.States[0].Model.Prompt = func(context.Context, *agent.Env, ledgerData) (llms.ChatRequest, error) {
				r := llms.ChatRequest{Messages: []llms.Message{llms.UserText("items for r1")}}
				mutate(&r)
				return r, nil
			}
			s := newBackend(t, "memstore").open()
			createLedgerRun(t, s)
			out, err := activateWith(context.Background(), agent.NewController(s, registry(t, typ)), claim(t, s), scripted(ledgerScript), 0)
			if err != nil || out.Reason.Code != agent.ReasonStateError {
				t.Fatalf("outcome = %+v, %v; want state_error", out, err)
			}
		})
	}
}

// The parameters sent are the recorded ones: a live run and its recovery
// send the same, whatever encoding/json normalizes (ADR 0008, item 8).
func TestModelStateParamsSentAsRecorded(t *testing.T) {
	typ := ledgerType(&execLog{})
	typ.States[0].Model.Prompt = func(context.Context, *agent.Env, ledgerData) (llms.ChatRequest, error) {
		return llms.ChatRequest{
			Messages: []llms.Message{llms.UserText("items for r1")},
			Metadata: map[string]string{"k": "a\xffb"},
			ProviderParams: map[llms.ProviderID]map[string]any{"p": {
				"list": []string{"x\xff"}, "big": int64(1) << 60,
			}},
		}, nil
	}
	reg := registry(t, typ)
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	live := scripted(ledgerScript)
	// Commit 5 is the lookup's permission: the first turn is recorded.
	if _, err := activateWith(context.Background(), agent.NewController(&crashStore{Store: s, at: 5}, reg), claim(t, s), live, 0); !errors.Is(err, agent.ErrStoreFailure) {
		t.Fatalf("crashed activation = %v", err)
	}
	releaseIfOwned(t, s)
	recovered := scripted(ledgerScript)
	if out, err := activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), recovered, 0); err != nil || out.Status != store.StatusSucceeded {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
	first, again := live.Calls()[0], recovered.Calls()[0]
	if first.Metadata["k"] != again.Metadata["k"] {
		t.Fatalf("metadata sent live %q, after recovery %q", first.Metadata["k"], again.Metadata["k"])
	}
	a, _ := json.Marshal(first.ProviderParams)
	b, _ := json.Marshal(again.ProviderParams)
	if string(a) != string(b) || !strings.Contains(string(a), "1152921504606846976") {
		t.Fatalf("provider params sent live %s, after recovery %s", a, b)
	}
}

// TestModelStateCancelledDuringWriteRead: a write whose store read the
// activation's end interrupted ends the activation by the context's cause,
// not as a store failure (ADR 0007, item 14).
func TestModelStateCancelledDuringWriteRead(t *testing.T) {
	execs := &execLog{}
	tools := ledgerTools(execs)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	tools[1] = agent.Tool[ledgerData]{
		Definition: llms.ToolDefinition{Name: "record"},
		Effect:     agent.EffectWrite,
		Contract:   agent.ContractIdempotent,
		Execute: func(ctx context.Context, c *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
			cancel(agent.ErrOperator)
			if _, _, err := ledger(ctx, c.Env); err != nil {
				return agent.Result[ledgerData]{}, err
			}
			return agent.Result[ledgerData]{}, errors.New("the read was not interrupted")
		},
	}
	s := ctxObjects{newBackend(t, "memstore").open()}
	createLedgerRun(t, s)
	out, err := activateWith(ctx, agent.NewController(s, registry(t, ledgerType(execs, tools...))), claim(t, s), scripted(ledgerScript), 0)
	if err != nil {
		t.Fatalf("Activate = %v, want the operator's cancellation", err)
	}
	if out.Status != store.StatusCancelled || out.Reason.Code != agent.ReasonOperator {
		t.Fatalf("outcome = %+v", out)
	}
	if run := runRecord(t, s, "r1"); run.Status != store.StatusCancelled {
		t.Fatalf("run = %+v, want cancelled", run)
	}
}

// ctxObjects fails an object read whose context has ended, as a store
// that honours its context does.
type ctxObjects struct{ store.Store }

func (s ctxObjects) Object(ctx context.Context, ns, key string) (store.Object, bool, error) {
	if err := ctx.Err(); err != nil {
		return store.Object{}, false, err
	}
	return s.Store.Object(ctx, ns, key)
}

// TestModelStateRecoveredWriteOutlivesItsTool: a write attempted with no
// result is resolved by the effect and contract its attempt recorded, not
// by what the recovering version declares: removing the tool, or
// redeclaring it as an atomic local transaction, leaves its outcome
// unknown (ADR 0008, item 17).
func TestModelStateRecoveredWriteOutlivesItsTool(t *testing.T) {
	for name, redeclare := range map[string]func([]agent.Tool[ledgerData]) []agent.Tool[ledgerData]{
		"removed": func(tools []agent.Tool[ledgerData]) []agent.Tool[ledgerData] {
			return []agent.Tool[ledgerData]{tools[0], tools[2]}
		},
		"now atomic local": func(tools []agent.Tool[ledgerData]) []agent.Tool[ledgerData] {
			tools[1].Contract = agent.ContractAtomicLocal
			return tools
		},
	} {
		t.Run(name, func(t *testing.T) {
			sent := 0
			tools := func() []agent.Tool[ledgerData] {
				tools := ledgerTools(&execLog{})
				tools[1] = agent.Tool[ledgerData]{
					Definition: llms.ToolDefinition{Name: "record"},
					Effect:     agent.EffectWrite,
					Contract:   agent.ContractIdempotent,
					Execute: func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
						sent++
						return agent.TextResult[ledgerData]("sent"), nil
					},
				}
				return tools
			}
			s := newBackend(t, "memstore").open()
			createLedgerRun(t, s)
			// Commit 10 is the record call's result: the write was sent,
			// its result did not land.
			cs := &crashStore{Store: s, at: 10}
			if _, err := activateWith(context.Background(), agent.NewController(cs, registry(t, ledgerType(&execLog{}, tools()...))), claim(t, s), scripted(ledgerScript), 0); !errors.Is(err, agent.ErrStoreFailure) {
				t.Fatalf("crashed activation = %v", err)
			}
			releaseIfOwned(t, s)
			reg := registry(t, ledgerType(&execLog{}, redeclare(tools())...))
			out, err := activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), scripted(ledgerScript), 0)
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonToolError || !strings.Contains(out.Detail, "unknown") {
				t.Fatalf("outcome = %+v", out)
			}
			if sent != 1 {
				t.Fatalf("write sent %d times, want once", sent)
			}
			es := entries(t, s, "r1")
			if r := payload[agent.ResultEntry](t, es[len(es)-2]); r.Status != agent.ResultUnknown || r.Tool != "record" {
				t.Fatalf("result = %+v", r)
			}
		})
	}
}

// Registration keeps its own copy of a tool's input schema: changing the
// declaration's bytes afterwards changes nothing sent (ADR 0007, item 3).
func TestModelStateSchemaIsASnapshot(t *testing.T) {
	tools := ledgerTools(&execLog{})
	schema := json.RawMessage(`{ "type": "object" }`)
	tools[0].Definition.InputSchema = schema
	reg := registry(t, ledgerType(&execLog{}, tools...))
	copy(schema, `{ "type": "string" }`)
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	client := scripted(ledgerScript)
	if _, err := activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), client, 0); err != nil {
		t.Fatal(err)
	}
	if got := string(client.Calls()[0].Tools[0].InputSchema); got != `{"type":"object"}` {
		t.Fatalf("schema sent = %s", got)
	}
}

// TestModelStateChangedToolsRecorded: a version that declares the state's
// tools differently records new parameters with its next attempt, so each
// attempt's recorded parameters are the ones sent (ADR 0008, item 8).
func TestModelStateChangedToolsRecorded(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := scripted(ledgerScript)
	inner := client.Func
	client.Func = func(c context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
		if len(client.Calls()) == 2 {
			cancel()
			return llms.ChatResponse{}, c.Err()
		}
		return inner(c, req)
	}
	if out, err := activateWith(ctx, agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s), client, 0); err != nil || out.Reason.Code != agent.ReasonShutdown {
		t.Fatalf("first activation = %+v, %v", out, err)
	}
	const changed = `{"type":"object","required":["key"]}`
	tools := ledgerTools(&execLog{})
	tools[0].Definition.InputSchema = json.RawMessage(changed)
	resumed := scripted(ledgerScript)
	if out, err := activateWith(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}, tools...))), claim(t, s), resumed, 0); err != nil || out.Status != store.StatusSucceeded {
		t.Fatalf("resumed = %+v, %v", out, err)
	}
	var params []string
	for _, e := range entries(t, s, "r1") {
		if e.Kind == agent.EntryModelAttempt {
			params = append(params, payload[agent.ModelAttemptEntry](t, e).Params)
		}
	}
	schemaOf := func(digest string) string {
		_, raw, err := s.Artifact(context.Background(), digest)
		if err != nil {
			t.Fatal(err)
		}
		var req llms.ChatRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatal(err)
		}
		return string(req.Tools[0].InputSchema)
	}
	if got := schemaOf(params[0]); got != `{"type":"object"}` {
		t.Fatalf("first attempt's schema = %s", got)
	}
	last := params[len(params)-1]
	if got := schemaOf(last); got != changed {
		t.Fatalf("last attempt's schema = %s, want %s", got, changed)
	}
	if got := string(resumed.Calls()[len(resumed.Calls())-1].Tools[0].InputSchema); got != changed {
		t.Fatalf("schema sent = %s", got)
	}
}

// TestModelStateChangedToolsDuringRecoveredTurn: parameters re-recorded
// for changed tools while a recovered turn completes are stored with the
// checkpoint that first references them, so a later activation restores
// them (ADR 0004, item 1).
func TestModelStateChangedToolsDuringRecoveredTurn(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	// Commit 5 is the lookup's permission: the first turn is recorded,
	// its calls unresolved.
	if _, err := activateWith(context.Background(), agent.NewController(&crashStore{Store: s, at: 5}, registry(t, ledgerType(&execLog{}))), claim(t, s), scripted(ledgerScript), 0); !errors.Is(err, agent.ErrStoreFailure) {
		t.Fatalf("crashed activation = %v", err)
	}
	releaseIfOwned(t, s)
	tools := ledgerTools(&execLog{})
	tools[0].Definition.InputSchema = json.RawMessage(`{"type":"object","required":["key"]}`)
	reg := registry(t, ledgerType(&execLog{}, tools...))
	// One step: the recovered turn completes, and its checkpoint is the
	// first to reference the new parameters.
	if out, err := activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), scripted(ledgerScript), 1); err != nil || out.Reason.Code != agent.ReasonStepLimit {
		t.Fatalf("recovering activation = %+v, %v", out, err)
	}
	out, err := activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), scripted(ledgerScript), 0)
	if err != nil || out.Status != store.StatusSucceeded {
		t.Fatalf("next activation = %+v, %v", out, err)
	}
}

// TestModelStateRecoveredCallChangedEffect: a recorded call whose tool a
// later version declares with another effect is refused, not issued under
// the permission granted for the old one (ADR 0008, item 11).
func TestModelStateRecoveredCallChangedEffect(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	// Commit 7 is the lookup's result: the read was attempted, its result
	// did not land.
	if _, err := activateWith(context.Background(), agent.NewController(&crashStore{Store: s, at: 7}, registry(t, ledgerType(&execLog{}))), claim(t, s), scripted(ledgerScript), 0); !errors.Is(err, agent.ErrStoreFailure) {
		t.Fatalf("crashed activation = %v", err)
	}
	releaseIfOwned(t, s)
	execs := &execLog{}
	tools := ledgerTools(execs)
	tools[0].Effect, tools[0].Contract = agent.EffectWrite, agent.ContractAtomicLocal
	var asked []string
	policy := agent.WithPolicy(func(_ context.Context, req *agent.PolicyRequest) (agent.Decision, error) {
		asked = append(asked, fmt.Sprintf("%s/%s", req.Tool, req.Effect))
		return agent.Decision{Verdict: agent.VerdictAllow}, nil
	})
	if _, err := activateWith(context.Background(), agent.NewController(s, registry(t, ledgerType(execs, tools...)), policy), claim(t, s), scripted(ledgerScript), 0); err != nil {
		t.Fatal(err)
	}
	for _, c := range execs.calls {
		if strings.HasPrefix(c, "lookup#1.") {
			t.Fatalf("executions = %v: the redeclared write ran under the read's permission", execs.calls)
		}
	}
	for _, e := range entries(t, s, "r1") {
		if e.Kind != agent.EntryResult {
			continue
		}
		if r := payload[agent.ResultEntry](t, e); r.Call == 0 && r.Tool == "lookup" {
			if r.Status != agent.ResultRefused || !r.IsError {
				t.Fatalf("lookup's result = %+v, want refused", r)
			}
			return
		}
	}
	t.Fatal("no result recorded for the lookup call")
}
