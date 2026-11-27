package storehttp_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/memstore"
	"github.com/SnapdragonPartners/maestro-agents/store/storehttp"
)

// recorder records every Call and decides it with allow.
type recorder struct {
	allow func(storehttp.Call) error
	calls []storehttp.Call
	mu    sync.Mutex
}

func (r *recorder) Authorize(_ context.Context, c storehttp.Call) error {
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
	if r.allow == nil {
		return nil
	}
	return r.allow(c)
}

func (r *recorder) take() []storehttp.Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.calls
	r.calls = nil
	return out
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newRun(id, inst string) store.Run {
	return store.Run{ID: id, Instance: inst, AgentType: "a", TriggerID: "t-" + id, TriggerKind: "k", Trigger: json.RawMessage(`{}`)}
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Every call names to the Authorizer exactly what it concerns (ADR 0026,
// item 4), field by field.
func TestCallsFieldByField(t *testing.T) {
	rec := &recorder{}
	c := serve(t, memstore.New(), storehttp.HandlerOptions{Authorizer: rec})
	ctx := context.Background()
	type step struct {
		do   func() error
		name string
		want []storehttp.Call
	}
	var fence store.Fence
	content := []byte("artifact")
	run := func(op storehttp.Operation, inst, run string) storehttp.Call {
		return storehttp.Call{Op: op, Instance: inst, Run: run}
	}
	inst := func(op storehttp.Operation, inst string) storehttp.Call {
		return storehttp.Call{Op: op, Instance: inst}
	}
	steps := []step{
		{name: "CreateRun", do: func() error { _, _, err := c.CreateRun(ctx, newRun("run-a", "inst-a")); return err },
			want: []storehttp.Call{run(storehttp.OpCreateRun, "inst-a", "run-a")}},
		{name: "Claim", do: func() error {
			own, err := c.Claim(ctx, "inst-a", "owner-a", time.Minute)
			fence = own.Fence()
			return err
		}, want: []storehttp.Call{{Op: storehttp.OpClaim, Instance: "inst-a", Owner: "owner-a"}}},
		{name: "Heartbeat", do: func() error { _, _, err := c.Heartbeat(ctx, fence, time.Minute); return err },
			want: []storehttp.Call{{Op: storehttp.OpHeartbeat, Instance: "inst-a", Fence: &fence}}},
		{name: "Commit", do: func() error {
			return c.Commit(ctx, fence, []store.Op{
				store.PutObjectJSON("ns-1", "k", 0, json.RawMessage(`1`)),
				store.PutObjectJSON("ns-2", "k", 0, json.RawMessage(`2`)),
				store.PutObjectBytes("ns-1", "k2", 0, []byte("x")),
				store.PutArtifactOp(store.Artifact{Digest: digest(content), MediaType: "text/plain", Content: content}),
			})
		}, want: []storehttp.Call{{Op: storehttp.OpCommit, Instance: "inst-a", Fence: &fence, Namespaces: []string{"ns-1", "ns-2"}}}},
		{name: "EnqueueTrigger", do: func() error {
			_, err := c.EnqueueTrigger(ctx, store.Trigger{Instance: "inst-a", ID: "t2", Kind: "k"})
			return err
		}, want: []storehttp.Call{inst(storehttp.OpEnqueueTrigger, "inst-a")}},
		{name: "Triggers", do: func() error { _, err := c.Triggers(ctx, "inst-a"); return err },
			want: []storehttp.Call{inst(storehttp.OpTriggers, "inst-a")}},
		{name: "RemoveTrigger", do: func() error { return c.RemoveTrigger(ctx, "inst-a", "t2") },
			want: []storehttp.Call{inst(storehttp.OpRemoveTrigger, "inst-a")}},
		{name: "Ownership", do: func() error { _, err := c.Ownership(ctx, "inst-a"); return err },
			want: []storehttp.Call{inst(storehttp.OpOwnership, "inst-a")}},
		{name: "ActiveRun", do: func() error { _, _, err := c.ActiveRun(ctx, "inst-a"); return err },
			want: []storehttp.Call{inst(storehttp.OpActiveRun, "inst-a")}},
		{name: "LatestCheckpoint", do: func() error { _, _, err := c.LatestCheckpoint(ctx, "inst-a"); return err },
			want: []storehttp.Call{inst(storehttp.OpLatestCheckpoint, "inst-a")}},
		{name: "Checkpoint", do: func() error {
			if _, err := c.Checkpoint(ctx, "inst-a", 1); !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("Checkpoint = %w, want ErrNotFound", err)
			}
			return nil
		}, want: []storehttp.Call{inst(storehttp.OpCheckpoint, "inst-a")}},
		{name: "Artifact", do: func() error { _, _, err := c.Artifact(ctx, "inst-a", digest(content)); return err },
			want: []storehttp.Call{inst(storehttp.OpArtifact, "inst-a")}},
		{name: "InstanceStatus", do: func() error { _, err := c.InstanceStatus(ctx, "inst-a"); return err },
			want: []storehttp.Call{inst(storehttp.OpInstanceStatus, "inst-a")}},
		{name: "Runs with a cursor", do: func() error { _, _, err := c.Runs(ctx, "inst-a", store.RunQuery{After: "run-a"}); return err },
			want: []storehttp.Call{inst(storehttp.OpRuns, "inst-a"), run(storehttp.OpRuns, "inst-a", "run-a")}},
		{name: "Runs", do: func() error { _, _, err := c.Runs(ctx, "inst-a", store.RunQuery{Newest: true}); return err },
			want: []storehttp.Call{inst(storehttp.OpRuns, "inst-a")}},
		{name: "Run", do: func() error { _, err := c.Run(ctx, "run-a"); return err },
			want: []storehttp.Call{run(storehttp.OpRun, "inst-a", "run-a")}},
		{name: "Entries", do: func() error { _, err := c.Entries(ctx, "run-a", 0); return err },
			want: []storehttp.Call{run(storehttp.OpEntries, "inst-a", "run-a")}},
		{name: "CancelRequest", do: func() error { _, _, err := c.CancelRequest(ctx, "run-a"); return err },
			want: []storehttp.Call{run(storehttp.OpCancelRequest, "inst-a", "run-a")}},
		{name: "RequestCancel", do: func() error { _, _, err := c.RequestCancel(ctx, "run-a", store.CancelOperator); return err },
			want: []storehttp.Call{run(storehttp.OpRequestCancel, "inst-a", "run-a")}},
		{name: "Status", do: func() error { _, err := c.Status(ctx, "run-a"); return err },
			want: []storehttp.Call{run(storehttp.OpStatus, "inst-a", "run-a")}},
		{name: "Object", do: func() error { _, _, err := c.Object(ctx, "ns-1", "k"); return err },
			want: []storehttp.Call{{Op: storehttp.OpObject, Namespaces: []string{"ns-1"}}}},
		{name: "ListObjects", do: func() error { _, err := c.ListObjects(ctx, "ns-2", ""); return err },
			want: []storehttp.Call{{Op: storehttp.OpListObjects, Namespaces: []string{"ns-2"}}}},
		{name: "Claimable", do: func() error { _, err := c.Claimable(ctx); return err },
			want: []storehttp.Call{{Op: storehttp.OpClaimable, StoreWide: true}}},
		{name: "AppendEvents", do: func() error {
			at := time.Now().UTC()
			return c.AppendEvents(ctx, []store.Event{
				{Run: "run-a", Seq: 1, Kind: "k", At: at}, {Run: "ghost", Seq: 1, Kind: "k", At: at}, {Run: "run-a", Seq: 2, Kind: "k", At: at},
			})
		}, want: []storehttp.Call{
			run(storehttp.OpAppendEvents, "inst-a", "run-a"),
			{Op: storehttp.OpAppendEvents, Run: "ghost", StoreWide: true},
		}},
		{name: "Events", do: func() error { _, err := c.Events(ctx, "run-a", 0); return err },
			want: []storehttp.Call{run(storehttp.OpEvents, "inst-a", "run-a")}},
		{name: "Events without a run record", do: func() error { _, err := c.Events(ctx, "ghost", 0); return err },
			want: []storehttp.Call{{Op: storehttp.OpEvents, Run: "ghost", StoreWide: true}}},
		{name: "Release", do: func() error { return c.Release(ctx, fence) },
			want: []storehttp.Call{{Op: storehttp.OpRelease, Instance: "inst-a", Fence: &fence}}},
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if got := rec.take(); !reflect.DeepEqual(got, s.want) {
			t.Fatalf("%s: calls = %+v, want %+v", s.name, got, s.want)
		}
	}
}

// scoped allows only calls on instance inst-a and namespace ns-a, never a
// store-wide one: a token confined to one instance.
func scoped(c storehttp.Call) error {
	if c.StoreWide || (c.Instance != "" && c.Instance != "inst-a") {
		return errors.New("token is scoped to inst-a; " + secret)
	}
	for _, ns := range c.Namespaces {
		if ns != "ns-a" {
			return errors.New("token is scoped to ns-a")
		}
	}
	return nil
}

// scopedEnv is a store with runs on inst-a (claimed) and inst-b, a client
// with a token scoped to inst-a, and a client allowed everything.
type scopedEnv struct {
	inner  store.Store
	scoped *storehttp.Client
	all    *storehttp.Client
	fence  store.Fence
}

func newScopedEnv(t *testing.T) scopedEnv {
	t.Helper()
	inner := memstore.New()
	ctx := context.Background()
	for _, r := range []store.Run{newRun("run-a", "inst-a"), newRun("run-b", "inst-b")} {
		if _, _, err := inner.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	own, err := inner.Claim(ctx, "inst-a", "owner-a", time.Hour)
	must(t, err)
	return scopedEnv{
		inner:  inner,
		scoped: serve(t, inner, storehttp.HandlerOptions{Authorizer: storehttp.AuthorizerFunc(func(_ context.Context, c storehttp.Call) error { return scoped(c) })}),
		all:    serve(t, inner, storehttp.HandlerOptions{}),
		fence:  own.Fence(),
	}
}

// remote returns err's RemoteError, failing if it has none.
func remote(t testing.TB, err error) *storehttp.RemoteError {
	t.Helper()
	var r *storehttp.RemoteError
	if !errors.As(err, &r) {
		t.Fatalf("error %v is not a RemoteError", err)
	}
	return r
}

// alike fails unless a and b are the same answer: the same class, the
// same status, the same message apart from the request ID.
func alike(t testing.TB, what string, a, b error) {
	t.Helper()
	ra, rb := remote(t, a), remote(t, b)
	strip := func(r *storehttp.RemoteError, err error) string {
		return strings.ReplaceAll(err.Error(), r.RequestID, "")
	}
	if ra.Class != rb.Class || ra.StatusCode != rb.StatusCode || strip(ra, a) != strip(rb, b) {
		t.Fatalf("%s: answers differ: %v / %v", what, a, b)
	}
}

// A call that names only a run is authorized on that run's instance, and
// refused answers exactly as a missing run (ADR 0024, item 9).
func TestRunOnlyCallsMasked(t *testing.T) {
	e := newScopedEnv(t)
	ctx := context.Background()
	for name, call := range map[string]func(run string) error{
		"Run":           func(r string) error { _, err := e.scoped.Run(ctx, r); return err },
		"Entries":       func(r string) error { _, err := e.scoped.Entries(ctx, r, 0); return err },
		"CancelRequest": func(r string) error { _, _, err := e.scoped.CancelRequest(ctx, r); return err },
		"RequestCancel": func(r string) error { _, _, err := e.scoped.RequestCancel(ctx, r, store.CancelOperator); return err },
		"Status":        func(r string) error { _, err := e.scoped.Status(ctx, r); return err },
		"Events":        func(r string) error { _, err := e.scoped.Events(ctx, r, 0); return err },
		"Runs cursor": func(r string) error {
			_, _, err := e.scoped.Runs(ctx, "inst-a", store.RunQuery{After: r})
			return err
		},
	} {
		foreign, missing := call("run-b"), call("missing")
		if !errors.Is(foreign, store.ErrNotFound) || !errors.Is(missing, store.ErrNotFound) {
			t.Fatalf("%s: foreign = %v, missing = %v; want ErrNotFound for both", name, foreign, missing)
		}
		alike(t, name, foreign, missing)
		if err := call("run-a"); err != nil && name != "Runs cursor" {
			t.Fatalf("%s on its own run: %v", name, err)
		}
	}
	if _, ok, _ := e.inner.CancelRequest(ctx, "run-b"); ok {
		t.Fatal("a refused RequestCancel recorded a request")
	}
}

// Claimable is store-wide, refused to a scoped token; a refusal not masked
// is ErrForbidden, which is definite ErrInvalid.
func TestForbidden(t *testing.T) {
	e := newScopedEnv(t)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"Claimable":      func() error { _, err := e.scoped.Claimable(ctx); return err },
		"Ownership":      func() error { _, err := e.scoped.Ownership(ctx, "inst-b"); return err },
		"Object":         func() error { _, _, err := e.scoped.Object(ctx, "ns-b", "k"); return err },
		"ListObjects":    func() error { _, err := e.scoped.ListObjects(ctx, "ns-b", ""); return err },
		"CreateRun":      func() error { _, _, err := e.scoped.CreateRun(ctx, newRun("run-c", "inst-b")); return err },
		"Claim":          func() error { _, err := e.scoped.Claim(ctx, "inst-b", "o", time.Minute); return err },
		"InstanceStatus": func() error { _, err := e.scoped.InstanceStatus(ctx, "inst-b"); return err },
		"Artifact":       func() error { _, _, err := e.scoped.Artifact(ctx, "inst-b", digest(nil)); return err },
		"Commit other ns": func() error {
			return e.scoped.Commit(ctx, e.fence, []store.Op{store.PutObjectJSON("ns-b", "k", 0, json.RawMessage(`1`))})
		},
		"AppendEvents across instances": func() error {
			return e.scoped.AppendEvents(ctx, []store.Event{{Run: "run-a", Seq: 1, Kind: "k"}, {Run: "run-b", Seq: 1, Kind: "k"}})
		},
		"AppendEvents for an absent run": func() error {
			return e.scoped.AppendEvents(ctx, []store.Event{{Run: "run-a", Seq: 1, Kind: "k"}, {Run: "ghost", Seq: 1, Kind: "k"}})
		},
	} {
		err := call()
		if !errors.Is(err, storehttp.ErrForbidden) || !errors.Is(err, store.ErrInvalid) || !store.IsDefinite(err) {
			t.Fatalf("%s = %v, want definite ErrForbidden wrapping ErrInvalid", name, err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("%s: the refusal's text crossed the wire: %v", name, err)
		}
	}
	if obj, ok, _ := e.inner.Object(ctx, "ns-b", "k"); ok {
		t.Fatalf("a refused commit wrote %+v", obj)
	}
	tel, ok := e.inner.(store.Telemetry)
	if !ok {
		t.Fatal("the memory store keeps no telemetry")
	}
	if got, _ := tel.Events(ctx, "run-a", 0); len(got) != 0 {
		t.Fatalf("a refused AppendEvents wrote %+v", got)
	}
}

// The store's own answers that would name another instance keep their
// class and lose their text: a foreign cursor the caller may see is the
// store's ErrInvalid, and a commit naming a foreign run answers exactly as
// one naming a missing run.
func TestStoreMessagesWithheld(t *testing.T) {
	e := newScopedEnv(t)
	ctx := context.Background()
	_, _, err := e.all.Runs(ctx, "inst-a", store.RunQuery{After: "run-b"})
	if !errors.Is(err, store.ErrInvalid) || strings.Contains(err.Error(), "inst-b") {
		t.Fatalf("Runs with a foreign cursor = %v, want ErrInvalid naming nothing", err)
	}
	commitFor := func(run string) error {
		return e.scoped.Commit(ctx, e.fence, []store.Op{store.AppendEntryOp(store.Entry{Run: run, Seq: 1, Step: 1, Kind: "k"})})
	}
	foreign, missing := commitFor("run-b"), commitFor("missing")
	if !errors.Is(foreign, store.ErrInvalid) {
		t.Fatalf("commit naming a foreign run = %v, want ErrInvalid", foreign)
	}
	alike(t, "commit naming a run", foreign, missing)
}

// Telemetry for a run with no record passes a store-wide grant (an
// executing service), as the store accepts it.
func TestTelemetryStoreWide(t *testing.T) {
	inner := memstore.New()
	service := serve(t, inner, storehttp.HandlerOptions{Authorizer: storehttp.AuthorizerFunc(func(_ context.Context, c storehttp.Call) error {
		if !c.StoreWide {
			return errors.New("service identity: store-wide only")
		}
		return nil
	})})
	ctx := context.Background()
	must(t, service.AppendEvents(ctx, []store.Event{{Run: "ghost", Seq: 1, Kind: "k", Payload: json.RawMessage(`{}`)}}))
	got, err := service.Events(ctx, "ghost", 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("Events = %+v, %v", got, err)
	}
}

// An Authorizer that cannot decide makes the call indeterminate, never a
// refusal.
func TestAuthorizerUnavailable(t *testing.T) {
	inner := memstore.New()
	ctx := context.Background()
	_, _, err := inner.CreateRun(ctx, newRun("r", "i"))
	must(t, err)
	c := serve(t, inner, storehttp.HandlerOptions{Authorizer: storehttp.AuthorizerFunc(func(context.Context, storehttp.Call) error {
		return fmt.Errorf("token service down: %w", storehttp.ErrUnavailable)
	})})
	for name, err := range map[string]error{
		"Claim": func() error { _, err := c.Claim(ctx, "i", "o", time.Minute); return err }(),
		"Run":   func() error { _, err := c.Run(ctx, "r"); return err }(),
	} {
		if !errors.Is(err, storehttp.ErrUnavailable) || store.IsDefinite(err) {
			t.Fatalf("%s = %v, want indeterminate ErrUnavailable", name, err)
		}
	}
}
