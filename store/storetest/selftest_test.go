package storetest

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/memstore"
)

// recorder runs a suite function and records its failures instead of
// failing the real test, so the suite itself can be tested against stores
// that are broken on purpose.
type recorder struct {
	testing.TB
	failures []string
	mu       sync.Mutex
}

func (r *recorder) Helper() {}

func (r *recorder) Fatalf(format string, args ...any) {
	r.mu.Lock()
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
	r.mu.Unlock()
	runtime.Goexit()
}

func (r *recorder) Fatal(args ...any) { r.Fatalf("%s", fmt.Sprint(args...)) }

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func record(t *testing.T, fn func(testing.TB, Harness), h Harness) []string {
	t.Helper()
	r := &recorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(r, h)
	}()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatalf("suite case did not finish within a minute; failures so far: %v", r.failures)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failures
}

type fault int

const (
	// noFence commits under whoever owns the instance at write time, as a
	// store that never checks the fence at commit would.
	noFence fault = iota + 1
	// unserialized checks the fence, then writes without holding anything
	// that keeps a claim out in between.
	unserialized
)

// broken wraps a correct memory store with one fencing fault. It calls the
// pause hook itself, at the points a real store would.
type broken struct {
	store.Store
	pause store.PauseHook
	fault fault
}

func (b *broken) Commit(ctx context.Context, f store.Fence, ops []store.Op) error {
	b.pause(ctx, store.PauseBeforeCommit, f)
	own, err := b.Ownership(ctx, f.Instance)
	if err != nil {
		return err
	}
	b.pause(ctx, store.PauseAfterOwnershipRead, f)
	if b.fault == unserialized {
		if own.Owner != f.Owner || own.Epoch != f.Epoch {
			return store.ErrOwnership
		}
		// The check passed; by now someone else may own the instance.
		if own, err = b.Ownership(ctx, f.Instance); err != nil {
			return err
		}
	}
	return b.Store.Commit(ctx, own.Fence(), ops)
}

func brokenHarness(f fault) Harness {
	return Harness{Open: func(_ testing.TB, o Options) store.Store {
		pause := o.Pause
		if pause == nil {
			pause = func(context.Context, store.PausePoint, store.Fence) {}
		}
		return &broken{Store: memstore.New(memstore.WithClock(o.Now)), pause: pause, fault: f}
	}}
}

func correctHarness() Harness {
	return Harness{Open: func(_ testing.TB, o Options) store.Store {
		return memstore.New(memstore.WithClock(o.Now), memstore.WithPauseHook(o.Pause))
	}}
}

// TEST-6's first pause point detects a store that does not check ownership
// at commit time.
func TestStaleOwnerBeforeCommitDetectsMissingFence(t *testing.T) {
	if got := record(t, staleOwnerBeforeCommit, correctHarness()); len(got) != 0 {
		t.Fatalf("correct store failed: %v", got)
	}
	got := record(t, staleOwnerBeforeCommit, brokenHarness(noFence))
	if len(got) == 0 {
		t.Fatal("a store with no fence check passed the before-commit test")
	}
	if !strings.Contains(strings.Join(got, "\n"), "ownership lost") {
		t.Fatalf("unexpected failure: %v", got)
	}
}

// TEST-6's second pause point detects a check and write not serialized
// against the transfer, which the first pause point cannot.
func TestStaleOwnerInsideCommitDetectsUnserializedCheck(t *testing.T) {
	if got := record(t, staleOwnerInsideCommit, correctHarness()); len(got) != 0 {
		t.Fatalf("correct store failed: %v", got)
	}
	if got := record(t, staleOwnerBeforeCommit, brokenHarness(unserialized)); len(got) != 0 {
		t.Fatalf("the before-commit test should not detect this fault, yet failed: %v", got)
	}
	got := record(t, staleOwnerInsideCommit, brokenHarness(unserialized))
	if len(got) == 0 {
		t.Fatal("a store whose check and write are not serialized passed the inside-commit test")
	}
}

// A store that never calls the pause hook fails both fencing tests with a
// diagnosis, rather than hanging the suite.
func TestFencingTestsFailWithoutPauseHook(t *testing.T) {
	noHook := Harness{Open: func(_ testing.TB, o Options) store.Store {
		return memstore.New(memstore.WithClock(o.Now))
	}}
	for name, fn := range map[string]func(testing.TB, Harness){
		"before": staleOwnerBeforeCommit,
		"inside": staleOwnerInsideCommit,
	} {
		got := record(t, fn, noHook)
		if len(got) == 0 || !strings.Contains(strings.Join(got, "\n"), "pause hook") {
			t.Fatalf("%s-commit test against a store without the pause hook = %v; want a pause hook failure", name, got)
		}
	}
}
