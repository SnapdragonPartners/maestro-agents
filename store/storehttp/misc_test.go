package storehttp_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/memstore"
	"github.com/SnapdragonPartners/maestro-agents/store/storehttp"
	"github.com/SnapdragonPartners/maestro-agents/store/storetest"
)

// refuseAll refuses every call.
var refuseAll = storehttp.AuthorizerFunc(func(context.Context, storehttp.Call) error { return errors.New("no") })

// versioned is a store that reports a schema version.
type versioned struct{ store.Store }

func (versioned) SchemaVersion() int { return 3 }

// Healthz answers without authorization: the version, the schema version
// when the store reports one, and whether the store answers (ADR 0024,
// item 9).
func TestHealth(t *testing.T) {
	ctx := context.Background()
	c := serve(t, memstore.New(), storehttp.HandlerOptions{Authorizer: refuseAll, Version: "v9.9.9"})
	h, err := c.Health(ctx)
	if err != nil || h.Version != "v9.9.9" || h.Store != "ok" || h.SchemaVersion != nil {
		t.Fatalf("Health = %+v, %v", h, err)
	}
	c = serve(t, versioned{memstore.New()}, storehttp.HandlerOptions{Authorizer: refuseAll})
	if h, err = c.Health(ctx); err != nil || h.SchemaVersion == nil || *h.SchemaVersion != 3 || h.Version == "" {
		t.Fatalf("Health = %+v, %v", h, err)
	}
	closed := memstore.New()
	must(t, closed.Close())
	c = serve(t, closed, storehttp.HandlerOptions{Authorizer: refuseAll})
	if h, err = c.Health(ctx); err != nil || h.Store != "unavailable" {
		t.Fatalf("Health of a closed store = %+v, %v", h, err)
	}
	// Every other route is authorized.
	if _, err = c.Ownership(ctx, "i"); !errors.Is(err, storehttp.ErrForbidden) {
		t.Fatalf("Ownership = %v, want ErrForbidden", err)
	}
}

// The status routes answer what agent.Status and agent.InstanceStatus
// derive from the store behind them.
func TestStatusRoutes(t *testing.T) {
	clk := storetest.NewClock()
	inner := memstore.New(memstore.WithClock(clk.Now))
	ctx := context.Background()
	_, _, err := inner.CreateRun(ctx, newRun("run", "inst"))
	must(t, err)
	_, err = inner.EnqueueTrigger(ctx, store.Trigger{Instance: "inst", ID: "t2", Kind: "k"})
	must(t, err)
	_, _, err = inner.RequestCancel(ctx, "run", store.CancelOperator)
	must(t, err)
	c := serve(t, inner, storehttp.HandlerOptions{})
	same := func(what string, got, want any) {
		t.Helper()
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		if string(g) != string(w) {
			t.Fatalf("%s over the wire = %s, want %s", what, g, w)
		}
	}
	got, err := c.Status(ctx, "run")
	must(t, err)
	want, err := agent.Status(ctx, inner, "run")
	must(t, err)
	same("Status", got, want)
	gotI, err := c.InstanceStatus(ctx, "inst")
	must(t, err)
	wantI, err := agent.InstanceStatus(ctx, inner, "inst")
	must(t, err)
	same("InstanceStatus", gotI, wantI)
	if _, err = c.Status(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Status(missing) = %v, want ErrNotFound", err)
	}
	// agent.Status through the client agrees with the status route.
	viaClient, err := agent.Status(ctx, c, "run")
	must(t, err)
	same("agent.Status through the client", viaClient, want)
}

