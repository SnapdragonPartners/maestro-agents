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

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/memstore"
	"github.com/SnapdragonPartners/maestro-agents/store/storetest"
)

func status(t testing.TB, s store.Reader, run string) agent.RunStatus {
	t.Helper()
	st, err := agent.Status(context.Background(), s, run)
	if err != nil {
		t.Fatalf("Status(%s): %v", run, err)
	}
	return st
}

func instanceStatus(t testing.TB, s store.Reader) agent.InstanceReport {
	t.Helper()
	st, err := agent.InstanceStatus(context.Background(), s, inst)
	if err != nil {
		t.Fatalf("InstanceStatus: %v", err)
	}
	return st
}

// lastAt is the commit time of the run's latest entry.
func lastAt(t testing.TB, s store.Store, run string) time.Time {
	t.Helper()
	es := entries(t, s, run)
	return es[len(es)-1].At
}

// A run's status through its life (ADR 0024, item 7): queued, running
// under a live lease, queued again between activations with its state and
// step, stopping once asked to cancel, and terminal with its reason, its
// state gone and its budgets kept.
func TestStatusLifecycle(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		ctx := context.Background()
		ctrl := agent.NewController(s, registry(t, counterType()))
		deadline := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		budgets := agent.Budgets{Turns: 7, ActiveTime: time.Hour, Deadline: deadline, Tokens: agent.TokenBudget{Limit: 1000}}

		createRun(t, s, "r1", `{"target":3}`)
		st := status(t, s, "r1")
		r := runRecord(t, s, "r1")
		if st.Phase != agent.PhaseQueued || st.State != nil || st.Budgets != nil || st.Step != 0 ||
			st.CancelRequested != nil || st.Reason != (store.Reason{}) || !st.LastActivity.Equal(r.CreatedAt) ||
			st.Run != "r1" || st.Instance != inst || st.AgentType != "counter" || !st.CreatedAt.Equal(r.CreatedAt) {
			t.Fatalf("new run's status = %+v", st)
		}

		f := claim(t, s)
		if st = status(t, s, "r1"); st.Phase != agent.PhaseRunning {
			t.Fatalf("claimed run's phase = %s, want running", st.Phase)
		}
		out, err := ctrl.Activate(ctx, agent.ActivationInput{
			Fence: f, Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}, Namespace: ns, MaxSteps: 1, Budgets: budgets,
		})
		if err != nil || out.Status != store.StatusRunnable {
			t.Fatalf("first activation = %+v, %v", out, err)
		}
		st = status(t, s, "r1")
		if st.Phase != agent.PhaseQueued || st.State == nil || *st.State != (agent.StateStatus{Name: "count", Kind: agent.StateCode}) ||
			st.Step != 2 || !st.LastActivity.Equal(lastAt(t, s, "r1")) {
			t.Fatalf("status between activations = %+v, state %+v", st, st.State)
		}
		want := agent.BudgetUse{
			Deadline: &deadline, Tokens: agent.CountUse{Limit: 1000}, Turns: agent.CountUse{Limit: 7},
			ActiveTime: agent.TimeUse{Used: st.Budgets.ActiveTime.Used, Limit: time.Hour},
		}
		if st.Budgets == nil || !st.Budgets.Deadline.Equal(deadline) || st.Budgets.Tokens != want.Tokens ||
			st.Budgets.Turns != want.Turns || st.Budgets.ActiveTime.Limit != time.Hour {
			t.Fatalf("budgets = %+v, want %+v", st.Budgets, want)
		}

		if _, _, err = s.RequestCancel(ctx, "r1", store.CancelOperator); err != nil {
			t.Fatal(err)
		}
		st = status(t, s, "r1")
		if st.Phase != agent.PhaseStopping || st.CancelRequested == nil || st.CancelRequested.Cause != store.CancelOperator {
			t.Fatalf("status with a cancel request = %+v", st)
		}
		out, err = activate(ctx, ctrl, claim(t, s), "r1", 0)
		if err != nil || out.Status != store.StatusCancelled {
			t.Fatalf("cancelling activation = %+v, %v", out, err)
		}
		st = status(t, s, "r1")
		r = runRecord(t, s, "r1")
		if st.Phase != agent.PhaseCancelled || st.Reason != (store.Reason{Code: agent.ReasonOperator}) || st.State != nil ||
			st.CancelRequested == nil || st.Budgets == nil || st.Budgets.Turns.Limit != 7 || st.Step != 2 ||
			!st.UpdatedAt.Equal(r.UpdatedAt) || st.LastActivity.Before(lastAt(t, s, "r1")) {
			t.Fatalf("cancelled run's status = %+v", st)
		}

		// A run that succeeds.
		createRun(t, s, "r2", `{"target":2}`)
		if out, err = activate(ctx, ctrl, claim(t, s), "r2", 0); err != nil || out.Status != store.StatusSucceeded {
			t.Fatalf("activation = %+v, %v", out, err)
		}
		st = status(t, s, "r2")
		if st.Phase != agent.PhaseSucceeded || st.Reason.Code != agent.ReasonChanged || st.State != nil || st.Budgets == nil ||
			st.Budgets.Turns.Limit != 0 || st.Budgets.Deadline != nil || st.Step == 0 || st.CancelRequested != nil {
			t.Fatalf("succeeded run's status = %+v", st)
		}
	})
}

