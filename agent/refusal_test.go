package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

func TestRegisterNil(t *testing.T) {
	for name, d := range map[string]agent.Definition{
		"nil":       nil,
		"typed nil": (*agent.Type[counterData])(nil),
	} {
		t.Run(name, func(t *testing.T) {
			if err := agent.NewRegistry().Register(d); !errors.Is(err, agent.ErrInvalidType) {
				t.Fatalf("Register = %v, want ErrInvalidType", err)
			}
		})
	}
}

// Registration captures the declaration: changing the Type afterwards
// (to register another version, say) does not change the registered one.
func TestRegistrationIsASnapshot(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createRun(t, s, "r1", `{"target":1}`)
	typ := counterType()
	r := registry(t, typ)
	typ.Init = func(context.Context, *agent.Env) (counterData, error) {
		return counterData{}, errors.New("changed after registration")
	}
	typ.Migrate = func(context.Context, string, json.RawMessage) (counterData, error) {
		return counterData{}, errors.New("changed after registration")
	}
	out, err := activate(context.Background(), agent.NewController(s, r), claim(t, s), "r1", 0)
	if err != nil || out.Status != store.StatusSucceeded {
		t.Fatalf("outcome = %+v, %v", out, err)
	}

	// Migrate: version 2 resumes version 1's checkpoint with the Migrate
	// it was registered with.
	createRun(t, s, "r2", `{"target":2}`)
	if _, err = activate(context.Background(), agent.NewController(s, registry(t, counterType())), claim(t, s), "r2", 1); err != nil {
		t.Fatal(err)
	}
	v2 := counterType()
	v2.Version = "2"
	v2.Migrate = func(_ context.Context, _ string, data json.RawMessage) (counterData, error) {
		var d counterData
		return d, json.Unmarshal(data, &d)
	}
	r2 := registry(t, v2)
	v2.Migrate = func(context.Context, string, json.RawMessage) (counterData, error) {
		return counterData{}, errors.New("changed after registration")
	}
	out, err = activate(context.Background(), agent.NewController(s, r2), claim(t, s), "r2", 0)
	if err != nil || out.Status != store.StatusSucceeded {
		t.Fatalf("migrated outcome = %+v, %v", out, err)
	}
}

// A nil Init starts the run from D's zero value.
func TestNilInit(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createRun(t, s, "r1", `{"target":5}`)
	typ := counterType()
	typ.Init = nil
	var got []counterData
	typ.States[0].Code = func(ctx context.Context, env *agent.Env, d counterData) (agent.Transition[counterData], error) {
		got = append(got, d)
		return countStep(ctx, env, d)
	}
	out, err := activate(context.Background(), agent.NewController(s, registry(t, typ)), claim(t, s), "r1", 0)
	if err != nil || out.Status != store.StatusSucceeded || !slices.Equal(got, []counterData{{}}) {
		t.Fatalf("outcome = %+v, %v; state saw %+v", out, err, got)
	}
}

// An activation whose context has already ended runs neither Init nor
// Migrate: the cause decides at once (ADR 0003, item 17).
func TestEndedContextSkipsInitAndMigrate(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		calls := 0
		v1 := counterType()
		init := v1.Init
		v1.Init = func(ctx context.Context, env *agent.Env) (counterData, error) {
			calls++
			return init(ctx, env)
		}
		v2 := counterType()
		v2.Version = "2"
		v2.Migrate = func(_ context.Context, _ string, data json.RawMessage) (counterData, error) {
			calls++
			var d counterData
			return d, json.Unmarshal(data, &d)
		}
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(agent.ErrSuperseded)

		// Init: cancelled, with the run's start on the record.
		createRun(t, s, "r1", `{"target":3}`)
		out, err := activate(ctx, agent.NewController(s, registry(t, v1)), claim(t, s), "r1", 0)
		if err != nil || out.Status != store.StatusCancelled || calls != 0 {
			t.Fatalf("outcome = %+v, %v; %d calls", out, err, calls)
		}
		if got := kinds(entries(t, s, "r1")); !slices.Equal(got, []string{"activation_started", "run_started", "outcome"}) {
			t.Fatalf("entries = %v", got)
		}

		// Migrate: a checkpoint from version 1, resumed by version 2.
		createRun(t, s, "r2", `{"target":3}`)
		if _, err = activate(context.Background(), agent.NewController(s, registry(t, v1)), claim(t, s), "r2", 1); err != nil {
			t.Fatal(err)
		}
		calls = 0
		out, err = activate(ctx, agent.NewController(s, registry(t, v2)), claim(t, s), "r2", 0)
		if err != nil || out.Status != store.StatusCancelled || calls != 0 {
			t.Fatalf("outcome = %+v, %v; %d calls", out, err, calls)
		}
	})
}

