package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// activateIn activates run with the given namespace.
func activateIn(ctx context.Context, c *agent.Controller, f store.Fence, run, namespace string, maxSteps int) (agent.Outcome, error) {
	return c.Activate(ctx, agent.ActivationInput{
		Fence: f, Scope: store.Scope{Kind: store.ScopeRun, ID: run}, Namespace: namespace, MaxSteps: maxSteps,
	})
}

func runStarted(t *testing.T, s store.Store, run string) agent.RunStarted {
	t.Helper()
	for _, e := range entries(t, s, run) {
		if e.Kind == agent.EntryRunStarted {
			return payload[agent.RunStarted](t, e)
		}
	}
	t.Fatalf("run %s has no run_started entry", run)
	return agent.RunStarted{}
}

// A run needs a namespace the store accepts; without one it fails with
// config_error before Init runs, and its start is on the record (REC-1).
func TestNamespaceRequired(t *testing.T) {
	cases := map[string]string{
		"missing":       "",
		"invalid UTF-8": "ns\xff",
		// The reference stores refuse a namespace this long (their limit
		// is on its encoded length); the run's start finds out before
		// agent code does.
		"refused by the store": strings.Repeat("n", 300),
	}
	for name, namespace := range cases {
		t.Run(name, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, b backend) {
				s := b.open()
				defer s.Close()
				createRun(t, s, "r1", `{"target":1}`)
				typ := counterType()
				inits := 0
				typ.Init = func(context.Context, *agent.Env) (counterData, error) {
					inits++
					return counterData{Target: 1}, nil
				}
				out, err := activateIn(context.Background(), agent.NewController(s, registry(t, typ)), claim(t, s), "r1", namespace, 0)
				if err != nil || out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonConfigError {
					t.Fatalf("outcome = %+v, %v; want failed with config_error", out, err)
				}
				if inits != 0 {
					t.Fatalf("Init ran %d times", inits)
				}
				if !strings.Contains(out.Detail, "namespace") {
					t.Fatalf("detail = %q, want it to name the namespace", out.Detail)
				}
				// The start is recorded with the namespace as passed, or
				// with none where the record cannot hold it as passed.
				want := namespace
				if name == "invalid UTF-8" {
					want = ""
				}
				if got := runStarted(t, s, "r1").Namespace; got != want {
					t.Fatalf("run_started namespace = %q, want %q", got, want)
				}
				wantReleased(t, s)
			})
		})
	}
}

// probeFailure fails the run start's namespace probe, as a broken store
// would.
type probeFailure struct{ store.Store }

func (p probeFailure) Object(ctx context.Context, ns, key string) (store.Object, bool, error) {
	if key == probeKey {
		return store.Object{}, false, errDisk
	}
	return p.Store.Object(ctx, ns, key)
}

// A store failure while checking the namespace is the store's, not the
// run's: the activation ends with an error and the run stays runnable.
func TestNamespaceProbeStoreFailure(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createRun(t, s, "r1", `{"target":1}`)
	_, err := activate(context.Background(), agent.NewController(probeFailure{s}, registry(t, counterType())), claim(t, s), "r1", 0)
	if !errors.Is(err, agent.ErrStoreFailure) || !errors.Is(err, errDisk) {
		t.Fatalf("Activate = %v, want a store failure wrapping the probe's", err)
	}
	if r := runRecord(t, s, "r1"); r.Status != store.StatusRunnable {
		t.Fatalf("run status = %s, want runnable", r.Status)
	}
}