// A run whose instance's latest checkpoint is a later run's reads its own
// record, and a run refused at its start has budgets but no state.
func TestStatusOfAnEarlierRun(t *testing.T) {
	s := memstore.New()
	ctx := context.Background()
	ctrl := agent.NewController(s, registry(t, counterType()))
	createRun(t, s, "r1", `{"target":2}`)
	if _, err := activate(ctx, ctrl, claim(t, s), "r1", 0); err != nil {
		t.Fatal(err)
	}
	before := status(t, s, "r1")
	createRun(t, s, "r2", `{"target":5}`)
	if _, err := activate(ctx, ctrl, claim(t, s), "r2", 1); err != nil {
		t.Fatal(err)
	}
	if after := status(t, s, "r1"); after.Step != before.Step || after.Phase != agent.PhaseSucceeded || after.State != nil {
		t.Fatalf("status of r1 after r2 started = %+v, before %+v", after, before)
	}

	// Refused at its start: a bad trigger kind.
	if _, _, err := s.CreateRun(ctx, store.Run{ID: "r0", Instance: "inst-2", AgentType: "counter", TriggerID: "t", TriggerKind: "nope", Trigger: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	own, err := s.Claim(ctx, "inst-2", "host", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ctrl.Activate(ctx, agent.ActivationInput{Fence: own.Fence(), Scope: store.Scope{Kind: store.ScopeRun, ID: "r0"}, Namespace: ns})
	if err != nil || out.Status != store.StatusFailed {
		t.Fatalf("refused activation = %+v, %v", out, err)
	}
	st := status(t, s, "r0")
	if st.Phase != agent.PhaseFailed || st.Reason.Code != agent.ReasonConfigError || st.State != nil || st.Budgets == nil {
		t.Fatalf("refused run's status = %+v", st)
	}
}

// A model run's status mid-step: the checkpoint's consumption charged the
// tail's attempts, as recovery would, with the state's kind; at every
// commit the turns used match the attempts recorded.
func TestStatusModelTail(t *testing.T) {
	total := func() int {
		s := &countingStore{Store: memstore.New()}
		createLedgerRun(t, s)
		if _, err := activateWith(context.Background(), agent.NewController(s, registry(t, ledgerType(&execLog{}))), claim(t, s), scripted(ledgerScript), 0); err != nil {
			t.Fatal(err)
		}
		return s.n
	}()
	sawTail := false
	for at := 3; at <= total; at++ {
		t.Run(fmt.Sprintf("commit-%d", at), func(t *testing.T) {
			s := memstore.New()
			createLedgerRun(t, s)
			cs := &crashStore{Store: s, at: at, landing: true}
			_, err := activateWith(context.Background(), agent.NewController(cs, registry(t, ledgerType(&execLog{}))), claim(t, s), scripted(ledgerScript), 0)
			if !errors.Is(err, agent.ErrStoreFailure) {
				t.Fatalf("crashed activation = %v", err)
			}
			if runRecord(t, s, "r1").Status.Terminal() {
				return
			}
			st := status(t, s, "r1")
			es := entries(t, s, "r1")
			attempts := countKind(es, agent.EntryModelAttempt)
			kind := map[string]agent.StateKind{"work": agent.StateModel, "review": agent.StateCode}
			if st.Phase != agent.PhaseRunning || st.State == nil || st.State.Kind == "" || kind[st.State.Name] != st.State.Kind ||
				st.Budgets == nil || st.Budgets.Turns.Used != int64(attempts) || !st.LastActivity.Equal(es[len(es)-1].At) {
				t.Fatalf("status = %+v, state %+v, budgets %+v; %d attempts recorded", st, st.State, st.Budgets, attempts)
			}
			cp, _, err := s.LatestCheckpoint(context.Background(), inst)
			if err != nil {
				t.Fatal(err)
			}
			if cp.RecordSeq < es[len(es)-1].Seq && countKind(es[cp.RecordSeq:], agent.EntryModelAttempt) > 0 {
				sawTail = true
			}
		})
	}
	if !sawTail {
		t.Fatal("no crash point left a model attempt past the checkpoint")
	}
}

// The lease is judged by the store's clock, not the reader's: a store
// clock far behind the wall clock still shows a fresh claim running, and
// one whose lease has expired by the store's clock shows queued.
func TestStatusStoreClock(t *testing.T) {
	clk := storetest.NewClock() // 2026-01-01, far from the wall clock
	s := memstore.New(memstore.WithClock(clk.Now))
	createRun(t, s, "r1", `{"target":3}`)
	claim(t, s)
	if st := status(t, s, "r1"); st.Phase != agent.PhaseRunning {
		t.Fatalf("phase under a fresh lease = %s, want running", st.Phase)
	}
	clk.Advance(2 * time.Minute)
	if st := status(t, s, "r1"); st.Phase != agent.PhaseQueued {
		t.Fatalf("phase after the lease expired by the store's clock = %s, want queued", st.Phase)
	}
}

func TestStatusErrors(t *testing.T) {
	s := memstore.New()
	if _, err := agent.Status(context.Background(), s, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Status(missing) = %v, want ErrNotFound", err)
	}
	if _, err := agent.InstanceStatus(context.Background(), s, ""); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("InstanceStatus(\"\") = %v, want ErrInvalid", err)
	}
}

// An instance's status: its active run, its newest succeeded and failed
// runs (a cancelled run is neither) and its queued triggers.
func TestInstanceStatus(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		ctx := context.Background()
		ctrl := agent.NewController(s, registry(t, counterType()))

		st := instanceStatus(t, s)
		if st.Instance != inst || st.Active != nil || st.LastSucceeded != nil || st.LastFailed != nil ||
			st.Queued.Count != 0 || st.Queued.Oldest != nil || st.Requests == nil {
			t.Fatalf("empty instance's status = %+v", st)
		}

		run := func(id, trigger string) agent.Outcome {
			t.Helper()
			createRun(t, s, id, trigger)
			out, err := activate(context.Background(), ctrl, claim(t, s), id, 0)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		run("r1", `{"target":1}`)
		if out := run("r2", `{"target":"x"}`); out.Status != store.StatusFailed {
			t.Fatalf("r2 = %+v, want failed", out)
		}
		run("r3", `{"target":2}`)
		createRun(t, s, "r4", `{"target":3}`)
		if _, _, err := s.RequestCancel(ctx, "r4", store.CancelOperator); err != nil {
			t.Fatal(err)
		}
		if _, err := activate(ctx, ctrl, claim(t, s), "r4", 0); err != nil {
			t.Fatal(err)
		}
		createRun(t, s, "r5", `{"target":3}`)
		for _, id := range []string{"q1", "q2"} {
			if _, err := s.EnqueueTrigger(ctx, store.Trigger{Instance: inst, ID: id, Kind: agent.TriggerOneShot}); err != nil {
				t.Fatal(err)
			}
		}

		st = instanceStatus(t, s)
		r2, r3 := runRecord(t, s, "r2"), runRecord(t, s, "r3")
		trs, err := s.Triggers(ctx, inst)
		if err != nil {
			t.Fatal(err)
		}
		if st.Active == nil || st.Active.Run != "r5" || st.Active.Phase != agent.PhaseQueued ||
			st.LastSucceeded == nil || *st.LastSucceeded != (agent.RunSummary{Run: "r3", At: r3.UpdatedAt, Reason: r3.Reason}) ||
			st.LastFailed == nil || *st.LastFailed != (agent.RunSummary{Run: "r2", At: r2.UpdatedAt, Reason: r2.Reason}) ||
			st.Queued.Count != 2 || st.Queued.Oldest == nil || !st.Queued.Oldest.Equal(trs[0].EnqueuedAt) {
			t.Fatalf("instance status = %+v", st)
		}
	})
}

// The JSON shapes of docs/contract.md section 2: every key present, the
// lists empty rather than null, durations as strings, absent values null.
func TestStatusJSON(t *testing.T) {
	s := memstore.New()
	ctx := context.Background()
	ctrl := agent.NewController(s, registry(t, counterType()))
	createRun(t, s, "r1", `{"target":3}`)
	if _, err := ctrl.Activate(ctx, agent.ActivationInput{
		Fence: claim(t, s), Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}, Namespace: ns, MaxSteps: 1,
		Budgets: agent.Budgets{Turns: 7, ActiveTime: 15 * time.Minute},
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(status(t, s, "r1"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{
		"agent_type", "agent_version", "artifacts", "budgets", "cancel_requested", "created_at", "instance",
		"last_activity", "phase", "reason", "requests", "run", "state", "step", "updated_at",
	}
	if !slices.Equal(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	for k, v := range map[string]string{
		"phase": `"queued"`, "reason": `{}`, "cancel_requested": `null`, "requests": `[]`, "artifacts": `[]`,
		"state": `{"name":"count","kind":"code"}`, "step": `2`,
	} {
		if string(got[k]) != v {
			t.Errorf("%s = %s, want %s", k, got[k], v)
		}
	}
	var budgets map[string]json.RawMessage
	if err = json.Unmarshal(got["budgets"], &budgets); err != nil {
		t.Fatal(err)
	}
	if string(budgets["turns"]) != `{"used":0,"limit":7}` || string(budgets["tokens"]) != `{"used":0}` || string(budgets["deadline"]) != `null` {
		t.Errorf("budgets = %s", got["budgets"])
	}
	var at agent.TimeUse
	if err = json.Unmarshal(budgets["active_time"], &at); err != nil || at.Limit != 15*time.Minute {
		t.Errorf("active_time = %s: %+v, %v", budgets["active_time"], at, err)
	}
	if err = json.Unmarshal([]byte(`{"used":"soon"}`), &at); err == nil {
		t.Error("a bad duration decoded")
	}

	inst, err := json.Marshal(instanceStatus(t, s))
	if err != nil {
		t.Fatal(err)
	}
	want2 := `"last_succeeded":null,"last_failed":null,"queued":{"oldest":null,"count":0},"instance":"inst-1","requests":[]}`
	if !strings.HasSuffix(string(inst), want2) || !strings.HasPrefix(string(inst), `{"active":{`) {
		t.Errorf("instance status = %s", inst)
	}
}
