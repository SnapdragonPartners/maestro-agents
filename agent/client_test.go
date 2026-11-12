package agent_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/ratelimit"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// fastRetry retries once more, at once.
var fastRetry = &middleware.RetryConfig{MaxAttempts: 2, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond}

func newModelClient(t *testing.T, base llms.ChatClient, cfg agent.ModelClientConfig) *agent.ModelClient { //nolint:gocritic // hugeParam: a test helper
	t.Helper()
	mc, err := agent.NewModelClient(base, cfg)
	if err != nil {
		t.Fatalf("NewModelClient: %v", err)
	}
	return mc
}

// failingFirst answers its first call with a retryable provider failure and
// every later one from the ledger script.
func failingFirst(provider llms.ProviderID) *testllm.FakeChatClient {
	inner := scripted(ledgerScript)
	var n atomic.Int32
	return &testllm.FakeChatClient{
		ModelRef: llms.ModelRef{Provider: provider, Name: "flaky"},
		Func: func(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
			if n.Add(1) == 1 {
				return llms.ChatResponse{}, &llms.ProviderError{Provider: provider, Kind: llms.ErrorKindUnavailable, StatusCode: 503, Message: "busy"}
			}
			return inner.Complete(ctx, req)
		},
	}
}

func activateClient(ctx context.Context, c *agent.Controller, f store.Fence, mc *agent.ModelClient, maxSteps int, b agent.Budgets) (agent.Outcome, error) { //nolint:gocritic // hugeParam: a test helper
	return c.Activate(ctx, agent.ActivationInput{
		Fence: f, Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}, Namespace: ns, Client: mc, MaxSteps: maxSteps, Budgets: b,
	})
}

type refusingLimiter struct{}

func (*refusingLimiter) Reserve(context.Context, ratelimit.ReservationRequest) (ratelimit.Reservation, error) {
	return nil, &llms.LimitError{Reason: "over quota", Permanent: true}
}

func TestNewModelClientRefusesBadConfig(t *testing.T) {
	base := scripted(ledgerScript)
	var nilBase *testllm.FakeChatClient
	var nilLimiter *refusingLimiter
	for name, tc := range map[string]struct {
		base llms.ChatClient
		cfg  agent.ModelClientConfig
	}{
		"nil base":          {nil, agent.ModelClientConfig{}},
		"typed nil base":    {nilBase, agent.ModelClientConfig{}},
		"nil outer layer":   {base, agent.ModelClientConfig{Outer: []middleware.ChatMiddleware{nil}}},
		"typed nil limiter": {base, agent.ModelClientConfig{Limiter: nilLimiter}},
	} {
		t.Run(name, func(t *testing.T) {
			if mc, err := agent.NewModelClient(tc.base, tc.cfg); err == nil || mc != nil {
				t.Fatalf("NewModelClient = %v, %v; want an error", mc, err)
			}
		})
	}
}

// TestModelClientOutsideActivation: a call no activation makes passes
// through the attempt layer unrecorded.
func TestModelClientOutsideActivation(t *testing.T) {
	base := scripted(ledgerScript)
	mc := newModelClient(t, base, agent.ModelClientConfig{})
	if mc.Model() != base.Model() {
		t.Fatalf("Model = %v, want the base's %v", mc.Model(), base.Model())
	}
	req := llms.ChatRequest{Messages: []llms.Message{llms.UserText("items for r1")}}
	if _, err := mc.Complete(context.Background(), req); err != nil || len(base.Calls()) != 1 {
		t.Fatalf("Complete: %v after %d calls", err, len(base.Calls()))
	}
}

