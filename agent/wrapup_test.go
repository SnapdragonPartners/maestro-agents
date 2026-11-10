package agent_test

import (
	"context"
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

// wrapData is the wrap-up fixture's data: which tool left the state, and
// whether its call was in a wrap-up turn.
type wrapData struct {
	By     string `json:"by,omitempty"`
	WrapUp bool   `json:"wrap_up,omitempty"`
}

// wrapLog records the fixture's tool executions.
type wrapLog struct {
	calls []string
	mu    sync.Mutex
}

func (l *wrapLog) add(name string, c *agent.Call[wrapData]) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, fmt.Sprintf("%s/wrap-up=%v", name, c.WrapUp))
}

func (l *wrapLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// wrapType is the wrap-up fixture: a model state with a read tool and two
// exits. submit refuses arguments holding "bad", in a wrap-up turn too;
// escalate always leaves. The prompt states the budget (Env.Budget).
func wrapType(log *wrapLog, w agent.WrapUp) *agent.Type[wrapData] {
	exit := func(name, to string) agent.Tool[wrapData] {
		return agent.Tool[wrapData]{
			Definition: llms.ToolDefinition{Name: name},
			Effect:     agent.EffectRead,
			Execute: func(_ context.Context, c *agent.Call[wrapData]) (agent.Result[wrapData], error) {
				log.add(name, c)
				if strings.Contains(string(c.ToolCall.Parameters), "bad") {
					return agent.ErrorResult[wrapData]("invalid submission"), nil
				}
				return agent.Result[wrapData]{Content: "ok", To: to, Data: wrapData{By: name, WrapUp: c.WrapUp}}, nil
			},
		}
	}
	return &agent.Type[wrapData]{
		Name: "ledger", Version: "1", Initial: "work",
		States: []agent.State[wrapData]{
			{Name: "work", Next: []string{"done", "manual"}, Model: &agent.ModelState[wrapData]{
				Prompt: func(_ context.Context, env *agent.Env, _ wrapData) (llms.ChatRequest, error) {
					return llms.ChatRequest{Messages: []llms.Message{llms.UserText(fmt.Sprint("work ", env.Budget()))}}, nil
				},
				Tools: []agent.Tool[wrapData]{
					{
						Definition: llms.ToolDefinition{Name: "lookup"},
						Effect:     agent.EffectRead,
						Execute: func(_ context.Context, c *agent.Call[wrapData]) (agent.Result[wrapData], error) {
							log.add("lookup", c)
							return agent.TextResult[wrapData]("found"), nil
						},
					},
					exit("submit", "done"),
					exit("escalate", "manual"),
				},
				WrapUp: w,
			}},
			{Name: "done", End: agent.EndChanged},
			{Name: "manual", End: agent.EndNoChange},
		},
	}
}

// wrapClient answers each request with answer(turn, wrapUp), where turn
// counts the transcript's assistant messages and wrapUp reports a request
// that offers only some tools, as a wrap-up turn does.
func wrapClient(answer func(turn int, wrapUp bool) llms.ChatResponse) *testllm.FakeChatClient {
	return &testllm.FakeChatClient{
		ModelRef: llms.ModelRef{Provider: "fake", Name: "wrap"},
		Func: func(_ context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
			n := 0
			for _, m := range req.Messages {
				if m.Role == llms.RoleAssistant {
					n++
				}
			}
			return answer(n, len(req.Tools) < 3), nil
		},
	}
}

func toolNames(req *llms.ChatRequest) []string {
	var out []string
	for _, d := range req.Tools {
		out = append(out, d.Name)
	}
	return out
}

func lastText(req *llms.ChatRequest) string {
	m := req.Messages[len(req.Messages)-1]
	var b strings.Builder
	for _, p := range m.Content {
		b.WriteString(p.Text)
		if p.ToolResult != nil {
			b.WriteString(p.ToolResult.Content)
		}
	}
	return b.String()
}

// countWrapUpMessages counts the wrap-up message in a request.
func countWrapUpMessages(req *llms.ChatRequest) int {
	n := 0
	for _, m := range req.Messages {
		if m.Role == llms.RoleUser && len(m.Content) == 1 && m.Content[0].Text == agent.DefaultWrapUpMessage {
			n++
		}
	}
	return n
}

func attempts(t *testing.T, es []store.Entry) []agent.ModelAttemptEntry {
	t.Helper()
	var out []agent.ModelAttemptEntry
	for _, e := range es {
		if e.Kind == agent.EntryModelAttempt {
			out = append(out, payload[agent.ModelAttemptEntry](t, e))
		}
	}
	return out
}

// TestWrapUpByTurnBudget: with four turns and the default three wrap-up
// turns, the second turn is a wrap-up turn. It offers only the wrap-up's
// tool, with tool choice auto (never forced, ADR 0010 item 5), starts with
// the wrap-up message, and its call executes
// with Call.WrapUp. The prompt and each tool result show the clock.
func TestWrapUpByTurnBudget(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createLedgerRun(t, s)
		log := &wrapLog{}
		client := wrapClient(func(turn int, wrapUp bool) llms.ChatResponse {
			if wrapUp {
				return callsResp(tc(fmt.Sprintf("s%d", turn), "submit", `{}`))
			}
			return callsResp(tc(fmt.Sprintf("l%d", turn), "lookup", `{}`))
		})
		reg := registry(t, wrapType(log, agent.WrapUp{Tools: []string{"submit"}}))
		out, err := activateBudgets(context.Background(), agent.NewController(s, reg), claim(t, s), client, 0, agent.Budgets{Turns: 4})
		if err != nil {
			t.Fatal(err)
		}
		if out.Status != store.StatusSucceeded || out.Reason.Code != agent.ReasonChanged {
			t.Fatalf("outcome = %+v", out)
		}
		calls := client.Calls()
		if len(calls) != 2 {
			t.Fatalf("%d requests, want 2", len(calls))
		}
		if got := toolNames(&calls[0]); !slices.Equal(got, []string{"lookup", "submit", "escalate"}) {
			t.Fatalf("first request offers %v with %+v", got, calls[0].ToolChoice)
		}
		if got := calls[0].Messages[0].Content[0].Text; got != "work [budget: turns 0 of 4 used, wrap-up after 1]" {
			t.Fatalf("prompt = %q", got)
		}
		if got := toolNames(&calls[1]); !slices.Equal(got, []string{"submit"}) ||
			calls[1].ToolChoice != (llms.ToolChoice{Type: llms.ToolChoiceAuto}) {
			t.Fatalf("wrap-up request offers %v with %+v", got, calls[1].ToolChoice)
		}
		if lastText(&calls[1]) != agent.DefaultWrapUpMessage {
			t.Fatalf("wrap-up request ends with %q", lastText(&calls[1]))
		}
		results := calls[1].Messages[len(calls[1].Messages)-2]
		if got := results.Content[0].ToolResult.Content; got != "found\n\n[budget: turns 1 of 4 used, wrap-up after 1]" {
			t.Fatalf("lookup result = %q", got)
		}
		if got := log.get(); !slices.Equal(got, []string{"lookup/wrap-up=false", "submit/wrap-up=true"}) {
			t.Fatalf("executions = %v", got)
		}
		at := attempts(t, entries(t, s, "r1"))
		if len(at) != 2 || at[0].WrapUp || at[0].StateParams != "" || !at[1].WrapUp || at[1].StateParams == "" {
			t.Fatalf("attempts = %+v", at)
		}
	})
}

