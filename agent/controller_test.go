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
)

func TestRunToCompletion(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createRun(t, s, "r1", `{"target":3}`)
		cs := &countingStore{Store: s}
		out, err := activate(context.Background(), agent.NewController(cs, registry(t, counterType())), claim(t, s), "r1", 0)
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
		want := agent.Outcome{Status: store.StatusSucceeded, Reason: store.Reason{Code: agent.ReasonChanged}, Steps: 3, CheckpointSeq: 4}
		if out.Consumed.ActiveTime <= 0 || out.Consumed.Turns != 0 || out.Consumed.Tokens != 0 {
			t.Fatalf("consumed = %+v, want active time only", out.Consumed)
		}
		out.Consumed = agent.Consumption{}
		if fmt.Sprint(out) != fmt.Sprint(want) {
			t.Fatalf("outcome = %+v, want %+v", out, want)
		}
		// activation_started; run_started with the first checkpoint; one
		// commit per step, the last carrying the ending.
		if cs.n != 5 {
			t.Fatalf("commits = %d, want 5", cs.n)
		}
		es := entries(t, s, "r1")
		wantKinds := []string{"activation_started", "run_started", "transition", "transition", "transition", "outcome"}
		if !slices.Equal(kinds(es), wantKinds) {
			t.Fatalf("entries = %v, want %v", kinds(es), wantKinds)
		}
		for i, step := range []uint64{1, 1, 1, 2, 3, 3} {
			if es[i].Step != step {
				t.Fatalf("entry %d (%s) step = %d, want %d", i+1, es[i].Kind, es[i].Step, step)
			}
		}
		r := runRecord(t, s, "r1")
		if r.Status != store.StatusSucceeded || r.Reason.Code != agent.ReasonChanged || count(t, s, "r1") != 3 {
			t.Fatalf("run = %s/%s, count %d", r.Status, r.Reason.Code, count(t, s, "r1"))
		}
		wantReleased(t, s)
		ck, ok, err := s.LatestCheckpoint(context.Background(), inst)
		if err != nil || !ok || ck.Seq != 4 || ck.RecordSeq != 6 {
			t.Fatalf("latest checkpoint = %+v, %v, %v", ck, ok, err)
		}
	})
}

// A crash at any durable boundary, before or after the commit lands, is
// recovered by a new activation: no step is duplicated or lost.
func TestRecoveryFromEveryCommit(t *testing.T) {
	const commits = 5 // TestRunToCompletion's count for target 3
	for _, name := range []string{"memstore", "filestore"} {
		for _, landing := range []bool{false, true} {
			for at := 1; at <= commits; at++ {
				t.Run(fmt.Sprintf("%s/landing=%v/commit=%d", name, landing, at), func(t *testing.T) {
					recoverFrom(t, newBackend(t, name), at, landing)
				})
			}
		}
	}
}

func recoverFrom(t *testing.T, b backend, at int, landing bool) {
	ctx := context.Background()
	reg := registry(t, counterType())
	s := b.open()
	createRun(t, s, "r1", `{"target":3}`)
	f := claim(t, s)
	_, err := activate(ctx, agent.NewController(&crashStore{Store: s, at: at, landing: landing}, reg), f, "r1", 0)
	if !errors.Is(err, agent.ErrStoreFailure) || !errors.Is(err, errCrash) {
		t.Fatalf("crashed Activate = %v, want a store failure", err)
	}
	// The process dies: the file store is reopened, which drops every
	// lease; the memory store's claim is released as an expired lease
	// would be. A landed final commit has released it already.
	if b.durable {
		_ = s.Close()
		s = b.open()
	} else {
		_ = s.Release(ctx, f)
	}
	defer s.Close()
	activations := 1
	// The host rereads the run record and retries a run that is not
	// terminal (ADR 0003, item 14).
	if !runRecord(t, s, "r1").Status.Terminal() {
		activations++
		out, err := activate(ctx, agent.NewController(s, reg), claim(t, s), "r1", 0)
		if err != nil || out.Status != store.StatusSucceeded || out.Steps != 3 {
			t.Fatalf("continuing Activate = %+v, %v", out, err)
		}
	}
	if r := runRecord(t, s, "r1"); r.Status != store.StatusSucceeded {
		t.Fatalf("run status = %s", r.Status)
	}
	if n := count(t, s, "r1"); n != 3 {
		t.Fatalf("count = %d, want 3: a step was duplicated or lost", n)
	}
	var steps []uint64
	started, runStarts := 0, 0
	for _, e := range entries(t, s, "r1") {
		switch e.Kind {
		case agent.EntryTransition:
			steps = append(steps, e.Step)
		case agent.EntryActivationStarted:
			started++
		case agent.EntryRunStarted:
			runStarts++
			if len(steps) > 0 {
				t.Fatal("run_started after a transition")
			}
		}
	}
	if runStarts != 1 {
		t.Fatalf("run_started entries = %d, want 1", runStarts)
	}
	// An activation whose first commit never landed left no record.
	if at == 1 && !landing {
		activations--
	}
	if !slices.Equal(steps, []uint64{1, 2, 3}) {
		t.Fatalf("transition steps = %v, want [1 2 3]", steps)
	}
	if started != activations {
		t.Fatalf("activation_started entries = %d, want %d", started, activations)
	}
	wantReleased(t, s)
}