// TestModelClientRetriesRecordedAsAttempts: a retry inside the client is a
// further attempt of the same turn, recorded before it is sent and charged
// the failed attempt's reservation (ADR 0014).
func TestModelClientRetriesRecordedAsAttempts(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createLedgerRun(t, s)
		base := failingFirst("fake")
		mc := newModelClient(t, base, agent.ModelClientConfig{Retry: fastRetry})
		out, err := activateClient(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s), mc, 1, agent.Budgets{})
		if err != nil {
			t.Fatal(err)
		}
		es := entries(t, s, "r1")
		at := attempts(t, es)
		if len(base.Calls()) != 2 || len(at) != 2 || at[0].Attempt != 1 || at[1].Attempt != 2 {
			t.Fatalf("%d provider calls; attempts %+v", len(base.Calls()), at)
		}
		if !slices.Equal(at[0].Messages, at[1].Messages) || at[0].Params != at[1].Params {
			t.Fatalf("the retry's entry does not repeat the first's request: %+v", at)
		}
		want := at[0].Reservation.Tokens + 15
		if c := out.Consumed; c.Turns != 1 || c.Tokens != want || !c.TokensEstimated || c.Models[0].Unknown != 1 {
			t.Fatalf("consumed = %+v, want 1 turn and %d tokens", c, want)
		}
		if turns, tokens := recordedCharges(t, es); turns != 1 || tokens != want {
			t.Fatalf("record charges %d turns, %d tokens", turns, tokens)
		}
	})
}

// TestModelClientRetryRecovery: an activation that dies after a retry's
// attempt is recorded counts that call's turn once on recovery and charges
// both attempts their reservations.
func TestModelClientRetryRecovery(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		createLedgerRun(t, s)
		reg := registry(t, ledgerType(&execLog{}))
		mc := newModelClient(t, failingFirst("fake"), agent.ModelClientConfig{Retry: fastRetry})
		// Commits: activation_started, the run's start, the first attempt,
		// the retry.
		cs := &crashStore{Store: s, at: 4, landing: true}
		if _, err := activateClient(context.Background(), agent.NewController(cs, reg), claim(t, s), mc, 0, agent.Budgets{}); !errors.Is(err, agent.ErrStoreFailure) {
			t.Fatalf("err = %v, want a store failure", err)
		}
		releaseIfOwned(t, s)
		if b.durable {
			s.Close()
			s = b.open()
		}
		defer s.Close()
		out, err := activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), scripted(ledgerScript), 0)
		if err != nil {
			t.Fatal(err)
		}
		res := reservations(t, entries(t, s, "r1"))
		if len(res) != 6 {
			t.Fatalf("%d attempts, want 6", len(res))
		}
		// Both attempts of the interrupted call (the flaky route, sorted
		// first) and the text answer are charged their reservations; the
		// call counts one turn.
		want := 3*15 + res[0].Tokens + res[1].Tokens + res[4].Tokens
		if c := out.Consumed; out.Status != store.StatusSucceeded || c.Turns != 5 || c.Tokens != want || c.Models[0].Unknown+c.Models[1].Unknown != 3 || c.Models[0].Turns != 1 {
			t.Fatalf("outcome = %+v, want succeeded with 5 turns and %d tokens", out, want)
		}
	})
}

// TestModelClientHardTokenBudget: a hard token budget admits each attempt
// on its known-safe bound, lowering the output cap to what the budget
// leaves after the input's bound (BUD-3, ADR 0014).
func TestModelClientHardTokenBudget(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createLedgerRun(t, s)
		base := scripted(ledgerScript)
		base.ModelRef = llms.ModelRef{Provider: llms.ProviderAnthropic, Name: "scripted"}
		const limit = 1_000_000
		out, err := activateClient(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s),
			newModelClient(t, base, agent.ModelClientConfig{}), 1, agent.Budgets{Tokens: agent.TokenBudget{Limit: limit}})
		if err != nil || out.Status != store.StatusRunnable {
			t.Fatalf("outcome = %+v, %v", out, err)
		}
		sent := base.Calls()[0]
		bound, ok, err := middleware.DefaultEstimator{}.BoundChat(base.Model(), sent)
		if !ok || err != nil {
			t.Fatalf("BoundChat = %v, %v", ok, err)
		}
		if sent.MaxTokens != limit-bound.InputTokens {
			t.Fatalf("MaxTokens = %d, want the %d the bound leaves", sent.MaxTokens, limit-bound.InputTokens)
		}
		res := reservations(t, entries(t, s, "r1"))
		if len(res) != 1 || !res[0].Bound || res[0].Tokens != int64(bound.Tokens()) || res[0].MaxTokens != sent.MaxTokens {
			t.Fatalf("reservation = %+v, want the bound %d", res, bound.Tokens())
		}
		if c := out.Consumed; c.Tokens != 15 || c.TokensEstimated {
			t.Fatalf("consumed = %+v, want the observed 15", c)
		}
	})
}