// TestWrapUpResubmissionsBounded: a wrap-up turn whose exit refuses the
// submission is followed by another, without the message again; after the
// bound the run fails with wrap_up_failed. A turn budget within the
// reserve wraps up from the state's first turn.
func TestWrapUpResubmissionsBounded(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	log := &wrapLog{}
	client := wrapClient(func(turn int, _ bool) llms.ChatResponse {
		return callsResp(tc(fmt.Sprintf("s%d", turn), "submit", `{"bad":true}`))
	})
	reg := registry(t, wrapType(log, agent.WrapUp{Tools: []string{"submit"}}))
	out, err := activateBudgets(context.Background(), agent.NewController(s, reg), claim(t, s), client, 0, agent.Budgets{Turns: 3})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonWrapUpFailed {
		t.Fatalf("outcome = %+v, want wrap_up_failed", out)
	}
	calls := client.Calls()
	if len(calls) != agent.DefaultWrapUpTurns {
		t.Fatalf("%d requests, want %d", len(calls), agent.DefaultWrapUpTurns)
	}
	for i := range calls {
		if n := countWrapUpMessages(&calls[i]); n != 1 {
			t.Fatalf("request %d carries the wrap-up message %d times", i, n)
		}
	}
	for _, at := range attempts(t, entries(t, s, "r1")) {
		if !at.WrapUp {
			t.Fatalf("attempt %+v is not a wrap-up attempt", at)
		}
	}
}

