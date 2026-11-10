package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

func activateBudgets(ctx context.Context, c *agent.Controller, f store.Fence, client llms.ChatClient, maxSteps int, b agent.Budgets) (agent.Outcome, error) {
	return c.Activate(ctx, agent.ActivationInput{
		Fence: f, Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}, Client: client, MaxSteps: maxSteps, Budgets: b,
	})
}

func wantBudgetFailure(t *testing.T, out agent.Outcome, code, budget string) {
	t.Helper()
	if out.Status != store.StatusFailed || out.Reason.Code != code || out.Reason.Budget != budget {
		t.Fatalf("outcome = %+v, want failed/%s naming %q", out, code, budget)
	}
}

// reservations returns the reservation of each model attempt in the record.
func reservations(t *testing.T, es []store.Entry) []agent.Reservation {
	t.Helper()
	var out []agent.Reservation
	for _, e := range es {
		if e.Kind != agent.EntryModelAttempt {
			continue
		}
		at := payload[agent.ModelAttemptEntry](t, e)
		if at.Reservation == nil {
			t.Fatalf("attempt %d records no reservation", e.Seq)
		}
		out = append(out, *at.Reservation)
	}
	return out
}

// ledgerTokens is what the ledger script's run is charged: each of the
// three tool-calling turns reports 15 tokens, and the text answer reports
// no usage, so it is charged its reservation.
func ledgerTokens(t *testing.T, es []store.Entry) int64 {
	t.Helper()
	res := reservations(t, es)
	if len(res) != 4 {
		t.Fatalf("%d attempts, want 4", len(res))
	}
	return 3*15 + res[2].Tokens
}

func TestBudgetsConsumptionReported(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createLedgerRun(t, s)
		out, err := activateBudgets(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s),
			scripted(ledgerScript), 0, agent.Budgets{Turns: 10, ActiveTime: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		if out.Status != store.StatusSucceeded {
			t.Fatalf("outcome = %+v", out)
		}
		es := entries(t, s, "r1")
		c := out.Consumed
		want := ledgerTokens(t, es)
		if c.Turns != 4 || c.Tokens != want || !c.TokensEstimated || c.ActiveTime <= 0 {
			t.Fatalf("consumed = %+v, want 4 turns, %d tokens, estimated", c, want)
		}
		if len(c.Models) != 1 {
			t.Fatalf("models = %+v", c.Models)
		}
		m := c.Models[0]
		if m.Model.Name != "scripted" || m.Turns != 4 || m.Unknown != 1 || m.InputTokens != 30 || m.OutputTokens != 15 || m.Tokens != want {
			t.Fatalf("model consumption = %+v", m)
		}
		// The budgets are recorded with the run's start, and the outcome
		// entry carries the consumption.
		rs := payload[agent.RunStarted](t, es[1])
		if rs.Budgets == nil || rs.Budgets.Turns != 10 {
			t.Fatalf("run_started = %+v", rs)
		}
		last := payload[agent.OutcomeEntry](t, es[len(es)-1])
		if last.Consumed == nil || last.Consumed.Turns != 4 || last.Consumed.Tokens != want {
			t.Fatalf("outcome entry = %+v", last)
		}
		// With no token budget the output cap is left as the prompt set it.
		for _, r := range reservations(t, es) {
			if r.MaxTokens != 0 || r.Bound || r.Tokens <= 0 {
				t.Fatalf("reservation = %+v", r)
			}
		}
	})
}

func TestBudgetsTurnsExhausted(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createLedgerRun(t, s)
		client := scripted(ledgerScript)
		out, err := activateBudgets(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s),
			client, 0, agent.Budgets{Turns: 1})
		if err != nil {
			t.Fatal(err)
		}
		wantBudgetFailure(t, out, agent.ReasonBudgetExhausted, agent.BudgetTurns)
		if len(client.Calls()) != 1 || out.Consumed.Turns != 1 || out.Steps != 1 {
			t.Fatalf("model asked %d times; outcome %+v", len(client.Calls()), out)
		}
		// The first turn's refused call is pending when the second attempt
		// is refused: its result and the turn's checkpoint commit with the
		// ending (ADR 0008, item 13).
		es := entries(t, s, "r1")
		got := kinds(es[len(es)-3:])
		if !slices.Equal(got, []string{"result", "result", "outcome"}) || countKind(es, agent.EntryModelAttempt) != 1 {
			t.Fatalf("record = %v", kinds(es))
		}
		if r := payload[agent.ResultEntry](t, es[len(es)-2]); r.Status != agent.ResultRefused {
			t.Fatalf("pending result = %+v", r)
		}
		if out.CheckpointSeq != 2 {
			t.Fatalf("checkpoint = %d, want the turn's", out.CheckpointSeq)
		}
	})
}