func TestMigrateFailure(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		ctx := context.Background()
		createRun(t, s, "r1", `{"target":3}`)
		if _, err := activate(ctx, agent.NewController(s, registry(t, counterType())), claim(t, s), "r1", 1); err != nil {
			t.Fatal(err)
		}
		v2 := counterType()
		v2.Version = "2"
		v2.Migrate = func(context.Context, string, json.RawMessage) (counterData, error) {
			return counterData{}, errors.New("unknown field layout")
		}
		out, err := activate(ctx, agent.NewController(s, registry(t, v2)), claim(t, s), "r1", 0)
		if err != nil || out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonStateError ||
			!strings.Contains(out.Detail, `migrate from version "1": unknown field layout`) {
			t.Fatalf("outcome = %+v, %v", out, err)
		}
	})
}

// gapStore hides one entry from the record, as a damaged store would.
type gapStore struct {
	store.Store
	drop uint64
}

func (g gapStore) Entries(ctx context.Context, run string, after uint64) ([]store.Entry, error) {
	es, err := g.Store.Entries(ctx, run, after)
	return slices.DeleteFunc(es, func(e store.Entry) bool { return e.Seq == g.drop }), err
}

// A record this version cannot recover is refused before anything is
// written: an entry kind it does not know past the checkpoint, a checkpoint
// in another format or of another agent type. A record with a gap is a
// store failure.
func TestUnrecoverableRecords(t *testing.T) {
	ckState := func(agentType string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{
			"agent_type": agentType, "agent_version": "1", "state": "count", "data": map[string]int{"target": 3, "n": 1}, "step": 2,
		})
		return raw
	}
	cases := []struct {
		damage func(t *testing.T, s store.Store, f store.Fence, last store.Entry, ck store.Checkpoint) store.Store
		want   error
		name   string
		detail string
	}{
		{name: "unknown entry kind past the checkpoint", want: agent.ErrNotActivatable, detail: "a kind this version cannot recover",
			damage: func(t *testing.T, s store.Store, f store.Fence, last store.Entry, _ store.Checkpoint) store.Store {
				t.Helper()
				commitOps(t, s, f, store.AppendEntryOp(store.Entry{Run: "r1", Seq: last.Seq + 1, Step: 2, Kind: "subagent_started", Payload: json.RawMessage(`{}`)}))
				return s
			}},
		{name: "model attempt in a code state", want: agent.ErrNotActivatable, detail: "not a model state",
			damage: func(t *testing.T, s store.Store, f store.Fence, last store.Entry, _ store.Checkpoint) store.Store {
				t.Helper()
				commitOps(t, s, f, store.AppendEntryOp(store.Entry{Run: "r1", Seq: last.Seq + 1, Step: 2, Kind: "model_attempt", Payload: json.RawMessage(`{"state":"count"}`)}))
				return s
			}},
		{name: "gap in the record", want: agent.ErrStoreFailure, detail: "where",
			damage: func(t *testing.T, s store.Store, f store.Fence, last store.Entry, ck store.Checkpoint) store.Store {
				t.Helper()
				// Two entries past the checkpoint, the first hidden: the
				// tail starts after a hole.
				commitOps(t, s, f, store.AppendEntryOp(store.Entry{Run: "r1", Seq: last.Seq + 1, Step: 2, Kind: agent.EntryOutcome, Payload: json.RawMessage(`{}`)}))
				return gapStore{Store: s, drop: ck.RecordSeq + 1}
			}},
		{name: "checkpoint format", want: agent.ErrNotActivatable, detail: "has format 2",
			damage: func(t *testing.T, s store.Store, f store.Fence, last store.Entry, ck store.Checkpoint) store.Store {
				t.Helper()
				commitOps(t, s, f, store.PutCheckpointOp(store.Checkpoint{
					Instance: inst, Scope: ck.Scope, Seq: ck.Seq + 1, RecordSeq: last.Seq, Format: 2, State: ckState("counter"),
				}))
				return s
			}},
		{name: "checkpoint of another agent type", want: agent.ErrNotActivatable, detail: `of agent type "summarizer"`,
			damage: func(t *testing.T, s store.Store, f store.Fence, last store.Entry, ck store.Checkpoint) store.Store {
				t.Helper()
				commitOps(t, s, f, store.PutCheckpointOp(store.Checkpoint{
					Instance: inst, Scope: ck.Scope, Seq: ck.Seq + 1, RecordSeq: last.Seq, Format: 1, State: ckState("summarizer"),
				}))
				return s
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, b backend) {
				s := b.open()
				defer s.Close()
				ctx := context.Background()
				createRun(t, s, "r1", `{"target":3}`)
				reg := registry(t, counterType())
				if _, err := activate(ctx, agent.NewController(s, reg), claim(t, s), "r1", 1); err != nil {
					t.Fatal(err)
				}
				// The tail past the checkpoint is the step-limit outcome.
				es := entries(t, s, "r1")
				ck, ok, err := s.LatestCheckpoint(ctx, inst)
				if err != nil || !ok || ck.RecordSeq >= es[len(es)-1].Seq {
					t.Fatalf("checkpoint = %+v, %v, %v; record ends at %d", ck, ok, err, es[len(es)-1].Seq)
				}
				f := claim(t, s)
				damaged := tc.damage(t, s, f, es[len(es)-1], ck)
				before := len(entries(t, s, "r1"))
				_, err = activate(ctx, agent.NewController(damaged, reg), f, "r1", 0)
				if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.detail) {
					t.Fatalf("Activate = %v, want %v with %q", err, tc.want, tc.detail)
				}
				if after := len(entries(t, s, "r1")); after != before {
					t.Fatalf("entries grew from %d to %d", before, after)
				}
			})
		})
	}
}