// TestWrapUpTextAnswers: a wrap-up turn that calls no tool is followed by the wrap-up message again, and counts
// toward the bound.
func TestWrapUpTextAnswers(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	client := wrapClient(func(int, bool) llms.ChatResponse { return textResp("thinking") })
	reg := registry(t, wrapType(&wrapLog{}, agent.WrapUp{Tools: []string{"submit"}, Turns: 2}))
	out, err := activateBudgets(context.Background(), agent.NewController(s, reg), claim(t, s), client, 0, agent.Budgets{Turns: 2})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonWrapUpFailed {
		t.Fatalf("outcome = %+v, want wrap_up_failed", out)
	}
	calls := client.Calls()
	if len(calls) != 2 || countWrapUpMessages(&calls[0]) != 1 || countWrapUpMessages(&calls[1]) != 2 {
		t.Fatalf("requests = %d, want two, the second with the message twice", len(calls))
	}
}

// TestWrapUpSeveralTools: a wrap-up naming several tools offers them alone,
// in declaration order, with tool choice auto; a call naming another declared
// tool is refused as the loop refuses any tool not offered, and recorded.
func TestWrapUpSeveralTools(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createLedgerRun(t, s)
		log := &wrapLog{}
		client := wrapClient(func(int, bool) llms.ChatResponse {
			return callsResp(tc("a", "lookup", `{}`), tc("b", "escalate", `{}`))
		})
		reg := registry(t, wrapType(log, agent.WrapUp{Tools: []string{"escalate", "submit"}}))
		out, err := activateBudgets(context.Background(), agent.NewController(s, reg), claim(t, s), client, 0, agent.Budgets{Turns: 1})
		if err != nil {
			t.Fatal(err)
		}
		if out.Status != store.StatusSucceeded || out.Reason.Code != agent.ReasonNoChange {
			t.Fatalf("outcome = %+v", out)
		}
		req := client.Calls()[0]
		if got := toolNames(&req); !slices.Equal(got, []string{"submit", "escalate"}) || req.ToolChoice.Type != llms.ToolChoiceAuto {
			t.Fatalf("wrap-up request offers %v with %+v", got, req.ToolChoice)
		}
		if got := log.get(); !slices.Equal(got, []string{"escalate/wrap-up=true"}) {
			t.Fatalf("executions = %v", got)
		}
		var refused []agent.ResultEntry
		for _, e := range entries(t, s, "r1") {
			if e.Kind == agent.EntryResult {
				if r := payload[agent.ResultEntry](t, e); r.Status == agent.ResultRefused {
					refused = append(refused, r)
				}
			}
		}
		if len(refused) != 1 || refused[0].Call != 0 || refused[0].Tool != "lookup" {
			t.Fatalf("refused results = %+v", refused)
		}
	})
}

// TestWrapUpOverridesStateToolChoice: a wrap-up turn's tool choice is auto
// whatever the state's own, here one naming a tool the wrap-up does not
// offer, which toolloop would refuse.
func TestWrapUpOverridesStateToolChoice(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	typ := wrapType(&wrapLog{}, agent.WrapUp{Tools: []string{"submit"}})
	typ.States[0].Model.Prompt = func(context.Context, *agent.Env, wrapData) (llms.ChatRequest, error) {
		return llms.ChatRequest{
			Messages:   []llms.Message{llms.UserText("work")},
			ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceTool, Name: "lookup"},
		}, nil
	}
	client := wrapClient(func(int, bool) llms.ChatResponse { return callsResp(tc("s", "submit", `{}`)) })
	out, err := activateBudgets(context.Background(), agent.NewController(s, registry(t, typ)), claim(t, s), client, 0, agent.Budgets{Turns: 1})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != store.StatusSucceeded {
		t.Fatalf("outcome = %+v", out)
	}
	if got := client.Calls()[0].ToolChoice; got != (llms.ToolChoice{Type: llms.ToolChoiceAuto}) {
		t.Fatalf("wrap-up tool choice = %+v, want auto", got)
	}
}