func TestBudgetsHardTokensUnenforceable(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	client := scripted(ledgerScript)
	out, err := activateBudgets(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s),
		client, 0, agent.Budgets{Tokens: agent.TokenBudget{Limit: 1_000_000}})
	if err != nil {
		t.Fatal(err)
	}
	wantBudgetFailure(t, out, agent.ReasonBudgetUnenforceable, agent.BudgetTokens)
	if len(client.Calls()) != 0 || out.Consumed.Turns != 0 || countKind(entries(t, s, "r1"), agent.EntryModelAttempt) != 0 {
		t.Fatalf("an attempt was made: %+v", out)
	}
	if !strings.Contains(out.Detail, "estimated") {
		t.Fatalf("detail %q does not point the host at an estimated budget", out.Detail)
	}
}

func TestBudgetsEstimatedTokens(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createLedgerRun(t, s)
		// Learn the first request's input estimate from a run with no
		// token budget.
		probe := scripted(ledgerScript)
		ps := newBackend(t, "memstore").open()
		createLedgerRun(t, ps)
		if _, err := activateWith(context.Background(), agent.NewController(ps, registry(t, ledgerType(&execLog{}))), claim(t, ps), probe, 1); err != nil {
			t.Fatal(err)
		}
		input := int64(middleware.DefaultEstimator{}.EstimateChat(probe.Calls()[0]).InputTokens)

		// The budget leaves 20 output tokens for the first attempt; it
		// reports 15, and the second request's larger input no longer fits.
		limit := input + 20
		client := scripted(ledgerScript)
		out, err := activateBudgets(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s),
			client, 0, agent.Budgets{Tokens: agent.TokenBudget{Limit: limit, Estimated: true}})
		if err != nil {
			t.Fatal(err)
		}
		wantBudgetFailure(t, out, agent.ReasonBudgetExhausted, agent.BudgetTokens)
		calls := client.Calls()
		if len(calls) != 1 || calls[0].MaxTokens != 20 {
			t.Fatalf("model asked %d times, first with MaxTokens %d; want once, lowered to 20", len(calls), calls[0].MaxTokens)
		}
		if out.Consumed.Tokens != 15 || out.Consumed.TokensEstimated || out.Consumed.Turns != 1 {
			t.Fatalf("consumed = %+v, want the observed 15 tokens", out.Consumed)
		}
		res := reservations(t, entries(t, s, "r1"))
		if len(res) != 1 || res[0].MaxTokens != 20 || res[0].Tokens != limit || res[0].Bound {
			t.Fatalf("reservation = %+v, want the budget's %d", res, limit)
		}
		wantAttemptParams(t, s, entries(t, s, "r1"), true)
	})
}

func TestBudgetsEstimatedTokensKeepsLowerCap(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	typ := ledgerType(&execLog{})
	prompt := typ.States[0].Model.Prompt
	typ.States[0].Model.Prompt = func(ctx context.Context, env *agent.Env, d ledgerData) (llms.ChatRequest, error) {
		req, err := prompt(ctx, env, d)
		req.MaxTokens = 7
		return req, err
	}
	client := scripted(ledgerScript)
	if _, err := activateBudgets(context.Background(), agent.NewController(s, registry(t, typ)), claim(t, s),
		client, 1, agent.Budgets{Tokens: agent.TokenBudget{Limit: 1_000_000, Estimated: true}}); err != nil {
		t.Fatal(err)
	}
	if got := client.Calls()[0].MaxTokens; got != 7 {
		t.Fatalf("MaxTokens = %d, want the prompt's lower 7", got)
	}
}

