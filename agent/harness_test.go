package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/filestore"
	"github.com/SnapdragonPartners/maestro-agents/store/memstore"
)

const (
	ns   = "counters"
	inst = "inst-1"
)

// backend is a store under test that can be reopened, as a process
// restart would.
type backend struct {
	open    func() store.Store
	durable bool
}

func newBackend(t *testing.T, name string) backend {
	t.Helper()
	if name == "memstore" {
		mem := memstore.New()
		return backend{open: func() store.Store { return mem }}
	}
	dir := t.TempDir()
	return backend{durable: true, open: func() store.Store {
		s, err := filestore.Open(dir)
		if err != nil {
			t.Fatalf("filestore.Open: %v", err)
		}
		return s
	}}
}

func forEachBackend(t *testing.T, fn func(t *testing.T, b backend)) {
	t.Helper()
	for _, name := range []string{"memstore", "filestore"} {
		t.Run(name, func(t *testing.T) { fn(t, newBackend(t, name)) })
	}
}

// counterData is the counter agent's state data.
type counterData struct {
	Target int `json:"target"`
	N      int `json:"n"`
}

// counterType counts to the trigger's target, one step per count, writing
// the count to an object keyed by run ID with a version precondition, so a
// duplicated step would be visible.
func counterType() *agent.Type[counterData] {
	return &agent.Type[counterData]{
		Name:     "counter",
		Version:  "1",
		Triggers: []string{agent.TriggerOneShot},
		Initial:  "count",
		Init: func(_ context.Context, env *agent.Env) (counterData, error) {
			var d counterData
			err := json.Unmarshal(env.Trigger(), &d)
			return d, err
		},
		States: []agent.State[counterData]{
			{Name: "count", Next: []string{"count", "done"}, Code: countStep},
			{Name: "done", End: agent.EndChanged},
		},
	}
}

func countStep(ctx context.Context, env *agent.Env, d counterData) (agent.Transition[counterData], error) {
	obj, ok, err := env.Object(ctx, ns, env.RunID())
	if err != nil {
		return agent.Transition[counterData]{}, err
	}
	stored := 0
	if ok {
		if err := json.Unmarshal(obj.JSON, &stored); err != nil {
			return agent.Transition[counterData]{}, err
		}
	}
	if stored != d.N {
		return agent.Transition[counterData]{}, fmt.Errorf("stored count %d, state data says %d", stored, d.N)
	}
	d.N++
	op := store.PutObjectJSON(ns, env.RunID(), obj.Version, json.RawMessage(strconv.Itoa(d.N)))
	if d.N >= d.Target {
		return agent.Goto("done", d, op), nil
	}
	return agent.Goto("count", d, op), nil
}

func registry(t testing.TB, types ...agent.Definition) *agent.Registry {
	t.Helper()
	r := agent.NewRegistry()
	for _, d := range types {
		if err := r.Register(d); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	return r
}

func createRun(t testing.TB, s store.Store, id, trigger string) {
	t.Helper()
	_, _, err := s.CreateRun(context.Background(), store.Run{
		ID: id, Instance: inst, AgentType: "counter", TriggerID: "trig-" + id, TriggerKind: agent.TriggerOneShot, Trigger: json.RawMessage(trigger),
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
}

func claim(t testing.TB, s store.Store) store.Fence {
	t.Helper()
	own, err := s.Claim(context.Background(), inst, "host", time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return own.Fence()
}

func activate(ctx context.Context, c *agent.Controller, f store.Fence, run string, maxSteps int) (agent.Outcome, error) {
	return c.Activate(ctx, agent.ActivationInput{
		Fence: f, Scope: store.Scope{Kind: store.ScopeRun, ID: run}, MaxSteps: maxSteps,
	})
}

func entries(t testing.TB, s store.Store, run string) []store.Entry {
	t.Helper()
	es, err := s.Entries(context.Background(), run, 0)
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	return es
}

func kinds(es []store.Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Kind
	}
	return out
}

func count(t testing.TB, s store.Store, run string) int {
	t.Helper()
	obj, ok, err := s.Object(context.Background(), ns, run)
	if err != nil {
		t.Fatalf("Object: %v", err)
	}
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(string(obj.JSON))
	if err != nil {
		t.Fatalf("count %q: %v", obj.JSON, err)
	}
	return n
}

func runRecord(t testing.TB, s store.Store, run string) store.Run {
	t.Helper()
	r, err := s.Run(context.Background(), run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return r
}

func wantReleased(t testing.TB, s store.Store) {
	t.Helper()
	own, err := s.Ownership(context.Background(), inst)
	if err != nil || own.Owner != "" {
		t.Fatalf("ownership = %+v, %v; want released", own, err)
	}
}

var errCrash = errors.New("injected crash")

// crashStore fails one commit, the at'th (from 1), as a process dying
// there would: before it lands, or after it has landed but before its
// result is returned. Either way the caller sees an indeterminate error.
type crashStore struct {
	store.Store
	mu      sync.Mutex
	n, at   int
	landing bool
}

func (c *crashStore) Commit(ctx context.Context, f store.Fence, ops []store.Op) error {
	c.mu.Lock()
	c.n++
	hit := c.n == c.at
	c.mu.Unlock()
	if !hit {
		return c.Store.Commit(ctx, f, ops)
	}
	if c.landing {
		if err := c.Store.Commit(ctx, f, ops); err != nil {
			return err
		}
	}
	return errCrash
}

// countingStore counts commits.
type countingStore struct {
	store.Store
	n int
}

func (c *countingStore) Commit(ctx context.Context, f store.Fence, ops []store.Op) error {
	c.n++
	return c.Store.Commit(ctx, f, ops)
}