func TestStepLimit(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		ctx := context.Background()
		createRun(t, s, "r1", `{"target":3}`)
		c := agent.NewController(s, registry(t, counterType()))
		for i := 1; i <= 2; i++ {
			out, err := activate(ctx, c, claim(t, s), "r1", 1)
			if err != nil || out.Status != store.StatusRunnable || out.Reason.Code != agent.ReasonStepLimit || out.Steps != uint64(i) {
				t.Fatalf("activation %d = %+v, %v", i, out, err)
			}
			// Released in the final commit, so ready again at once.
			if cl, err := s.Claimable(ctx); err != nil || len(cl) != 1 || cl[0].Run != "r1" {
				t.Fatalf("Claimable = %v, %v", cl, err)
			}
		}
		out, err := activate(ctx, c, claim(t, s), "r1", 1)
		if err != nil || out.Status != store.StatusSucceeded || out.Steps != 3 {
			t.Fatalf("last activation = %+v, %v", out, err)
		}
		want := []string{
			"activation_started", "run_started", "transition", "outcome",
			"activation_started", "transition", "outcome",
			"activation_started", "transition", "outcome",
		}
		if got := kinds(entries(t, s, "r1")); !slices.Equal(got, want) {
			t.Fatalf("entries = %v, want %v", got, want)
		}
	})
}

func TestEndingByContextCause(t *testing.T) {
	cases := []struct {
		cause  error
		status store.Status
		reason store.Reason
	}{
		{agent.ErrSuperseded, store.StatusCancelled, store.Reason{Code: agent.ReasonSuperseded}},
		{fmt.Errorf("wrapped: %w", agent.ErrOperator), store.StatusCancelled, store.Reason{Code: agent.ReasonOperator}},
		{&agent.BudgetExhausted{Budget: "deadline"}, store.StatusFailed, store.Reason{Code: agent.ReasonBudgetExhausted, Budget: "deadline"}},
		{errors.New("SIGTERM"), store.StatusRunnable, store.Reason{Code: agent.ReasonShutdown}},
		{nil, store.StatusRunnable, store.Reason{Code: agent.ReasonShutdown}},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.cause), func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, b backend) {
				s := b.open()
				defer s.Close()
				createRun(t, s, "r1", `{"target":3}`)
				// Cancelled while the second step runs: the state sees its
				// context end and returns its error.
				ctx, cancel := context.WithCancelCause(context.Background())
				typ := counterType()
				typ.States[0].Code = func(ctx context.Context, env *agent.Env, d counterData) (agent.Transition[counterData], error) {
					if env.Step() == 2 {
						cancel(tc.cause)
						return agent.Transition[counterData]{}, context.Cause(ctx)
					}
					return countStep(ctx, env, d)
				}
				out, err := activate(ctx, agent.NewController(s, registry(t, typ)), claim(t, s), "r1", 0)
				if err != nil || out.Status != tc.status || out.Reason != tc.reason || out.Steps != 1 {
					t.Fatalf("outcome = %+v, %v; want %s/%+v after 1 step", out, err, tc.status, tc.reason)
				}
				r := runRecord(t, s, "r1")
				if r.Status != tc.status || r.Reason != tc.reason {
					t.Fatalf("run = %s/%+v", r.Status, r.Reason)
				}
				wantReleased(t, s)
			})
		})
	}
}

