package agent_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

func TestCancelCause(t *testing.T) {
	for _, tc := range []struct {
		cause store.CancelCause
		want  error
	}{
		{store.CancelOperator, agent.ErrOperator},
		{store.CancelSuperseded, agent.ErrSuperseded},
		{store.CancelUnauthorized, agent.ErrUnauthorized},
		{"bored", nil},
		{"", nil},
	} {
		if got := agent.CancelCause(tc.cause); !errors.Is(got, tc.want) || (tc.want == nil) != (got == nil) {
			t.Errorf("CancelCause(%q) = %v, want %v", tc.cause, got, tc.want)
		}
	}
	// Each cause's reason is the request's cause, so a request and its
	// run's reason read alike (ADR 0024, item 6).
	for c, reason := range map[store.CancelCause]string{
		store.CancelOperator: agent.ReasonOperator, store.CancelSuperseded: agent.ReasonSuperseded,
		store.CancelUnauthorized: agent.ReasonUnauthorized,
	} {
		if string(c) != reason {
			t.Errorf("cause %q has reason %q", c, reason)
		}
	}
}

// A cancel request found when an activation starts cancels the run before
// it takes any step, with the request's cause (ADR 0024, item 6): a run
// not yet started records its start, and a started one ends at its last
// checkpoint, its count unchanged.
func TestCancelRequestAtStart(t *testing.T) {
	for _, tc := range []struct {
		cause  store.CancelCause
		reason string
	}{
		{store.CancelOperator, agent.ReasonOperator},
		{store.CancelSuperseded, agent.ReasonSuperseded},
		{store.CancelUnauthorized, agent.ReasonUnauthorized},
	} {
		t.Run(string(tc.cause), func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, b backend) {
				s := b.open()
				defer s.Close()
				ctrl := agent.NewController(s, registry(t, counterType()))
				ctx := context.Background()

				createRun(t, s, "r1", `{"target":3}`)
				if _, _, err := s.RequestCancel(ctx, "r1", tc.cause); err != nil {
					t.Fatalf("RequestCancel: %v", err)
				}
				out, err := activate(ctx, ctrl, claim(t, s), "r1", 0)
				want := store.Reason{Code: tc.reason}
				if err != nil || out.Status != store.StatusCancelled || out.Reason != want || out.Steps != 0 {
					t.Fatalf("outcome = %+v, %v; want cancelled/%s with no step", out, err, tc.reason)
				}
				if got := kinds(entries(t, s, "r1")); !slices.Equal(got, []string{"activation_started", "run_started", "outcome"}) {
					t.Fatalf("entries = %v", got)
				}
				if r := runRecord(t, s, "r1"); r.Status != store.StatusCancelled || r.Reason != want {
					t.Fatalf("run = %s/%+v", r.Status, r.Reason)
				}
				wantReleased(t, s)

				// A started run: one step, then the request.
				createRun(t, s, "r2", `{"target":3}`)
				out, err = activate(ctx, ctrl, claim(t, s), "r2", 1)
				if err != nil || out.Status != store.StatusRunnable || count(t, s, "r2") != 1 {
					t.Fatalf("first activation = %+v, %v", out, err)
				}
				if _, _, err = s.RequestCancel(ctx, "r2", tc.cause); err != nil {
					t.Fatalf("RequestCancel: %v", err)
				}
				before := len(entries(t, s, "r2"))
				out, err = activate(ctx, ctrl, claim(t, s), "r2", 0)
				if err != nil || out.Status != store.StatusCancelled || out.Reason != want || out.Steps != 1 {
					t.Fatalf("outcome = %+v, %v; want cancelled/%s with no step past the first", out, err, tc.reason)
				}
				if got := kinds(entries(t, s, "r2")[before:]); !slices.Equal(got, []string{"activation_started", "outcome"}) {
					t.Fatalf("second activation's entries = %v", got)
				}
				if n := count(t, s, "r2"); n != 1 {
					t.Fatalf("count = %d after cancel, want 1", n)
				}
			})
		})
	}
}

// A request's cause wins over a deadline already passed: the request
// cancels the activation before its clocks are installed.
func TestCancelRequestAtStartBeatsDeadline(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createRun(t, s, "r1", `{"target":3}`)
		if _, _, err := s.RequestCancel(context.Background(), "r1", store.CancelOperator); err != nil {
			t.Fatalf("RequestCancel: %v", err)
		}
		out, err := agent.NewController(s, registry(t, counterType())).Activate(context.Background(), agent.ActivationInput{
			Fence: claim(t, s), Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}, Namespace: ns,
			Budgets: agent.Budgets{Deadline: time.Now().Add(-time.Hour)},
		})
		if err != nil || out.Status != store.StatusCancelled || out.Reason.Code != agent.ReasonOperator {
			t.Fatalf("outcome = %+v, %v; want cancelled/operator", out, err)
		}
	})
}

// A request that is not one of the three causes is the store breaking its
// contract: the activation ends with a store failure before writing.
type badCause struct{ store.Store }

func (b badCause) CancelRequest(_ context.Context, run string) (store.CancelRequest, bool, error) {
	return store.CancelRequest{Run: run, Cause: "bored"}, true, nil
}

func TestCancelRequestUnknownCause(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createRun(t, s, "r1", `{"target":3}`)
		_, err := activate(context.Background(), agent.NewController(badCause{s}, registry(t, counterType())), claim(t, s), "r1", 0)
		if !errors.Is(err, agent.ErrStoreFailure) {
			t.Fatalf("Activate = %v, want ErrStoreFailure", err)
		}
		if es := entries(t, s, "r1"); len(es) != 0 {
			t.Fatalf("entries = %v, want none", kinds(es))
		}
	})
}
