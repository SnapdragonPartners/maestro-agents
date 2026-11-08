// Package storetest is the conformance suite for [store.Store]
// implementations (ADR 0005, item 6): the operations' semantics, claims and
// leases, run creation and queued triggers, durability across a reopen,
// and TEST-6's two stale-owner interleavings through the store's pause
// hook.
//
// A host store that claims HOST-6 runs the same suite as the reference
// stores:
//
//	func TestConformance(t *testing.T) {
//		storetest.Run(t, storetest.Harness{
//			Open: func(t testing.TB, o storetest.Options) store.Store { ... },
//		})
//	}
//
// Passing TEST-6 against a store that serializes every commit trivially
// (the memory store, a fake) is not evidence of fencing.
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Options configures a store the suite opens.
type Options struct {
	// Now is the clock the store must use for leases and timestamps. The
	// suite advances it to expire leases.
	Now func() time.Time
	// Pause is the test-only pause hook (store.PauseHook) the store must
	// call at both pause points of every commit.
	Pause store.PauseHook
}

// Harness adapts a store implementation to the suite.
type Harness struct {
	// Open returns a new, empty store configured with o. The suite closes
	// it. Required.
	Open func(t testing.TB, o Options) store.Store

	// Reopen, for a durable store, closes s and opens the same storage
	// again with o, as a process restart would. Nil skips the durability
	// tests.
	Reopen func(t testing.TB, s store.Store, o Options) store.Store
}

// Run runs the whole suite as subtests of t.
func Run(t *testing.T, h Harness) {
	t.Helper()
	if h.Open == nil {
		t.Fatal("storetest: Harness.Open is required")
	}
	for _, c := range cases() {
		t.Run(c.name, func(t *testing.T) { c.fn(t, h) })
	}
	t.Run("StaleOwnerBeforeCommit", func(t *testing.T) { staleOwnerBeforeCommit(t, h) })
	t.Run("StaleOwnerInsideCommit", func(t *testing.T) { staleOwnerInsideCommit(t, h) })
	t.Run("Durability", func(t *testing.T) {
		if h.Reopen == nil {
			t.Skip("store is not durable: Harness.Reopen is nil")
		}
		durability(t, h)
	})
}

type testCase struct {
	fn   func(testing.TB, Harness)
	name string
}

// Clock is a manual clock for leases.
type Clock struct {
	t  time.Time
	mu sync.Mutex
}

// NewClock returns a clock at a fixed instant.
func NewClock() *Clock {
	return &Clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

// Now returns the clock's time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

const (
	lease    = time.Minute
	instance = "inst-1"
	runID    = "run-1"
	ns       = "shared"
)

// env is a store with an instance that has a runnable run and an owner.
type env struct {
	s   store.Store
	clk *Clock
	own store.Ownership
	run store.Run
}

func open(t testing.TB, h Harness, pause store.PauseHook) (store.Store, *Clock) {
	t.Helper()
	clk := NewClock()
	s := h.Open(t, Options{Now: clk.Now, Pause: pause})
	t.Cleanup(func() { _ = s.Close() })
	return s, clk
}

func setup(t testing.TB, h Harness) *env {
	t.Helper()
	return setupWith(t, h, nil)
}

func setupWith(t testing.TB, h Harness, pause store.PauseHook) *env {
	t.Helper()
	s, clk := open(t, h, pause)
	run := createRun(t, s, instance, runID, "trigger-1")
	own := claim(t, s, instance, "owner-a")
	return &env{s: s, clk: clk, own: own, run: run}
}

func createRun(t testing.TB, s store.Store, inst, id, trigger string) store.Run {
	t.Helper()
	run, created, err := s.CreateRun(context.Background(), store.Run{
		ID: id, Instance: inst, AgentType: "test-agent", AgentVersion: "1",
		TriggerID: trigger, Trigger: json.RawMessage(`{"file":"a.pdf"}`),
	})
	if err != nil || !created {
		t.Fatalf("CreateRun(%s) = created %v, %v", id, created, err)
	}
	return run
}

func claim(t testing.TB, s store.Store, inst, owner string) store.Ownership {
	t.Helper()
	own, err := s.Claim(context.Background(), inst, owner, lease)
	if err != nil {
		t.Fatalf("Claim(%s, %s): %v", inst, owner, err)
	}
	return own
}

func commit(t testing.TB, s store.Store, f store.Fence, ops ...store.Op) {
	t.Helper()
	if err := s.Commit(context.Background(), f, ops); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// wantErr fails unless err matches target and is classified as definite.
func wantErr(t testing.TB, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want %v", err, target)
	}
	if !store.IsDefinite(err) {
		t.Fatalf("error %v is not classified as definite", err)
	}
}

// wantConflict fails unless err is a conflict on operation op with the
// given current version.
func wantConflict(t testing.TB, err error, op int, current uint64) *store.ConflictError {
	t.Helper()
	var c *store.ConflictError
	if !errors.As(err, &c) {
		t.Fatalf("error = %v, want a *store.ConflictError", err)
	}
	if !store.IsDefinite(err) {
		t.Fatalf("conflict %v is not classified as definite", err)
	}
	if c.Op != op || c.Current != current {
		t.Fatalf("conflict = op %d current %d, want op %d current %d", c.Op, c.Current, op, current)
	}
	return c
}

func entry(seq uint64, kind string) store.Entry {
	return store.Entry{Run: runID, Seq: seq, Step: 1, Kind: kind, Payload: json.RawMessage(fmt.Sprintf(`{"n":%d}`, seq))}
}

func checkpoint(seq, recordSeq uint64) store.Checkpoint {
	return store.Checkpoint{
		Instance: instance, Seq: seq, Scope: store.Scope{Kind: store.ScopeRun, ID: runID},
		RecordSeq: recordSeq, Format: 1, State: json.RawMessage(fmt.Sprintf(`{"cp":%d}`, seq)),
	}
}

func entries(t testing.TB, s store.Store, run string, after uint64) []store.Entry {
	t.Helper()
	got, err := s.Entries(context.Background(), run, after)
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	return got
}

func getRun(t testing.TB, s store.Store, id string) store.Run {
	t.Helper()
	run, err := s.Run(context.Background(), id)
	if err != nil {
		t.Fatalf("Run(%s): %v", id, err)
	}
	return run
}

func object(t testing.TB, s store.Store, key string) (store.Object, bool) {
	t.Helper()
	obj, ok, err := s.Object(context.Background(), ns, key)
	if err != nil {
		t.Fatalf("Object(%s): %v", key, err)
	}
	return obj, ok
}