// An activation whose context has already ended still records its start
// and commits its ending.
func TestCancelledBeforeActivation(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createRun(t, s, "r1", `{"target":3}`)
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(agent.ErrOperator)
		out, err := activate(ctx, agent.NewController(s, registry(t, counterType())), claim(t, s), "r1", 0)
		if err != nil || out.Status != store.StatusCancelled || out.Steps != 0 {
			t.Fatalf("outcome = %+v, %v", out, err)
		}
		if got := kinds(entries(t, s, "r1")); !slices.Equal(got, []string{"activation_started", "run_started", "outcome"}) {
			t.Fatalf("entries = %v", got)
		}
	})
}

// failingReader fails object reads, as a broken store would.
type failingReader struct{ store.Store }

var errDisk = errors.New("disk gone")

func (f failingReader) Object(context.Context, string, string) (store.Object, bool, error) {
	return store.Object{}, false, errDisk
}

func TestCodeStateFailures(t *testing.T) {
	bad := func(code func(context.Context, *agent.Env, counterData) (agent.Transition[counterData], error)) *agent.Type[counterData] {
		typ := counterType()
		typ.States[0].Code = code
		return typ
	}
	cases := []struct {
		typ    *agent.Type[counterData]
		detail string
	}{
		{bad(func(context.Context, *agent.Env, counterData) (agent.Transition[counterData], error) {
			return agent.Transition[counterData]{}, errors.New("cannot parse statement")
		}), "cannot parse statement"},
		{bad(func(context.Context, *agent.Env, counterData) (agent.Transition[counterData], error) {
			panic("boom")
		}), "panic: boom"},
		{bad(func(_ context.Context, _ *agent.Env, d counterData) (agent.Transition[counterData], error) {
			return agent.Goto("elsewhere", d), nil
		}), `does not declare`},
		{bad(func(_ context.Context, env *agent.Env, d counterData) (agent.Transition[counterData], error) {
			return agent.Goto("done", d, store.AppendEntryOp(store.Entry{Run: env.RunID(), Seq: 99, Kind: "forged"})), nil
		}), "object operations only"},
		{bad(func(_ context.Context, _ *agent.Env, d counterData) (agent.Transition[counterData], error) {
			return agent.Goto("done", d, store.PutObjectJSON(ns, "k", 0, json.RawMessage(`{`))), nil
		}), "not valid JSON"},
		{bad(func(_ context.Context, _ *agent.Env, d counterData) (agent.Transition[counterData], error) {
			// Valid as an operation, but longer than any store key.
			return agent.Goto("done", d, store.PutObjectJSON(ns, strings.Repeat("k", 300), 0, json.RawMessage(`1`))), nil
		}), "invalid request"},
		{func() *agent.Type[counterData] {
			typ := counterType()
			typ.Init = func(context.Context, *agent.Env) (counterData, error) {
				return counterData{}, errors.New("trigger has no document")
			}
			return typ
		}(), "init: trigger has no document"},
	}
	for _, tc := range cases {
		t.Run(tc.detail, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, b backend) {
				s := b.open()
				defer s.Close()
				createRun(t, s, "r1", `{"target":3}`)
				out, err := activate(context.Background(), agent.NewController(s, registry(t, tc.typ)), claim(t, s), "r1", 0)
				if err != nil || out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonStateError || !strings.Contains(out.Detail, tc.detail) {
					t.Fatalf("outcome = %+v, %v; want state_error with %q", out, err, tc.detail)
				}
				if count(t, s, "r1") != 0 {
					t.Fatal("a refused transition's operations landed")
				}
				es := entries(t, s, "r1")
				var oe agent.OutcomeEntry
				if err := json.Unmarshal(es[len(es)-1].Payload, &oe); err != nil || oe.Detail != out.Detail {
					t.Fatalf("outcome entry = %+v, %v", oe, err)
				}
				wantReleased(t, s)
			})
		})
	}
}

