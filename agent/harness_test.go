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

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"

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
	put := &putArtifacts{by: map[string]map[string]bool{}}
	if name == "memstore" {
		mem := &refChecked{Store: memstore.New(), t: t, put: put}
		return backend{open: func() store.Store { return mem }}
	}
	dir := t.TempDir()
	return backend{durable: true, open: func() store.Store {
		s, err := filestore.Open(dir)
		if err != nil {
			t.Fatalf("filestore.Open: %v", err)
		}
		return &refChecked{Store: s, t: t, put: put}
	}}
}

// putArtifacts records, per instance, the digest of every artifact a
// commit fenced on it stored.
type putArtifacts struct {
	by map[string]map[string]bool
	mu sync.Mutex
}

// refChecked checks, after every commit it passes on that lands, that
// agent.ArtifactReferences names every artifact the commit's instance has
// stored so far (ADR 0024, item 9): the commit that first references an
// artifact stores it (ADR 0004, item 1), so the references must cover
// them at every commit. Every runtime test that opens its store through
// newBackend checks the migration's completeness this way, so an
// artifact-writing path the migration does not read fails a test.
type refChecked struct {
	store.Store
	t   *testing.T
	put *putArtifacts
}

func (s *refChecked) Commit(ctx context.Context, f store.Fence, ops []store.Op) error {
	if err := s.Store.Commit(ctx, f, ops); err != nil {
		return err //nolint:wrapcheck // passed through unchanged
	}
	s.put.mu.Lock()
	defer s.put.mu.Unlock()
	for _, op := range ops {
		if op.Kind == store.OpPutArtifact {
			if s.put.by[f.Instance] == nil {
				s.put.by[f.Instance] = map[string]bool{}
			}
			s.put.by[f.Instance][op.PutArtifact.Digest] = true
		}
	}
	refs, err := agent.ArtifactReferences(context.WithoutCancel(ctx), s.Store, f.Instance)
	if err != nil {
		s.t.Errorf("ArtifactReferences(%s) after a commit: %v", f.Instance, err)
		return nil
	}
	named := map[string]bool{}
	for _, d := range refs {
		named[d] = true
	}
	for d := range s.put.by[f.Instance] {
		if !named[d] {
			s.t.Errorf("instance %s stored artifact %s, which ArtifactReferences does not name", f.Instance, d)
		}
	}
	return nil
}

// unchecked returns the store under a refChecked one, for a test that
// commits a record ArtifactReferences refuses on purpose.
func unchecked(s store.Store) store.Store {
	if r, ok := s.(*refChecked); ok {
		return r.Store
	}
	return s
}

func (s *refChecked) AppendEvents(ctx context.Context, events []store.Event) error {
	return s.Store.(store.Telemetry).AppendEvents(ctx, events) //nolint:forcetypeassert,wrapcheck // both reference stores implement it
}

func (s *refChecked) Events(ctx context.Context, run string, after uint64) ([]store.Event, error) {
	return s.Store.(store.Telemetry).Events(ctx, run, after) //nolint:forcetypeassert,wrapcheck // both reference stores implement it
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
	obj, ok, err := env.Object(ctx, env.RunID())
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
	op := env.PutJSON(env.RunID(), obj.Version, json.RawMessage(strconv.Itoa(d.N)))
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
		Fence: f, Scope: store.Scope{Kind: store.ScopeRun, ID: run}, Namespace: ns, MaxSteps: maxSteps,
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

// modelClient wraps a test client in a ModelClient with a single attempt per
// call, so a scripted failure fails the call; nil stays nil.
func modelClient(c llms.ChatClient) *agent.ModelClient {
	if c == nil {
		return nil
	}
	mc, err := agent.NewModelClient(c, agent.ModelClientConfig{Retry: &middleware.RetryConfig{MaxAttempts: 1}})
	if err != nil {
		panic(err)
	}
	return mc
}