// TestWrapUpCutsOffSlowCall: a model call still in flight at the wrap-up
// point is cut off there and recorded as failed, and the wrap-up follows
// within the reserve.
func TestWrapUpCutsOffSlowCall(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createLedgerRun(t, s)
		client := &testllm.FakeChatClient{
			ModelRef: llms.ModelRef{Provider: "fake", Name: "slow"},
			Func: func(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
				if len(req.Tools) == 3 {
					<-ctx.Done()
					return llms.ChatResponse{}, ctx.Err()
				}
				return callsResp(tc("s", "submit", `{}`)), nil
			},
		}
		reg := registry(t, wrapType(&wrapLog{}, agent.WrapUp{Tools: []string{"submit"}, Reserve: 2 * time.Second}))
		out, err := activateBudgets(context.Background(), agent.NewController(s, reg), claim(t, s), client, 0,
			agent.Budgets{ActiveTime: 2*time.Second + 300*time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		if out.Status != store.StatusSucceeded {
			t.Fatalf("outcome = %+v", out)
		}
		es := entries(t, s, "r1")
		var failures []agent.ModelFailureEntry
		for _, e := range es {
			if e.Kind == agent.EntryModelFailure {
				failures = append(failures, payload[agent.ModelFailureEntry](t, e))
			}
		}
		if len(failures) != 1 || !strings.Contains(failures[0].Message, "cut off at the wrap-up point") {
			t.Fatalf("failures = %+v", failures)
		}
		if at := attempts(t, es); len(at) != 2 || at[0].WrapUp || !at[1].WrapUp {
			t.Fatalf("attempts = %+v", at)
		}
		if a := out.Consumed.ActiveTime; a < 300*time.Millisecond || a > 2*time.Second {
			t.Fatalf("active time %s, want the wrap-up to begin at 300ms and end well within the reserve", a)
		}
	})
}