func TestBudgetsFailedAttemptChargedReservation(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	client := &testllm.FakeChatClient{
		ModelRef: llms.ModelRef{Provider: "fake", Name: "down"},
		Func: func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
			return llms.ChatResponse{}, &llms.ProviderError{Provider: "fake", Kind: llms.ErrorKindUnavailable, StatusCode: 500, Message: "down"}
		},
	}
	out, err := activateWith(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s), client, 0)
	if err != nil {
		t.Fatal(err)
	}
	res := reservations(t, entries(t, s, "r1"))
	if out.Reason.Code != agent.ReasonModelError || len(res) != 1 {
		t.Fatalf("outcome = %+v, reservations %+v", out, res)
	}
	if c := out.Consumed; c.Tokens != res[0].Tokens || !c.TokensEstimated || c.Turns != 1 || c.Models[0].Unknown != 1 {
		t.Fatalf("consumed = %+v, want the reservation %d", c, res[0].Tokens)
	}
}

// TestBudgetsInterruptedAttemptCharged: an attempt whose commit landed but
// whose activation died before a turn or failure was recorded is charged
// its reservation on recovery, and its turn counts (ADR 0004, item 13).
func TestBudgetsInterruptedAttemptCharged(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		createLedgerRun(t, s)
		reg := registry(t, ledgerType(&execLog{}))
		budgets := agent.Budgets{Turns: 5}
		// Commits: activation_started, the run's start, the first attempt.
		cs := &crashStore{Store: s, at: 3, landing: true}
		if _, err := activateBudgets(context.Background(), agent.NewController(cs, reg), claim(t, s), scripted(ledgerScript), 0, budgets); !errors.Is(err, agent.ErrStoreFailure) {
			t.Fatalf("err = %v, want a store failure", err)
		}
		releaseIfOwned(t, s)
		if b.durable {
			s.Close()
			s = b.open()
		}
		defer s.Close()
		out, err := activateBudgets(context.Background(), agent.NewController(s, reg), claim(t, s), scripted(ledgerScript), 0, agent.Budgets{})
		if err != nil {
			t.Fatal(err)
		}
		es := entries(t, s, "r1")
		res := reservations(t, es)
		if len(res) != 5 {
			t.Fatalf("%d attempts, want 5", len(res))
		}
		// The interrupted attempt and the text answer are charged their
		// reservations; the turn budget of five admits exactly this run.
		want := 3*15 + res[0].Tokens + res[3].Tokens
		if c := out.Consumed; out.Status != store.StatusSucceeded || c.Turns != 5 || c.Tokens != want || c.Models[0].Unknown != 2 {
			t.Fatalf("outcome = %+v, want succeeded with 5 turns and %d tokens", out, want)
		}
	})
}

// TestBudgetsInterruptedAttemptCountsAgainstTurns: the turn an interrupted
// attempt was admitted for counts, so a run that keeps dying mid-call
// cannot exceed its turn budget.
func TestBudgetsInterruptedAttemptCountsAgainstTurns(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	reg := registry(t, ledgerType(&execLog{}))
	cs := &crashStore{Store: s, at: 3, landing: true}
	if _, err := activateBudgets(context.Background(), agent.NewController(cs, reg), claim(t, s), scripted(ledgerScript), 0, agent.Budgets{Turns: 1}); err == nil {
		t.Fatal("want the injected crash")
	}
	releaseIfOwned(t, s)
	client := scripted(ledgerScript)
	out, err := activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), client, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantBudgetFailure(t, out, agent.ReasonBudgetExhausted, agent.BudgetTurns)
	if len(client.Calls()) != 0 {
		t.Fatalf("model asked %d times after the budget was spent", len(client.Calls()))
	}
}

// TestBudgetsCarriedAcrossActivations: consumption is checkpointed and the
// run's budgets are the recorded ones, so an activation per step consumes
// what one activation does, and later activations' budgets are ignored.
func TestBudgetsCarriedAcrossActivations(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createLedgerRun(t, s)
		reg := registry(t, ledgerType(&execLog{}))
		var out agent.Outcome
		var active time.Duration
		for i := 0; ; i++ {
			// Only the first activation's budgets count.
			budgets := agent.Budgets{Turns: 10, ActiveTime: time.Hour}
			if i > 0 {
				budgets = agent.Budgets{Turns: 1}
			}
			var err error
			out, err = activateBudgets(context.Background(), agent.NewController(s, reg), claim(t, s), scripted(ledgerScript), 1, budgets)
			if err != nil {
				t.Fatal(err)
			}
			if out.Consumed.ActiveTime < active {
				t.Fatalf("active time went back from %s to %s", active, out.Consumed.ActiveTime)
			}
			active = out.Consumed.ActiveTime
			if out.Status != store.StatusRunnable {
				break
			}
			if i > 10 {
				t.Fatal("run does not end")
			}
		}
		es := entries(t, s, "r1")
		if c := out.Consumed; out.Status != store.StatusSucceeded || c.Turns != 4 || c.Tokens != ledgerTokens(t, es) {
			t.Fatalf("outcome = %+v", out)
		}
	})
}