// TestModelClientHardTokenBudgetRetryMustFit: a retry is admitted against
// what the budget leaves after the failed attempt's reservation; one that
// no longer fits is refused before it is sent.
func TestModelClientHardTokenBudgetRetryMustFit(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	base := failingFirst(llms.ProviderAnthropic)
	out, err := activateClient(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s),
		newModelClient(t, base, agent.ModelClientConfig{Retry: fastRetry}), 0, agent.Budgets{Tokens: agent.TokenBudget{Limit: 1_000_000}})
	if err != nil {
		t.Fatal(err)
	}
	wantBudgetFailure(t, out, agent.ReasonBudgetExhausted, agent.BudgetTokens)
	if len(base.Calls()) != 1 || len(attempts(t, entries(t, s, "r1"))) != 1 {
		t.Fatalf("the refused retry was sent or recorded: %d calls", len(base.Calls()))
	}
	if c := out.Consumed; c.Tokens != 1_000_000 || c.Turns != 1 {
		t.Fatalf("consumed = %+v, want the first attempt's whole reservation", c)
	}
}

// TestModelClientOuterLayers: a host's outer layer sees each logical call;
// one that changes the request is refused before anything is sent.
func TestModelClientOuterLayers(t *testing.T) {
	t.Run("pass through", func(t *testing.T) {
		s := newBackend(t, "memstore").open()
		createLedgerRun(t, s)
		var seen atomic.Int32
		observe := func(next llms.ChatClient) llms.ChatClient {
			return &testllm.FakeChatClient{ModelRef: next.Model(), Func: func(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
				seen.Add(1)
				return next.Complete(ctx, req)
			}}
		}
		base := scripted(ledgerScript)
		mc := newModelClient(t, base, agent.ModelClientConfig{Outer: []middleware.ChatMiddleware{observe}})
		out, err := activateClient(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s), mc, 1, agent.Budgets{})
		if err != nil || out.Status != store.StatusRunnable || seen.Load() != 1 || len(base.Calls()) != 1 {
			t.Fatalf("outcome = %+v, %v; layer saw %d", out, err, seen.Load())
		}
	})
	t.Run("changed request", func(t *testing.T) {
		s := newBackend(t, "memstore").open()
		createLedgerRun(t, s)
		rewrite := func(next llms.ChatClient) llms.ChatClient {
			return &testllm.FakeChatClient{ModelRef: next.Model(), Func: func(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
				req.Messages = append(slices.Clone(req.Messages), llms.UserText("also this"))
				return next.Complete(ctx, req)
			}}
		}
		base := scripted(ledgerScript)
		mc := newModelClient(t, base, agent.ModelClientConfig{Outer: []middleware.ChatMiddleware{rewrite}})
		out, err := activateClient(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s), mc, 0, agent.Budgets{})
		if err != nil || out.Reason.Code != agent.ReasonModelError {
			t.Fatalf("outcome = %+v, %v; want model_error", out, err)
		}
		if len(base.Calls()) != 0 || len(attempts(t, entries(t, s, "r1"))) != 0 {
			t.Fatalf("the changed request was sent or recorded: %d calls", len(base.Calls()))
		}
	})
}