// A store read failure inside a code state is a store failure, not a run
// failure (ADR 0003, item 15): the run stays runnable.
func TestReadFailureIsStoreFailure(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createRun(t, s, "r1", `{"target":3}`)
		_, err := activate(context.Background(), agent.NewController(failingReader{s}, registry(t, counterType())), claim(t, s), "r1", 0)
		if !errors.Is(err, agent.ErrStoreFailure) || !errors.Is(err, errDisk) {
			t.Fatalf("Activate = %v, want a store failure wrapping the read error", err)
		}
		if r := runRecord(t, s, "r1"); r.Status != store.StatusRunnable {
			t.Fatalf("run status = %s, want runnable", r.Status)
		}
	})
}

// interfering returns a counter type whose state, the first n times it
// runs, lets another instance write the counter after the state has read
// it, so the state's commit conflicts.
func interfering(s store.Store, n int, runs *int) *agent.Type[counterData] {
	typ := counterType()
	typ.States[0].Code = func(ctx context.Context, env *agent.Env, d counterData) (agent.Transition[counterData], error) {
		*runs++
		tr, err := countStep(ctx, env, d)
		if err != nil || *runs > n {
			return tr, err
		}
		other, err := s.Claim(ctx, "other", "x", time.Minute)
		if err != nil {
			return tr, err
		}
		obj, ok, _ := s.Object(ctx, ns, env.RunID())
		if !ok {
			obj.JSON = json.RawMessage("0")
		}
		err = s.Commit(ctx, other.Fence(), []store.Op{store.PutObjectJSON(ns, env.RunID(), obj.Version, obj.JSON)})
		if err == nil {
			err = s.Release(ctx, other.Fence())
		}
		return tr, err
	}
	typ.Init = func(context.Context, *agent.Env) (counterData, error) { return counterData{Target: 1}, nil }
	return typ
}

func TestObjectConflict(t *testing.T) {
	cases := []struct {
		name      string
		status    store.Status
		opts      []agent.Option
		interfere int
		runs      int
	}{
		{name: "retried", interfere: 3, runs: 4, status: store.StatusSucceeded},
		{name: "bound", interfere: 4, runs: 4, status: store.StatusFailed},
		{name: "no retries", opts: []agent.Option{agent.WithConflictRetries(0)}, interfere: 1, runs: 1, status: store.StatusFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, b backend) {
				s := b.open()
				defer s.Close()
				createRun(t, s, "r1", `{}`)
				runs := 0
				out, err := activate(context.Background(), agent.NewController(s, registry(t, interfering(s, tc.interfere, &runs)), tc.opts...), claim(t, s), "r1", 0)
				if err != nil || out.Status != tc.status || runs != tc.runs {
					t.Fatalf("outcome = %+v, %v after %d runs; want %s after %d", out, err, runs, tc.status, tc.runs)
				}
				if tc.status == store.StatusFailed && out.Reason.Code != agent.ReasonObjectConflict {
					t.Fatalf("reason = %q, want object_conflict", out.Reason.Code)
				}
			})
		})
	}
}

// A claim taken over mid-run rejects the old owner's next commit: the
// activation returns ownership lost and its transition never lands.
func TestOwnershipLost(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createRun(t, s, "r1", `{"target":3}`)
		f := claim(t, s)
		typ := counterType()
		typ.States[0].Code = func(ctx context.Context, env *agent.Env, d counterData) (agent.Transition[counterData], error) {
			if env.Step() == 2 {
				if err := s.Release(ctx, f); err != nil {
					return agent.Transition[counterData]{}, err
				}
				if _, err := s.Claim(ctx, inst, "thief", time.Minute); err != nil {
					return agent.Transition[counterData]{}, err
				}
			}
			return countStep(ctx, env, d)
		}
		_, err := activate(context.Background(), agent.NewController(s, registry(t, typ)), f, "r1", 0)
		if !errors.Is(err, agent.ErrOwnershipLost) || !errors.Is(err, store.ErrOwnership) {
			t.Fatalf("Activate = %v, want ownership lost", err)
		}
		if n := count(t, s, "r1"); n != 1 {
			t.Fatalf("count = %d, want 1", n)
		}
		if r := runRecord(t, s, "r1"); r.Status != store.StatusRunnable {
			t.Fatalf("run status = %s", r.Status)
		}
	})
}