// TestWrapUpByDeadline: a deadline within the reserve wraps up from the
// state's first turn, and the clock shows the deadline.
func TestWrapUpByDeadline(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	client := wrapClient(func(int, bool) llms.ChatResponse { return callsResp(tc("s", "submit", `{}`)) })
	reg := registry(t, wrapType(&wrapLog{}, agent.WrapUp{Tools: []string{"submit"}, Reserve: 2 * time.Hour}))
	out, err := activateBudgets(context.Background(), agent.NewController(s, reg), claim(t, s), client, 0,
		agent.Budgets{Deadline: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != store.StatusSucceeded {
		t.Fatalf("outcome = %+v", out)
	}
	req := client.Calls()[0]
	if got := toolNames(&req); !slices.Equal(got, []string{"submit"}) {
		t.Fatalf("first request offers %v, want submit alone", got)
	}
	if got := req.Messages[0].Content[0].Text; got != "work [budget: deadline in 60 min, wrap-up in 0 s]" {
		t.Fatalf("prompt = %q", got)
	}
}

// TestWrapUpDefaultDeadlineReserve: with no reserve declared, the deadline
// keeps a tenth of the time from the run's creation to the deadline.
func TestWrapUpDefaultDeadlineReserve(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	created := runRecord(t, s, "r1").CreatedAt
	client := wrapClient(func(int, bool) llms.ChatResponse { return callsResp(tc("s", "submit", `{}`)) })
	reg := registry(t, wrapType(&wrapLog{}, agent.WrapUp{Tools: []string{"submit"}}))
	deadline := created.Add(100 * time.Minute)
	out, err := activateBudgets(context.Background(), agent.NewController(s, reg), claim(t, s), client, 0, agent.Budgets{Deadline: deadline})
	if err != nil || out.Status != store.StatusSucceeded {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
	req := client.Calls()[0]
	if len(req.Tools) != 3 {
		t.Fatal("the first turn wrapped up with most of the time left")
	}
	if got := req.Messages[0].Content[0].Text; got != "work [budget: deadline in 100 min, wrap-up in 90 min]" {
		t.Fatalf("prompt = %q", got)
	}
}

// TestWrapUpCrashAtEveryCommit is TEST-5 for a wrap-up: the activation dies
// at each commit in turn, before it lands or after, and a new activation
// continues. The wrap-up survives recovery: every exit executes with
// Call.WrapUp, no recorded turn is asked again, and the transcript, clock
// lines included, is the uninterrupted run's.
func TestWrapUpCrashAtEveryCommit(t *testing.T) {
	answer := func(turn int, _ bool) llms.ChatResponse {
		if turn == 0 {
			return callsResp(tc("s0", "submit", `{"bad":true}`))
		}
		return callsResp(tc("s1", "submit", `{}`))
	}
	deadline := time.Now().Add(time.Hour)
	budgets := agent.Budgets{Deadline: deadline}
	wrap := agent.WrapUp{Tools: []string{"submit"}, Reserve: 2 * time.Hour}
	var want []string
	total := func() int {
		s := &countingStore{Store: newBackend(t, "memstore").open()}
		createLedgerRun(t, s)
		out, err := activateBudgets(context.Background(), agent.NewController(s, registry(t, wrapType(&wrapLog{}, wrap))), claim(t, s),
			wrapClient(answer), 0, budgets)
		if err != nil || out.Status != store.StatusSucceeded {
			t.Fatalf("uninterrupted run = %+v, %v", out, err)
		}
		want = lastTranscript(t, s)
		return s.n
	}()
	for at := 1; at <= total; at++ {
		for _, landing := range []bool{false, true} {
			t.Run(fmt.Sprintf("commit-%d/landing-%v", at, landing), func(t *testing.T) {
				t.Parallel()
				s := newBackend(t, "memstore").open()
				createLedgerRun(t, s)
				log := &wrapLog{}
				reg := registry(t, wrapType(log, wrap))
				cs := &crashStore{Store: s, at: at, landing: landing}
				_, err := activateBudgets(context.Background(), agent.NewController(cs, reg), claim(t, s), wrapClient(answer), 0, budgets)
				if !errors.Is(err, agent.ErrStoreFailure) {
					t.Fatalf("crashed activation = %v, want a store failure", err)
				}
				if run := runRecord(t, s, "r1"); !run.Status.Terminal() {
					releaseIfOwned(t, s)
					// A crash before the run's start landed leaves the next
					// activation the run's first, so it passes the budgets;
					// a later one ignores them.
					out, err := activateBudgets(context.Background(), agent.NewController(s, reg), claim(t, s), wrapClient(answer), 0, budgets)
					if err != nil {
						t.Fatal(err)
					}
					if out.Status != store.StatusSucceeded {
						t.Fatalf("outcome = %+v", out)
					}
				} else if run.Status != store.StatusSucceeded || at != total || !landing {
					t.Fatalf("run = %+v after commit %d", run, at)
				}
				for _, c := range log.get() {
					if !strings.HasSuffix(c, "wrap-up=true") {
						t.Fatalf("executions = %v: an exit ran outside the wrap-up", log.get())
					}
				}
				if n := countKind(entries(t, s, "r1"), agent.EntryTurn); n != 2 {
					t.Fatalf("%d turns recorded, want 2: a recorded turn was asked again", n)
				}
				if got := lastTranscript(t, s); !slices.Equal(got, want) {
					t.Fatalf("transcript = %v, want the uninterrupted run's %v", got, want)
				}
			})
		}
	}
}

// TestWrapUpHonorsStepLimit: the activation's step limit comes before a
// completion-policy ending, however the last step completed: a text answer,
// a tool error, or a turn recovered after a crash. The bound is enforced by
// the next activation.
func TestWrapUpHonorsStepLimit(t *testing.T) {
	budgets := agent.Budgets{Turns: 1}
	wrap := agent.WrapUp{Tools: []string{"submit"}, Turns: 1}
	answers := map[string]llms.ChatResponse{
		"text answer": textResp("thinking"),
		"tool error":  callsResp(tc("s", "submit", `{"bad":true}`)),
	}
	for name, resp := range answers {
		for _, crash := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/crash-%v", name, crash), func(t *testing.T) {
				if crash && name == "text answer" {
					t.Skip("a text answer's turn completes with its own commit")
				}
				s := newBackend(t, "memstore").open()
				createLedgerRun(t, s)
				reg := registry(t, wrapType(&wrapLog{}, wrap))
				client := wrapClient(func(int, bool) llms.ChatResponse { return resp })
				if crash {
					// Commits: activation_started, the run's start, the
					// attempt, the turn, the permission, the call's attempt;
					// the result, commit 7, never lands.
					cs := &crashStore{Store: s, at: 7}
					if _, err := activateBudgets(context.Background(), agent.NewController(cs, reg), claim(t, s), client, 1, budgets); !errors.Is(err, agent.ErrStoreFailure) {
						t.Fatalf("crashed activation = %v", err)
					}
					releaseIfOwned(t, s)
				}
				out, err := activateBudgets(context.Background(), agent.NewController(s, reg), claim(t, s), client, 1, budgets)
				if err != nil {
					t.Fatal(err)
				}
				if out.Status != store.StatusRunnable || out.Reason.Code != agent.ReasonStepLimit {
					t.Fatalf("outcome = %+v, want runnable at the step limit", out)
				}
				out, err = activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), client, 0)
				if err != nil {
					t.Fatal(err)
				}
				if out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonWrapUpFailed {
					t.Fatalf("next activation = %+v, want wrap_up_failed", out)
				}
			})
		}
	}
}