// The namespace is recorded with the run's start and fixed for the run: a
// later activation's namespace is ignored.
func TestNamespaceFixedForTheRun(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		ctx := context.Background()
		createRun(t, s, "r1", `{"target":3}`)
		reg := registry(t, counterType())
		out, err := activateIn(ctx, agent.NewController(s, reg), claim(t, s), "r1", "first", 1)
		if err != nil || out.Status != store.StatusRunnable {
			t.Fatalf("first activation = %+v, %v", out, err)
		}
		out, err = activateIn(ctx, agent.NewController(s, reg), claim(t, s), "r1", "second", 0)
		if err != nil || out.Status != store.StatusSucceeded {
			t.Fatalf("second activation = %+v, %v", out, err)
		}
		if got := runStarted(t, s, "r1").Namespace; got != "first" {
			t.Fatalf("run_started namespace = %q, want first", got)
		}
		obj, ok, err := s.Object(ctx, "first", "r1")
		if err != nil || !ok || string(obj.JSON) != "3" {
			t.Fatalf("count in first = %s, %v, %v; want 3", obj.JSON, ok, err)
		}
		if infos, err := s.ListObjects(ctx, "second", ""); err != nil || len(infos) != 0 {
			t.Fatalf("second namespace holds %v, %v; want nothing", infos, err)
		}
		// Even a host that passes none later keeps the recorded one.
		createRun(t, s, "r2", `{"target":2}`)
		if out, err := activateIn(ctx, agent.NewController(s, reg), claim(t, s), "r2", "first", 1); err != nil || out.Status != store.StatusRunnable {
			t.Fatalf("r2 first activation = %+v, %v", out, err)
		}
		if out, err := activateIn(ctx, agent.NewController(s, reg), claim(t, s), "r2", "", 0); err != nil || out.Status != store.StatusSucceeded {
			t.Fatalf("r2 second activation = %+v, %v", out, err)
		}
	})
}

// Runs given the same namespace share its objects; a run given another
// sees none of them (STORE-2).
func TestNamespaceSharingAndIsolation(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		ctx := context.Background()
		reg := registry(t, counterType())
		createRun(t, s, "r1", `{"target":1}`)
		if out, err := activateIn(ctx, agent.NewController(s, reg), claim(t, s), "r1", "shared", 0); err != nil || out.Status != store.StatusSucceeded {
			t.Fatalf("r1 = %+v, %v", out, err)
		}
		seen := func(namespace string) []string {
			typ := counterType()
			var keys []string
			typ.States[0].Code = func(ctx context.Context, env *agent.Env, d counterData) (agent.Transition[counterData], error) {
				infos, err := env.ListObjects(ctx, "")
				if err != nil {
					return agent.Transition[counterData]{}, err
				}
				for _, info := range infos {
					keys = append(keys, info.Key)
				}
				if _, ok, err := env.Object(ctx, "r1"); err != nil || ok != (len(keys) > 0) {
					return agent.Transition[counterData]{}, errors.New("Object disagrees with ListObjects")
				}
				return agent.Goto("done", d), nil
			}
			run := "r-" + namespace
			createRun(t, s, run, `{"target":1}`)
			if out, err := activateIn(context.Background(), agent.NewController(s, registry(t, typ)), claim(t, s), run, namespace, 0); err != nil || out.Status != store.StatusSucceeded {
				t.Fatalf("%s = %+v, %v", run, out, err)
			}
			return keys
		}
		if got := seen("shared"); len(got) != 1 || got[0] != "r1" {
			t.Fatalf("a run sharing the namespace sees %q, want [r1]", got)
		}
		if got := seen("other"); len(got) != 0 {
			t.Fatalf("a run in another namespace sees %q, want nothing", got)
		}
	})
}