func TestStaleClaimWritesNothing(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		ctx := context.Background()
		createRun(t, s, "r1", `{"target":3}`)
		f := claim(t, s)
		if err := s.Release(ctx, f); err != nil {
			t.Fatal(err)
		}
		claim(t, s)
		_, err := activate(ctx, agent.NewController(s, registry(t, counterType())), f, "r1", 0)
		if !errors.Is(err, agent.ErrOwnershipLost) {
			t.Fatalf("Activate = %v, want ownership lost", err)
		}
		if es := entries(t, s, "r1"); len(es) != 0 {
			t.Fatalf("entries = %v, want none", kinds(es))
		}
	})
}

func TestNotActivatable(t *testing.T) {
	s := newBackend(t, "memstore").open()
	ctx := context.Background()
	createRun(t, s, "r1", `{"target":1}`)
	createRunOn := func(id, instance, typ string) {
		if _, _, err := s.CreateRun(ctx, store.Run{ID: id, Instance: instance, AgentType: typ, TriggerID: id}); err != nil {
			t.Fatal(err)
		}
	}
	createRunOn("r2", "inst-2", "unregistered")
	createRunOn("r3", "inst-3", "counter")
	c := agent.NewController(s, registry(t, counterType()))
	f := claim(t, s)
	if out, err := activate(ctx, c, f, "r1", 0); err != nil || out.Status != store.StatusSucceeded {
		t.Fatalf("Activate = %+v, %v", out, err)
	}
	claimOn := func(instance string) store.Fence {
		own, err := s.Claim(ctx, instance, "host", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return own.Fence()
	}
	cases := []struct {
		name string
		in   agent.ActivationInput
	}{
		{"missing run", agent.ActivationInput{Fence: claim(t, s), Scope: store.Scope{Kind: store.ScopeRun, ID: "nope"}}},
		{"terminal run", agent.ActivationInput{Fence: f, Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}}},
		{"unregistered type", agent.ActivationInput{Fence: claimOn("inst-2"), Scope: store.Scope{Kind: store.ScopeRun, ID: "r2"}}},
		{"other instance", agent.ActivationInput{Fence: f, Scope: store.Scope{Kind: store.ScopeRun, ID: "r3"}}},
		{"not a run scope", agent.ActivationInput{Fence: f, Scope: store.Scope{Kind: "chat_turn", ID: "r1"}}},
		{"no owner", agent.ActivationInput{Fence: store.Fence{Instance: inst}, Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := s.Entries(ctx, tc.in.Scope.ID, 0)
			if _, err := c.Activate(ctx, tc.in); !errors.Is(err, agent.ErrNotActivatable) {
				t.Fatalf("Activate = %v, want ErrNotActivatable", err)
			}
			if after, _ := s.Entries(ctx, tc.in.Scope.ID, 0); len(after) != len(before) {
				t.Fatalf("entries grew from %d to %d", len(before), len(after))
			}
		})
	}
}