func TestBudgetsDeadlinePassed(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createLedgerRun(t, s)
		client := scripted(ledgerScript)
		deadline := time.Now().Add(-time.Second)
		out, err := activateBudgets(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s),
			client, 0, agent.Budgets{Deadline: deadline})
		if err != nil {
			t.Fatal(err)
		}
		wantBudgetFailure(t, out, agent.ReasonBudgetExhausted, agent.BudgetDeadline)
		es := entries(t, s, "r1")
		if len(client.Calls()) != 0 || !slices.Equal(kinds(es), []string{"activation_started", "run_started", "outcome"}) {
			t.Fatalf("work was done: %d calls, record %v", len(client.Calls()), kinds(es))
		}
		if rs := payload[agent.RunStarted](t, es[1]); rs.Budgets == nil || !rs.Budgets.Deadline.Equal(deadline) {
			t.Fatalf("run_started = %+v, want the deadline recorded", rs)
		}
	})
}

// TestBudgetsDeadlineAcrossActivations: the deadline recorded with the
// run's start ends a later activation that starts after it.
func TestBudgetsDeadlineAcrossActivations(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	reg := registry(t, ledgerType(&execLog{}))
	deadline := time.Now().Add(300 * time.Millisecond)
	out, err := activateBudgets(context.Background(), agent.NewController(s, reg), claim(t, s), scripted(ledgerScript), 1, agent.Budgets{Deadline: deadline})
	if err != nil || out.Status != store.StatusRunnable {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
	time.Sleep(time.Until(deadline))
	client := scripted(ledgerScript)
	out, err = activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), client, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantBudgetFailure(t, out, agent.ReasonBudgetExhausted, agent.BudgetDeadline)
	if len(client.Calls()) != 0 {
		t.Fatalf("model asked %d times after the deadline", len(client.Calls()))
	}
}

// TestBudgetsActiveTimeCutsOffACall: active time ends the activation's
// context, so a model call in flight is cut off rather than overshooting,
// and its attempt is recorded as failed.
func TestBudgetsActiveTimeCutsOffACall(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createLedgerRun(t, s)
		client := &testllm.FakeChatClient{
			ModelRef: llms.ModelRef{Provider: "fake", Name: "slow"},
			Func: func(ctx context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
				<-ctx.Done()
				return llms.ChatResponse{}, ctx.Err()
			},
		}
		out, err := activateBudgets(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s),
			client, 0, agent.Budgets{ActiveTime: 200 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		wantBudgetFailure(t, out, agent.ReasonBudgetExhausted, agent.BudgetActiveTime)
		es := entries(t, s, "r1")
		if countKind(es, agent.EntryModelFailure) != 1 || out.Consumed.ActiveTime < 200*time.Millisecond {
			t.Fatalf("record %v, consumed %+v", kinds(es), out.Consumed)
		}
	})
}

// TestBudgetsActiveTimeAccumulates: active time spent by earlier
// activations counts, so a later one starts with only what is left.
func TestBudgetsActiveTimeAccumulates(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	reg := registry(t, ledgerType(&execLog{}))
	slow := &testllm.FakeChatClient{
		ModelRef: llms.ModelRef{Provider: "fake", Name: "scripted"},
		Func: func(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
			time.Sleep(150 * time.Millisecond)
			return scripted(ledgerScript).Func(ctx, req)
		},
	}
	out, err := activateBudgets(context.Background(), agent.NewController(s, reg), claim(t, s), slow, 1, agent.Budgets{ActiveTime: 250 * time.Millisecond})
	if err != nil || out.Status != store.StatusRunnable || out.Consumed.ActiveTime < 150*time.Millisecond {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
	// About 100ms are left: the next slow call does not fit.
	out, err = activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), slow, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantBudgetFailure(t, out, agent.ReasonBudgetExhausted, agent.BudgetActiveTime)
	if out.Consumed.ActiveTime < 250*time.Millisecond {
		t.Fatalf("consumed = %+v", out.Consumed)
	}
}