// TestNudgeHonorsStepLimit: the nudge's failure, like the wrap-up's, waits
// for the next attempt, after the step limit.
func TestNudgeHonorsStepLimit(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	reg := registry(t, wrapType(&wrapLog{}, agent.WrapUp{Tools: []string{"submit"}}))
	client := wrapClient(func(int, bool) llms.ChatResponse { return textResp("thinking") })
	out, err := activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), client, 2)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != store.StatusRunnable || out.Reason.Code != agent.ReasonStepLimit {
		t.Fatalf("outcome = %+v, want runnable at the step limit", out)
	}
	out, err = activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), client, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonWrapUpFailed {
		t.Fatalf("next activation = %+v, want wrap_up_failed", out)
	}
}

func TestWrapUpRegister(t *testing.T) {
	cases := map[string]agent.WrapUp{
		"no tools":         {},
		"undeclared tool":  {Tools: []string{"nope"}},
		"duplicate tool":   {Tools: []string{"submit", "submit"}},
		"negative reserve": {Tools: []string{"submit"}, Reserve: -time.Second},
		"negative turns":   {Tools: []string{"submit"}, Turns: -1},
		"invalid message":  {Tools: []string{"submit"}, Message: "\xff"},
	}
	for name, w := range cases {
		if err := agent.NewRegistry().Register(wrapType(&wrapLog{}, w)); !errors.Is(err, agent.ErrInvalidType) {
			t.Errorf("%s: Register = %v, want ErrInvalidType", name, err)
		}
	}
	if err := agent.NewRegistry().Register(wrapType(&wrapLog{}, agent.WrapUp{Tools: []string{"submit", "escalate"}})); err != nil {
		t.Fatalf("Register = %v", err)
	}
}

func TestBudgetStatusString(t *testing.T) {
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	full := agent.BudgetStatus{
		At: at,
		Budgets: agent.Budgets{
			ActiveTime: 20 * time.Minute, Deadline: at.Add(35 * time.Minute), Turns: 30,
			Tokens: agent.TokenBudget{Limit: 100000, Estimated: true},
		},
		Consumed: agent.Consumption{ActiveTime: 4 * time.Minute, Turns: 7, Tokens: 12345},
		WrapUp:   &agent.WrapUpPoint{ActiveTime: 18 * time.Minute, Deadline: at.Add(90 * time.Second), Turns: 27},
	}
	cases := []struct {
		name string
		st   agent.BudgetStatus
		want string
	}{
		{"none", agent.BudgetStatus{At: at}, ""},
		{"all", full, "[budget: active time 4 min of 20 min used, wrap-up at 18 min; deadline in 35 min, wrap-up in 90 s; " +
			"turns 7 of 30 used, wrap-up after 27; tokens ~12345 of 100000 used]"},
		{"outside a model state", agent.BudgetStatus{At: at, Budgets: agent.Budgets{Turns: 3}, Consumed: agent.Consumption{Turns: 1}},
			"[budget: turns 1 of 3 used]"},
	}
	for _, c := range cases {
		if got := c.st.String(); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		// A value and a pointer both render with fmt (a fmt.Stringer).
		if got, gotp := fmt.Sprint(c.st), fmt.Sprint(&c.st); got != c.want || gotp != c.want {
			t.Errorf("%s: fmt.Sprint %q and %q, want %q", c.name, got, gotp, c.want)
		}
	}
}

// crashWhere crashes a run at the first commit after which its record
// satisfies cond, and returns the store, the run left for recovery.
func crashWhere(t *testing.T, reg *agent.Registry, budgets agent.Budgets, answer func(int, bool) llms.ChatResponse,
	landing bool, cond func([]store.Entry) bool,
) store.Store {
	t.Helper()
	for at := 1; at <= 20; at++ {
		s := newBackend(t, "memstore").open()
		createLedgerRun(t, s)
		cs := &crashStore{Store: s, at: at, landing: landing}
		if _, err := activateBudgets(context.Background(), agent.NewController(cs, reg), claim(t, s), wrapClient(answer), 0, budgets); !errors.Is(err, agent.ErrStoreFailure) {
			t.Fatalf("crashed activation = %v, want a store failure", err)
		}
		if cond(entries(t, s, "r1")) {
			releaseIfOwned(t, s)
			return s
		}
	}
	t.Fatal("no commit leaves the record wanted")
	return nil
}