func TestRegistration(t *testing.T) {
	code := countStep
	valid := func() *agent.Type[counterData] { return counterType() }
	mutate := func(f func(*agent.Type[counterData])) *agent.Type[counterData] {
		typ := valid()
		f(typ)
		return typ
	}
	cases := map[string]*agent.Type[counterData]{
		"no name":         mutate(func(t *agent.Type[counterData]) { t.Name = "" }),
		"no initial":      mutate(func(t *agent.Type[counterData]) { t.Initial = "missing" }),
		"terminal start":  mutate(func(t *agent.Type[counterData]) { t.Initial = "done" }),
		"duplicate state": mutate(func(t *agent.Type[counterData]) { t.States = append(t.States, t.States[1]) }),
		"code and end":    mutate(func(t *agent.Type[counterData]) { t.States[0].End = agent.EndChanged }),
		"neither":         mutate(func(t *agent.Type[counterData]) { t.States[1].End = "" }),
		"undeclared next": mutate(func(t *agent.Type[counterData]) { t.States[0].Next = []string{"nowhere"} }),
		"duplicate next":  mutate(func(t *agent.Type[counterData]) { t.States[0].Next = []string{"done", "done"} }),
		"no next":         mutate(func(t *agent.Type[counterData]) { t.States[0].Next = nil }),
		"terminal next": mutate(func(t *agent.Type[counterData]) {
			t.States[1].Next = []string{"count"}
		}),
		"zero end": mutate(func(t *agent.Type[counterData]) { t.States[1].End = agent.End("cancelled") }),
		"unnamed state": mutate(func(t *agent.Type[counterData]) {
			t.States = append(t.States, agent.State[counterData]{Code: code, Next: []string{"done"}})
		}),
	}
	for name, typ := range cases {
		t.Run(name, func(t *testing.T) {
			if err := agent.NewRegistry().Register(typ); !errors.Is(err, agent.ErrInvalidType) {
				t.Fatalf("Register = %v, want ErrInvalidType", err)
			}
		})
	}
	r := registry(t, valid())
	if err := r.Register(valid()); !errors.Is(err, agent.ErrInvalidType) {
		t.Fatalf("second Register = %v, want ErrInvalidType", err)
	}
}

// The configuration is fixed when the run starts and recorded as an
// artifact; later activations use it, whatever their input says (RUN-1).
func TestConfigurationRecorded(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		ctx := context.Background()
		createRun(t, s, "r1", `{"target":2}`)
		var seen []string
		typ := counterType()
		typ.States[0].Code = func(ctx context.Context, env *agent.Env, d counterData) (agent.Transition[counterData], error) {
			seen = append(seen, string(env.Config()))
			return countStep(ctx, env, d)
		}
		c := agent.NewController(s, registry(t, typ))
		for _, cfg := range []string{`{"model":"a"}`, `{"model":"b"}`} {
			_, err := c.Activate(ctx, agent.ActivationInput{
				Fence: claim(t, s), Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}, Config: json.RawMessage(cfg), MaxSteps: 1,
			})
			if err != nil {
				t.Fatalf("Activate: %v", err)
			}
		}
		if !slices.Equal(seen, []string{`{"model":"a"}`, `{"model":"a"}`}) {
			t.Fatalf("configurations seen = %v", seen)
		}
		var rs agent.RunStarted
		if err := json.Unmarshal(entries(t, s, "r1")[1].Payload, &rs); err != nil {
			t.Fatal(err)
		}
		if _, cfg, err := s.Artifact(ctx, rs.Config); err != nil || string(cfg) != `{"model":"a"}` {
			t.Fatalf("configuration artifact = %q, %v", cfg, err)
		}
	})
}

// A run never restores an earlier run's checkpoint, though checkpoint
// sequences continue across the instance's runs (ADR 0004, item 3).
func TestEarlierRunsCheckpointIgnored(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		ctx := context.Background()
		c := agent.NewController(s, registry(t, counterType()))
		createRun(t, s, "r1", `{"target":3}`)
		if _, err := activate(ctx, c, claim(t, s), "r1", 0); err != nil {
			t.Fatal(err)
		}
		createRun(t, s, "r2", `{"target":2}`)
		out, err := activate(ctx, c, claim(t, s), "r2", 0)
		if err != nil || out.Status != store.StatusSucceeded || out.Steps != 2 || out.CheckpointSeq != 7 {
			t.Fatalf("second run = %+v, %v", out, err)
		}
		if n, r := count(t, s, "r2"), runRecord(t, s, "r2"); n != 2 || r.Status != store.StatusSucceeded {
			t.Fatalf("second run: count %d, status %s", n, r.Status)
		}
		// Each run's record is numbered from 1.
		if es := entries(t, s, "r2"); es[0].Seq != 1 || es[1].Kind != agent.EntryRunStarted {
			t.Fatalf("second run's record starts %v", es[:2])
		}
	})
}