func TestBudgetsInvalid(t *testing.T) {
	for name, b := range map[string]agent.Budgets{
		"negative tokens":      {Tokens: agent.TokenBudget{Limit: -1, Estimated: true}},
		"estimated, no limit":  {Tokens: agent.TokenBudget{Estimated: true}},
		"negative turns":       {Turns: -1},
		"negative active time": {ActiveTime: -time.Second},
		"negative cost":        {Cost: -1},
	} {
		t.Run(name, func(t *testing.T) {
			s := newBackend(t, "memstore").open()
			createLedgerRun(t, s)
			out, err := activateBudgets(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s),
				scripted(ledgerScript), 0, b)
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonStateError || !strings.Contains(out.Detail, "budgets") {
				t.Fatalf("outcome = %+v, want a state error naming the budgets", out)
			}
		})
	}
}

// TestBudgetsCodeStatesUnaffected: a code-state run with token and turn
// budgets makes no model attempt, so it never consults them.
func TestBudgetsCodeStatesUnaffected(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createRun(t, s, "r1", `{"target":2}`)
	out, err := agent.NewController(s, registry(t, counterType())).Activate(context.Background(), agent.ActivationInput{
		Fence: claim(t, s), Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"},
		Budgets: agent.Budgets{Turns: 1, Tokens: agent.TokenBudget{Limit: 1}},
	})
	if err != nil || out.Status != store.StatusSucceeded || out.Consumed.Turns != 0 {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
}

// TestBudgetsCostNotBuilt: a cost budget is a contract only; a run that
// sets one fails at its start, before any work, rather than running
// unenforced (MVP.md, BUD-1).
func TestBudgetsCostNotBuilt(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	client := scripted(ledgerScript)
	out, err := activateBudgets(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s),
		client, 0, agent.Budgets{Cost: 100})
	if err != nil {
		t.Fatal(err)
	}
	wantBudgetFailure(t, out, agent.ReasonBudgetUnenforceable, agent.BudgetCost)
	if es := entries(t, s, "r1"); len(client.Calls()) != 0 || !slices.Equal(kinds(es), []string{"activation_started", "run_started", "outcome"}) {
		t.Fatalf("work was done: record %v", kinds(es))
	}
}

// recordedCharges recomputes a run's turns and tokens from its record:
// each attempt counts a turn and is charged its turn's usage, or its
// reservation when the usage is unknown, it failed or it was interrupted.
func recordedCharges(t *testing.T, es []store.Entry) (turns int, tokens int64) {
	t.Helper()
	var open *agent.Reservation
	settle := func(u *agent.Usage) {
		if open == nil {
			return
		}
		n := int64(0)
		if u != nil {
			n = max(int64(u.TotalTokens), int64(u.InputTokens)+int64(max(u.BillableOutputTokens, u.OutputTokens+u.ReasoningTokens)))
		}
		if n == 0 {
			n = open.Tokens
		}
		tokens += n
		open = nil
	}
	for _, e := range es {
		switch e.Kind {
		case agent.EntryModelAttempt:
			settle(nil)
			at := payload[agent.ModelAttemptEntry](t, e)
			open = at.Reservation
			turns++
		case agent.EntryTurn:
			u := payload[agent.TurnEntry](t, e).Usage
			settle(&u)
		case agent.EntryModelFailure:
			settle(nil)
		}
	}
	settle(nil)
	return turns, tokens
}

// paramsOf decodes a recorded parameters artifact.
func paramsOf(t *testing.T, s store.Store, digest string) llms.ChatRequest {
	t.Helper()
	_, raw, err := s.Artifact(context.Background(), digest)
	if err != nil {
		t.Fatalf("Artifact %s: %v", digest, err)
	}
	var req llms.ChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	return req
}

// wantAttemptParams checks that every attempt records the parameters it
// sent, and, when a token budget lowered the output cap (the ledger's
// prompt sets none), the state's own in StateParams.
func wantAttemptParams(t *testing.T, s store.Store, es []store.Entry, lowered bool) {
	t.Helper()
	for _, e := range es {
		if e.Kind != agent.EntryModelAttempt {
			continue
		}
		at := payload[agent.ModelAttemptEntry](t, e)
		if got := paramsOf(t, s, at.Params).MaxTokens; got != at.Reservation.MaxTokens || (got == 0) == lowered {
			t.Fatalf("attempt %d records MaxTokens %d, sent %d", e.Seq, got, at.Reservation.MaxTokens)
		}
		if lowered != (at.StateParams != "") || (lowered && paramsOf(t, s, at.StateParams).MaxTokens != 0) {
			t.Fatalf("attempt %d: state parameters %q, lowered %v", e.Seq, at.StateParams, lowered)
		}
	}
}