// Identifiers holding anything the store accepts travel as one path
// segment or a query value: slashes, dots, percent signs, spaces,
// non-ASCII, query and fragment characters.
func TestIdentifiersEscaped(t *testing.T) {
	inner := memstore.New()
	c := serve(t, inner, storehttp.HandlerOptions{})
	ctx := context.Background()
	for i, id := range []string{"a/b", "..", ".", "a b%2F", "ünï", "?x#y&z=1", "a//b/", "%"} {
		inst := id + "-inst"
		run, created, err := c.CreateRun(ctx, newRun(id, inst))
		if err != nil || !created || run.ID != id || run.Instance != inst {
			t.Fatalf("CreateRun(%q) = %+v, %v, %v", id, run, created, err)
		}
		if got, rerr := c.Run(ctx, id); rerr != nil || got.ID != id {
			t.Fatalf("Run(%q) = %+v, %v", id, got, rerr)
		}
		if runs, _, rerr := c.Runs(ctx, inst, store.RunQuery{After: id}); rerr != nil || len(runs) != 0 {
			t.Fatalf("Runs(%q, after itself) = %+v, %v", inst, runs, rerr)
		}
		own, err := c.Claim(ctx, inst, "owner/"+id, time.Minute)
		if err != nil || own.Instance != inst {
			t.Fatalf("Claim(%q) = %+v, %v", inst, own, err)
		}
		ns, key := "ns/"+id, "key/"+id
		must(t, c.Commit(ctx, own.Fence(), []store.Op{store.PutObjectJSON(ns, key, 0, json.RawMessage(`1`)), store.ReleaseOp()}))
		if obj, ok, oerr := c.Object(ctx, ns, key); oerr != nil || !ok || obj.Key != key || obj.Namespace != ns {
			t.Fatalf("Object(%q, %q) = %+v, %v, %v", ns, key, obj, ok, oerr)
		}
		if keys, lerr := c.ListObjects(ctx, ns, key[:4]); lerr != nil || len(keys) != 1 || keys[0].Key != key {
			t.Fatalf("ListObjects(%q) = %+v, %v (case %d)", ns, keys, lerr, i)
		}
		if _, err = c.EnqueueTrigger(ctx, store.Trigger{Instance: inst, ID: id, Kind: "k"}); err != nil {
			t.Fatalf("EnqueueTrigger(%q): %v", id, err)
		}
		must(t, c.RemoveTrigger(ctx, inst, id))
		if trig, err := c.Triggers(ctx, inst); err != nil || len(trig) != 0 {
			t.Fatalf("Triggers(%q) = %+v, %v", inst, trig, err)
		}
	}
}

// noTelemetry is a store that keeps no telemetry.
type noTelemetry struct{ store.Store }

// A store without telemetry answers telemetry calls with ErrInvalid.
func TestTelemetryUnsupported(t *testing.T) {
	c := serve(t, noTelemetry{memstore.New()}, storehttp.HandlerOptions{})
	ctx := context.Background()
	if err := c.AppendEvents(ctx, []store.Event{{Run: "r", Seq: 1, Kind: "k"}}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("AppendEvents = %v, want ErrInvalid", err)
	}
	if _, err := c.Events(ctx, "r", 0); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("Events = %v, want ErrInvalid", err)
	}
}

// A refused telemetry call answers the refusal, never the store's lack of
// telemetry: the Authorizer is asked before the capability is looked at.
func TestTelemetryRefusedFirst(t *testing.T) {
	c := serve(t, noTelemetry{memstore.New()}, storehttp.HandlerOptions{Authorizer: refuseAll})
	ctx := context.Background()
	if err := c.AppendEvents(ctx, []store.Event{{Run: "r", Seq: 1, Kind: "k"}}); !errors.Is(err, storehttp.ErrForbidden) {
		t.Fatalf("AppendEvents = %v, want ErrForbidden", err)
	}
	if _, err := c.Events(ctx, "r", 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Events = %v, want ErrNotFound", err)
	}
	// An empty batch is authorized store-wide like any call, and refused
	// alike whether or not the store keeps telemetry.
	var calls []storehttp.Call
	record := storehttp.AuthorizerFunc(func(_ context.Context, call storehttp.Call) error {
		calls = append(calls, call)
		return errors.New("no")
	})
	for _, inner := range []store.Store{noTelemetry{memstore.New()}, memstore.New()} {
		calls = nil
		c = serve(t, inner, storehttp.HandlerOptions{Authorizer: record})
		if err := c.AppendEvents(ctx, nil); !errors.Is(err, storehttp.ErrForbidden) {
			t.Fatalf("AppendEvents(nil) over %T = %v, want ErrForbidden", inner, err)
		}
		if len(calls) != 1 || calls[0].Op != storehttp.OpAppendEvents || !calls[0].StoreWide || calls[0].Instance != "" || calls[0].Run != "" {
			t.Fatalf("AppendEvents(nil) over %T asked %+v, want one store-wide call", inner, calls)
		}
	}
	// Allowed, it reaches the store, which answers it as it answers one
	// directly: a no-op on a live store, ErrUnusable on a closed one.
	live := memstore.New()
	c = serve(t, live, storehttp.HandlerOptions{})
	must(t, c.AppendEvents(ctx, nil))
	must(t, live.Close())
	if err := c.AppendEvents(ctx, nil); err == nil || store.IsDefinite(err) {
		t.Fatalf("AppendEvents(nil) over a closed store = %v, want an indeterminate error", err)
	}
}