func TestMigrate(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		ctx := context.Background()
		createRun(t, s, "r1", `{"target":3}`)
		v1 := agent.NewController(s, registry(t, counterType()))
		if _, err := activate(ctx, v1, claim(t, s), "r1", 1); err != nil {
			t.Fatal(err)
		}
		// Version 2 without Migrate refuses version 1's checkpoint.
		v2 := counterType()
		v2.Version = "2"
		out, err := activate(ctx, agent.NewController(s, registry(t, v2)), claim(t, s), "r1", 0)
		if err != nil || out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonStateError {
			t.Fatalf("unmigrated = %+v, %v", out, err)
		}
		// With Migrate it continues from the migrated data.
		createRun(t, s, "r2", `{"target":3}`)
		if _, err = activate(ctx, v1, claim(t, s), "r2", 1); err != nil {
			t.Fatal(err)
		}
		var from []string
		v2.Migrate = func(_ context.Context, version string, data json.RawMessage) (counterData, error) {
			from = append(from, version)
			var d counterData
			derr := json.Unmarshal(data, &d)
			d.Target = 2
			return d, derr
		}
		out, err = activate(ctx, agent.NewController(s, registry(t, v2)), claim(t, s), "r2", 0)
		if err != nil || out.Status != store.StatusSucceeded || out.Steps != 2 || !slices.Equal(from, []string{"1"}) {
			t.Fatalf("migrated = %+v, %v, from %v", out, err, from)
		}
	})
}

func TestEnv(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createRun(t, s, "r1", `{"target":1}`)
	var got []string
	typ := counterType()
	init := typ.Init
	typ.Init = func(ctx context.Context, env *agent.Env) (counterData, error) {
		got = append(got, fmt.Sprintf("%s %s %v %s %s %d", env.RunID(), env.Instance(), env.Scope(), env.TriggerID(), env.State(), env.Step()))
		return init(ctx, env)
	}
	typ.States[0].Code = func(ctx context.Context, env *agent.Env, d counterData) (agent.Transition[counterData], error) {
		got = append(got, fmt.Sprintf("%s %d %s", env.State(), env.Step(), env.Trigger()))
		return countStep(ctx, env, d)
	}
	if _, err := activate(context.Background(), agent.NewController(s, registry(t, typ)), claim(t, s), "r1", 0); err != nil {
		t.Fatal(err)
	}
	want := []string{"r1 inst-1 {run r1} trig-r1 count 1", `count 1 {"target":1}`}
	if !slices.Equal(got, want) {
		t.Fatalf("env = %q, want %q", got, want)
	}
}

// A state that returns a transition after its context ended has the result
// discarded: the cause decides the ending, terminal transition or not
// (RUN-2; Codex round 1).
func TestCauseBeatsStateResult(t *testing.T) {
	for _, to := range []string{"done", "count"} {
		t.Run(to, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, b backend) {
				s := b.open()
				defer s.Close()
				createRun(t, s, "r1", `{"target":3}`)
				ctx, cancel := context.WithCancelCause(context.Background())
				typ := counterType()
				typ.States[0].Code = func(ctx context.Context, env *agent.Env, d counterData) (agent.Transition[counterData], error) {
					tr, err := countStep(ctx, env, d)
					tr.To = to
					cancel(agent.ErrOperator)
					return tr, err
				}
				out, err := activate(ctx, agent.NewController(s, registry(t, typ)), claim(t, s), "r1", 0)
				if err != nil || out.Status != store.StatusCancelled || out.Reason.Code != agent.ReasonOperator || out.Steps != 0 {
					t.Fatalf("outcome = %+v, %v; want cancelled by the operator", out, err)
				}
				if n := count(t, s, "r1"); n != 0 {
					t.Fatalf("count = %d: the discarded transition's operations landed", n)
				}
				if got := kinds(entries(t, s, "r1")); !slices.Equal(got, []string{"activation_started", "run_started", "outcome"}) {
					t.Fatalf("entries = %v", got)
				}
			})
		})
	}
}

// An ended context also decides an ending the runtime would otherwise
// choose itself, such as a checkpoint it cannot migrate.
func TestCauseBeatsRuntimeFailure(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createRun(t, s, "r1", `{"target":3}`)
		if _, err := activate(context.Background(), agent.NewController(s, registry(t, counterType())), claim(t, s), "r1", 1); err != nil {
			t.Fatal(err)
		}
		v2 := counterType()
		v2.Version = "2"
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(agent.ErrSuperseded)
		out, err := activate(ctx, agent.NewController(s, registry(t, v2)), claim(t, s), "r1", 0)
		if err != nil || out.Status != store.StatusCancelled || out.Reason.Code != agent.ReasonSuperseded {
			t.Fatalf("outcome = %+v, %v; want cancelled as superseded", out, err)
		}
	})
}