// TestModelClientRateLimiterRefusal: the host's rate limiter refuses before
// the attempt layer, so nothing is recorded or counted, and the run fails
// with model_error.
func TestModelClientRateLimiterRefusal(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	base := scripted(ledgerScript)
	mc := newModelClient(t, base, agent.ModelClientConfig{Limiter: &refusingLimiter{}})
	out, err := activateClient(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s), mc, 0, agent.Budgets{})
	if err != nil || out.Reason.Code != agent.ReasonModelError {
		t.Fatalf("outcome = %+v, %v; want model_error", out, err)
	}
	if len(base.Calls()) != 0 || len(attempts(t, entries(t, s, "r1"))) != 0 || out.Consumed.Turns != 0 {
		t.Fatalf("an attempt was made: %+v", out)
	}
}

// estimateOnly is an estimator with no input bound.
type estimateOnly struct{ middleware.DefaultEstimator }

func (estimateOnly) BoundChat() {} // hides DefaultEstimator's, so no InputBounder

// TestModelClientHardTokenBudgetNeedsBounder: a hard token budget with an
// estimator that gives no bound fails before anything is sent.
func TestModelClientHardTokenBudgetNeedsBounder(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	base := scripted(ledgerScript)
	base.ModelRef = llms.ModelRef{Provider: llms.ProviderAnthropic, Name: "scripted"}
	mc := newModelClient(t, base, agent.ModelClientConfig{Estimator: estimateOnly{}})
	out, err := activateClient(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s), mc, 0,
		agent.Budgets{Tokens: agent.TokenBudget{Limit: 1_000_000}})
	if err != nil {
		t.Fatal(err)
	}
	wantBudgetFailure(t, out, agent.ReasonBudgetUnenforceable, agent.BudgetTokens)
	if len(base.Calls()) != 0 || len(attempts(t, entries(t, s, "r1"))) != 0 {
		t.Fatalf("an attempt was made: %d calls", len(base.Calls()))
	}
}

// countingLimiter admits every request and counts what its reservations
// see; with block set, its first Reserve waits for the context to end, as a
// rate limiter's wait would.
type countingLimiter struct {
	reserves, commits, releases, rollbacks atomic.Int32
	block                                  bool
}

func (l *countingLimiter) Reserve(ctx context.Context, _ ratelimit.ReservationRequest) (ratelimit.Reservation, error) {
	if l.reserves.Add(1) == 1 && l.block {
		<-ctx.Done()
		return nil, &llms.LimitError{Reason: "waited", Cause: ctx.Err()}
	}
	return &countingReservation{l: l}, nil
}

type countingReservation struct{ l *countingLimiter }

func (r *countingReservation) Commit(context.Context, llms.Usage) error {
	r.l.commits.Add(1)
	return nil
}

func (r *countingReservation) Release(context.Context) error {
	r.l.releases.Add(1)
	return nil
}

func (r *countingReservation) Rollback(context.Context) error {
	r.l.rollbacks.Add(1)
	return nil
}

// TestModelClientHostReservations: the host's reservation is committed and
// released for a sent attempt, and rolled back for one the runtime refused
// before sending it.
func TestModelClientHostReservations(t *testing.T) {
	t.Run("sent", func(t *testing.T) {
		s := newBackend(t, "memstore").open()
		createLedgerRun(t, s)
		l := &countingLimiter{}
		mc := newModelClient(t, scripted(ledgerScript), agent.ModelClientConfig{Limiter: l})
		if _, err := activateClient(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s), mc, 1, agent.Budgets{}); err != nil {
			t.Fatal(err)
		}
		if l.reserves.Load() != 1 || l.commits.Load() != 1 || l.releases.Load() != 1 || l.rollbacks.Load() != 0 {
			t.Fatalf("reserves %d, commits %d, releases %d, rollbacks %d", l.reserves.Load(), l.commits.Load(), l.releases.Load(), l.rollbacks.Load())
		}
	})
	t.Run("refused retry", func(t *testing.T) {
		s := newBackend(t, "memstore").open()
		createLedgerRun(t, s)
		l := &countingLimiter{}
		mc := newModelClient(t, failingFirst(llms.ProviderAnthropic), agent.ModelClientConfig{Limiter: l, Retry: fastRetry})
		out, err := activateClient(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s), mc, 0,
			agent.Budgets{Tokens: agent.TokenBudget{Limit: 1_000_000}})
		if err != nil {
			t.Fatal(err)
		}
		wantBudgetFailure(t, out, agent.ReasonBudgetExhausted, agent.BudgetTokens)
		// The failed attempt was sent: released, nothing to commit. The
		// refused retry was not: rolled back.
		if l.reserves.Load() != 2 || l.commits.Load() != 0 || l.releases.Load() != 1 || l.rollbacks.Load() != 1 {
			t.Fatalf("reserves %d, commits %d, releases %d, rollbacks %d", l.reserves.Load(), l.commits.Load(), l.releases.Load(), l.rollbacks.Load())
		}
	})
}