// TestWrapUpSurvivesAnInterruptedAttempt: a wrap-up attempt that commits
// and is interrupted before its turn lands keeps the state in wrap-up,
// even when the recovering version's reserve would not start one (ADR
// 0010, item 3).
func TestWrapUpSurvivesAnInterruptedAttempt(t *testing.T) {
	budgets := agent.Budgets{Deadline: time.Now().Add(time.Hour)}
	answer := func(int, bool) llms.ChatResponse { return callsResp(tc("s", "submit", `{}`)) }
	s := crashWhere(t, registry(t, wrapType(&wrapLog{}, agent.WrapUp{Tools: []string{"submit"}, Reserve: 2 * time.Hour})),
		budgets, answer, true, func(es []store.Entry) bool {
			return len(attempts(t, es)) == 1 && countKind(es, agent.EntryTurn) == 0
		})
	if a := attempts(t, entries(t, s, "r1")); !a[0].WrapUp {
		t.Fatalf("interrupted attempt = %+v, want a wrap-up attempt", a[0])
	}
	log := &wrapLog{}
	var offered [][]string
	client := wrapClient(answer)
	inner := client.Func
	client.Func = func(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
		offered = append(offered, toolNames(&req))
		return inner(ctx, req)
	}
	reg := registry(t, wrapType(log, agent.WrapUp{Tools: []string{"submit"}, Reserve: time.Nanosecond}))
	out, err := activateBudgets(context.Background(), agent.NewController(s, reg), claim(t, s), client, 0, budgets)
	if err != nil || out.Status != store.StatusSucceeded {
		t.Fatalf("recovered run = %+v, %v", out, err)
	}
	if len(offered) != 1 || !slices.Equal(offered[0], []string{"submit"}) {
		t.Fatalf("recovery offered %v, want only the wrap-up's tool", offered)
	}
	if got := log.get(); !slices.Equal(got, []string{"submit/wrap-up=true"}) {
		t.Fatalf("executions = %v, want submit in wrap-up", got)
	}
}

// TestWrapUpRecoveryRefusesUnofferedCall: a wrap-up turn's call naming a
// tool its attempt did not offer is refused by recovery even when the
// recovering version adds that tool to the wrap-up (ADR 0010, item 5).
func TestWrapUpRecoveryRefusesUnofferedCall(t *testing.T) {
	budgets := agent.Budgets{Deadline: time.Now().Add(time.Hour)}
	answer := func(turn int, _ bool) llms.ChatResponse {
		if turn == 0 {
			return callsResp(tc("e", "escalate", `{}`))
		}
		return callsResp(tc("s", "submit", `{}`))
	}
	s := crashWhere(t, registry(t, wrapType(&wrapLog{}, agent.WrapUp{Tools: []string{"submit"}, Reserve: 2 * time.Hour})),
		budgets, answer, false, func(es []store.Entry) bool {
			return countKind(es, agent.EntryTurn) == 1 && countKind(es, agent.EntryResult) == 0
		})
	log := &wrapLog{}
	reg := registry(t, wrapType(log, agent.WrapUp{Tools: []string{"submit", "escalate"}, Reserve: 2 * time.Hour}))
	out, err := activateBudgets(context.Background(), agent.NewController(s, reg), claim(t, s), wrapClient(answer), 0, budgets)
	if err != nil || out.Status != store.StatusSucceeded {
		t.Fatalf("recovered run = %+v, %v", out, err)
	}
	if got := log.get(); !slices.Equal(got, []string{"submit/wrap-up=true"}) {
		t.Fatalf("executions = %v: the unoffered escalate ran", got)
	}
	var first agent.ResultEntry
	for _, e := range entries(t, s, "r1") {
		if e.Kind == agent.EntryResult {
			first = payload[agent.ResultEntry](t, e)
			break
		}
	}
	if first.Status != agent.ResultRefused {
		t.Fatalf("escalate's result = %+v, want refused", first)
	}
}