// A run that ends while starting still records its start: the trigger, the
// agent type's version and the configuration (REC-1; Codex round 1). A
// non-terminal ending leaves the start to the next activation, so it is
// recorded once.
func TestStartRecordedOnTerminalStartFailure(t *testing.T) {
	failingInit := func(fn func(context.Context) error) *agent.Type[counterData] {
		typ := counterType()
		typ.Init = func(ctx context.Context, _ *agent.Env) (counterData, error) {
			return counterData{}, fn(ctx)
		}
		return typ
	}
	cases := []struct {
		typ    *agent.Type[counterData]
		cancel error
		name   string
		config string
		status store.Status
	}{
		{name: "init error", typ: failingInit(func(context.Context) error { return errors.New("no document") }),
			config: `{"model":"a"}`, status: store.StatusFailed},
		{name: "init panic", typ: failingInit(func(context.Context) error { panic("boom") }),
			config: `{"model":"a"}`, status: store.StatusFailed},
		{name: "invalid configuration", typ: counterType(), config: `{`, status: store.StatusFailed},
		{name: "cancelled", typ: failingInit(func(ctx context.Context) error { return context.Cause(ctx) }),
			cancel: agent.ErrOperator, config: `{"model":"a"}`, status: store.StatusCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, b backend) {
				s := b.open()
				defer s.Close()
				createRun(t, s, "r1", `{"target":3}`)
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				if tc.cancel != nil {
					cancel(tc.cancel)
				}
				out, err := agent.NewController(s, registry(t, tc.typ)).Activate(ctx, agent.ActivationInput{
					Fence: claim(t, s), Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}, Config: json.RawMessage(tc.config),
				})
				if err != nil || out.Status != tc.status {
					t.Fatalf("outcome = %+v, %v; want %s", out, err, tc.status)
				}
				es := entries(t, s, "r1")
				if got := kinds(es); !slices.Equal(got, []string{"activation_started", "run_started", "outcome"}) {
					t.Fatalf("entries = %v", got)
				}
				var rs agent.RunStarted
				if err := json.Unmarshal(es[1].Payload, &rs); err != nil || rs.AgentVersion != "1" || string(rs.Trigger) != `{"target":3}` {
					t.Fatalf("run_started = %+v, %v", rs, err)
				}
				if json.Valid([]byte(tc.config)) {
					if _, cfg, err := s.Artifact(context.Background(), rs.Config); err != nil || string(cfg) != tc.config {
						t.Fatalf("configuration artifact = %q, %v", cfg, err)
					}
				} else if rs.Config != "" {
					t.Fatalf("invalid configuration recorded as %q", rs.Config)
				}
			})
		})
	}
}

func TestShutdownDuringInitStartsOnce(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		createRun(t, s, "r1", `{"target":2}`)
		typ := counterType()
		init := typ.Init
		shut := true
		typ.Init = func(ctx context.Context, env *agent.Env) (counterData, error) {
			if shut {
				return counterData{}, context.Cause(ctx)
			}
			return init(ctx, env)
		}
		c := agent.NewController(s, registry(t, typ))
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(errors.New("SIGTERM"))
		out, err := activate(ctx, c, claim(t, s), "r1", 0)
		if err != nil || out.Status != store.StatusRunnable || out.Reason.Code != agent.ReasonShutdown {
			t.Fatalf("shutdown outcome = %+v, %v", out, err)
		}
		shut = false
		if out, err = activate(context.Background(), c, claim(t, s), "r1", 0); err != nil || out.Status != store.StatusSucceeded {
			t.Fatalf("second activation = %+v, %v", out, err)
		}
		want := []string{"activation_started", "outcome", "activation_started", "run_started", "transition", "transition", "outcome"}
		if got := kinds(entries(t, s, "r1")); !slices.Equal(got, want) {
			t.Fatalf("entries = %v, want %v", got, want)
		}
	})
}