// TestBudgetsCrashAtEveryCommit: wherever an activation dies, the
// consumption the run ends with is exactly what its record charges: each
// recorded attempt counts one turn, never two (a checkpoint covers only the
// entries before it), and its tokens are charged once. The prompt sets no
// output cap, so admission lowers it to all the budget leaves, and an
// attempt of unknown usage (an interrupted one, or the fixture's text
// answer, which reports none) is charged that much (BUD-3, BUD-4): the run
// may end with the token budget exhausted, which the record must also
// account for.
func TestBudgetsCrashAtEveryCommit(t *testing.T) {
	budgets := agent.Budgets{Tokens: agent.TokenBudget{Limit: 1_000_000, Estimated: true}, Turns: 100}
	total := func() int {
		s := &countingStore{Store: newBackend(t, "memstore").open()}
		createLedgerRun(t, s)
		if _, err := activateBudgets(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s), scripted(ledgerScript), 0, budgets); err != nil {
			t.Fatal(err)
		}
		return s.n
	}()
	for at := 1; at <= total; at++ {
		for _, landing := range []bool{false, true} {
			t.Run(fmt.Sprintf("commit-%d/landing-%v", at, landing), func(t *testing.T) {
				t.Parallel()
				s := newBackend(t, "memstore").open()
				createLedgerRun(t, s)
				reg := registry(t, ledgerType(&execLog{}))
				cs := &crashStore{Store: s, at: at, landing: landing}
				if _, err := activateBudgets(context.Background(), agent.NewController(cs, reg), claim(t, s), scripted(ledgerScript), 0, budgets); !errors.Is(err, agent.ErrStoreFailure) {
					t.Fatalf("crashed activation = %v, want a store failure", err)
				}
				if runRecord(t, s, "r1").Status.Terminal() {
					return
				}
				releaseIfOwned(t, s)
				// The budgets count only if the crashed activation did not
				// record the run's start.
				out, err := activateBudgets(context.Background(), agent.NewController(s, reg), claim(t, s), scripted(ledgerScript), 0, budgets)
				if err != nil {
					t.Fatal(err)
				}
				exhausted := out.Reason.Code == agent.ReasonBudgetExhausted && out.Reason.Budget == agent.BudgetTokens
				if out.Status != store.StatusSucceeded && !exhausted {
					t.Fatalf("outcome = %+v", out)
				}
				es := entries(t, s, "r1")
				turns, tokens := recordedCharges(t, es)
				if out.Consumed.Turns != turns || out.Consumed.Tokens != tokens {
					t.Fatalf("consumed %d turns, %d tokens; the record charges %d, %d", out.Consumed.Turns, out.Consumed.Tokens, turns, tokens)
				}
				if exhausted && tokens < budgets.Tokens.Limit-1000 {
					t.Fatalf("token budget exhausted with %d of %d charged", tokens, budgets.Tokens.Limit)
				}
				wantAttemptParams(t, s, es, true)
			})
		}
	}
}

// TestBudgetsRefusedStartRecordsInputs: a start refused for its budgets
// still records them, and the configuration, with its ending (REC-1).
func TestBudgetsRefusedStartRecordsInputs(t *testing.T) {
	for name, b := range map[string]agent.Budgets{
		"cost":    {Cost: 100, Turns: 3},
		"invalid": {Turns: -1},
	} {
		t.Run(name, func(t *testing.T) {
			s := newBackend(t, "memstore").open()
			createLedgerRun(t, s)
			out, err := agent.NewController(s, registry(t, ledgerType(&execLog{}))).Activate(context.Background(), agent.ActivationInput{
				Fence: claim(t, s), Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}, Client: scripted(ledgerScript),
				Config: json.RawMessage(`{"k":1}`), Budgets: b,
			})
			if err != nil || out.Status != store.StatusFailed {
				t.Fatalf("outcome = %+v, %v", out, err)
			}
			rs := payload[agent.RunStarted](t, entries(t, s, "r1")[1])
			if rs.Budgets == nil || *rs.Budgets != b || rs.Config == "" {
				t.Fatalf("run_started = %+v, want the budgets and configuration", rs)
			}
			if _, _, err := s.Artifact(context.Background(), rs.Config); err != nil {
				t.Fatalf("configuration artifact: %v", err)
			}
		})
	}
}