// A code state's operation outside the run's namespace fails the run with
// state_error, and nothing it carried lands.
func TestNamespaceCodeStateOpOutside(t *testing.T) {
	ops := map[string]func(*agent.Env) store.Op{
		"put":   func(*agent.Env) store.Op { return store.PutObjectJSON("elsewhere", "k", 0, json.RawMessage(`1`)) },
		"bytes": func(*agent.Env) store.Op { return store.PutObjectBytes("elsewhere", "k", 0, []byte("x")) },
		"delete": func(*agent.Env) store.Op {
			return store.DeleteObjectOp("elsewhere", "k", 1)
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, b backend) {
				s := b.open()
				defer s.Close()
				ctx := context.Background()
				// The delete has an object to delete, so only the
				// namespace check can refuse it.
				commitOps(t, s, claim(t, s), store.PutObjectJSON("elsewhere", "k", 0, json.RawMessage(`0`)))
				if err := s.Release(ctx, ownershipFence(t, s)); err != nil {
					t.Fatal(err)
				}
				createRun(t, s, "r1", `{"target":1}`)
				typ := counterType()
				typ.States[0].Code = func(_ context.Context, env *agent.Env, d counterData) (agent.Transition[counterData], error) {
					return agent.Goto("done", d, env.PutJSON("mine", 0, json.RawMessage(`1`)), op(env)), nil
				}
				out, err := activate(ctx, agent.NewController(s, registry(t, typ)), claim(t, s), "r1", 0)
				if err != nil || out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonStateError {
					t.Fatalf("outcome = %+v, %v; want failed with state_error", out, err)
				}
				if !strings.Contains(out.Detail, `"elsewhere"`) {
					t.Fatalf("detail = %q, want it to name the namespace", out.Detail)
				}
				if obj, _, _ := s.Object(ctx, "elsewhere", "k"); obj.Version != 1 || string(obj.JSON) != "0" {
					t.Fatalf("elsewhere/k = %+v, want untouched", obj)
				}
				if _, ok, _ := s.Object(ctx, ns, "mine"); ok {
					t.Fatal("the state's in-namespace operation landed")
				}
			})
		})
	}
}

// A tool's operation outside the run's namespace fails the run with
// tool_error, and nothing it carried lands.
func TestNamespaceToolOpOutside(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		s := b.open()
		defer s.Close()
		ctx := context.Background()
		tools := ledgerTools(&execLog{})
		tools[1].Execute = func(_ context.Context, c *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
			return agent.Result[ledgerData]{Content: "recorded", Ops: []store.Op{
				c.Env.PutJSON(c.Env.RunID(), 0, json.RawMessage(`["mine"]`)),
				store.PutObjectJSON("elsewhere", "k", 0, json.RawMessage(`1`)),
			}}, nil
		}
		createLedgerRun(t, s)
		script := []llms.ChatResponse{callsResp(tc("c1", "record", `{"item":"a"}`))}
		out, err := activateWith(ctx, agent.NewController(s, registry(t, ledgerType(&execLog{}, tools...))), claim(t, s), scripted(script), 0)
		if err != nil || out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonToolError {
			t.Fatalf("outcome = %+v, %v; want failed with tool_error", out, err)
		}
		if !strings.Contains(out.Detail, `"elsewhere"`) {
			t.Fatalf("detail = %q, want it to name the namespace", out.Detail)
		}
		if _, ok, _ := s.Object(ctx, "elsewhere", "k"); ok {
			t.Fatal("the out-of-namespace operation landed")
		}
		if items := ledgerItems(t, s); len(items) != 0 {
			t.Fatalf("ledger = %q, want nothing landed", items)
		}
	})
}

// A checkpoint that records no namespace was not written by this runtime;
// recovery refuses it rather than guess one.
func TestNamespaceCheckpointWithoutOne(t *testing.T) {
	s := newBackend(t, "memstore").open()
	ctx := context.Background()
	createRun(t, s, "r1", `{"target":2}`)
	reg := registry(t, counterType())
	if out, err := activate(ctx, agent.NewController(s, reg), claim(t, s), "r1", 1); err != nil || out.Status != store.StatusRunnable {
		t.Fatalf("first activation = %+v, %v", out, err)
	}
	f := claim(t, s)
	latest, ok, err := s.LatestCheckpoint(ctx, inst)
	if err != nil || !ok {
		t.Fatalf("LatestCheckpoint = %v, %v", ok, err)
	}
	var state map[string]json.RawMessage
	if uerr := json.Unmarshal(latest.State, &state); uerr != nil {
		t.Fatal(uerr)
	}
	delete(state, "namespace")
	latest.State, _ = json.Marshal(state)
	latest.Seq++
	commitOps(t, s, f, store.PutCheckpointOp(latest))
	_, err = activate(ctx, agent.NewController(s, reg), f, "r1", 0)
	if !errors.Is(err, agent.ErrNotActivatable) || !strings.Contains(err.Error(), "namespace") {
		t.Fatalf("Activate = %v, want not activatable for the missing namespace", err)
	}
}