func commitOps(t *testing.T, s store.Store, f store.Fence, ops ...store.Op) {
	t.Helper()
	if err := s.Commit(context.Background(), f, ops); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// failingLister fails listings, as a broken store would.
type failingLister struct{ store.Store }

func (failingLister) ListObjects(context.Context, string, string) ([]store.ObjectInfo, error) {
	return nil, errDisk
}

func TestEnvListObjects(t *testing.T) {
	listing := func(got *[]string) *agent.Type[counterData] {
		typ := counterType()
		typ.States[0].Code = func(ctx context.Context, env *agent.Env, d counterData) (agent.Transition[counterData], error) {
			infos, err := env.ListObjects(ctx, "a/")
			if err != nil {
				return agent.Transition[counterData]{}, err
			}
			for _, info := range infos {
				*got = append(*got, info.Key)
			}
			return agent.Goto("done", d), nil
		}
		return typ
	}
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		ctx := context.Background()
		createRun(t, s, "r1", `{"target":1}`)
		f := claim(t, s)
		commitOps(t, s, f,
			store.PutObjectJSON(ns, "a/1", 0, json.RawMessage(`1`)),
			store.PutObjectJSON(ns, "a/2", 0, json.RawMessage(`2`)),
			store.PutObjectJSON(ns, "b/1", 0, json.RawMessage(`3`)))
		var got []string
		out, err := activate(ctx, agent.NewController(s, registry(t, listing(&got))), f, "r1", 0)
		if err != nil || out.Status != store.StatusSucceeded || !slices.Equal(got, []string{"a/1", "a/2"}) {
			t.Fatalf("outcome = %+v, %v; listed %q", out, err, got)
		}

		// A malformed read (an invalid key) is the state's defect, not a
		// store failure (ADR 0007, item 13).
		createRun(t, s, "r2", `{"target":1}`)
		bad := counterType()
		bad.States[0].Code = func(ctx context.Context, env *agent.Env, d counterData) (agent.Transition[counterData], error) {
			if _, _, rerr := env.Object(ctx, ""); rerr != nil {
				return agent.Transition[counterData]{}, rerr
			}
			return agent.Goto("done", d), nil
		}
		out, err = activate(ctx, agent.NewController(s, registry(t, bad)), claim(t, s), "r2", 0)
		if err != nil || out.Reason.Code != agent.ReasonStateError || !strings.Contains(out.Detail, "invalid request") {
			t.Fatalf("outcome = %+v, %v; want state_error for the invalid key", out, err)
		}
		// A listing failure is a store failure, not a run failure.
		createRun(t, s, "r3", `{"target":1}`)
		_, err = activate(ctx, agent.NewController(failingLister{s}, registry(t, listing(&got))), claim(t, s), "r3", 0)
		if !errors.Is(err, agent.ErrStoreFailure) || !errors.Is(err, errDisk) {
			t.Fatalf("Activate = %v, want a store failure wrapping the listing error", err)
		}
		if r := runRecord(t, s, "r3"); r.Status != store.StatusRunnable {
			t.Fatalf("run status = %s, want runnable", r.Status)
		}
	})
}