// TestModelClientWrapUpDuringLimiterWait: a rate limiter's wait that
// reaches the wrap-up point begins the wrap-up; no attempt was sent, so
// none is recorded as failed (ADR 0010).
func TestModelClientWrapUpDuringLimiterWait(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	base := &testllm.FakeChatClient{
		ModelRef: llms.ModelRef{Provider: "fake", Name: "wrap"},
		Func: func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
			return callsResp(tc("s", "submit", `{}`)), nil
		},
	}
	l := &countingLimiter{block: true}
	mc := newModelClient(t, base, agent.ModelClientConfig{Limiter: l, Retry: &middleware.RetryConfig{MaxAttempts: 1}})
	const reserve = 2 * time.Second
	reg := registry(t, wrapType(&wrapLog{}, agent.WrapUp{Tools: []string{"submit"}, Reserve: reserve}))
	out, err := activateClient(context.Background(), agent.NewController(s, reg), claim(t, s), mc, 0, agent.Budgets{ActiveTime: reserve + time.Second})
	if err != nil || out.Status != store.StatusSucceeded {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
	es := entries(t, s, "r1")
	at := attempts(t, es)
	if countKind(es, agent.EntryModelFailure) != 0 || len(at) != 1 || !at[0].WrapUp || len(base.Calls()) != 1 {
		t.Fatalf("record = %v, attempts %+v", kinds(es), at)
	}
}

// TestModelClientEstimatedRetryMustFit: a retry under an estimated budget is
// refused when its whole estimate no longer fits after the failed attempt
// was charged, so the run never overshoots by a second reservation.
func TestModelClientEstimatedRetryMustFit(t *testing.T) {
	const capTokens = 50
	typ := func() *agent.Type[ledgerData] {
		typ := ledgerType(&execLog{})
		prompt := typ.States[0].Model.Prompt
		typ.States[0].Model.Prompt = func(ctx context.Context, env *agent.Env, d ledgerData) (llms.ChatRequest, error) {
			req, err := prompt(ctx, env, d)
			req.MaxTokens = capTokens
			return req, err
		}
		return typ
	}
	// Learn the first request's estimate from a run with no budget.
	ps := newBackend(t, "memstore").open()
	createLedgerRun(t, ps)
	probe := scripted(ledgerScript)
	if _, err := activateWith(context.Background(), agent.NewController(ps, registry(t, typ())), claim(t, ps), probe, 1); err != nil {
		t.Fatal(err)
	}
	one := int64(middleware.DefaultEstimator{}.EstimateChat(probe.Calls()[0]).Tokens())

	// The first attempt fits with room to spare; the retry's input would
	// still fit, but its whole estimate does not.
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	base := failingFirst("fake")
	out, err := activateClient(context.Background(), agent.NewController(s, registry(t, typ())), claim(t, s),
		newModelClient(t, base, agent.ModelClientConfig{Retry: fastRetry}), 0, agent.Budgets{Tokens: agent.TokenBudget{Limit: 2*one - 1, Estimated: true}})
	if err != nil {
		t.Fatal(err)
	}
	wantBudgetFailure(t, out, agent.ReasonBudgetExhausted, agent.BudgetTokens)
	if len(base.Calls()) != 1 || len(attempts(t, entries(t, s, "r1"))) != 1 || out.Consumed.Tokens != one {
		t.Fatalf("%d calls; consumed %+v, want the first attempt's %d", len(base.Calls()), out.Consumed, one)
	}
}
