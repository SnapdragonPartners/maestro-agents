package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

const (
	ownerA = "owner-a"
	ownerB = "owner-b"
	// blockedFor is how long the inside-commit test waits for B's claim
	// before concluding that the claim is blocked behind A's commit. A
	// store that does not serialize the claim against the commit lets the
	// claim return at once, well inside this.
	blockedFor = 300 * time.Millisecond
	// hangFor bounds every other wait in the fencing tests, so a store
	// that never reaches a pause point, or never returns, fails with a
	// diagnosis instead of hanging until the test binary's timeout.
	hangFor = 30 * time.Second
)

// waitPaused waits for A's commit to reach the pause point. A commit that
// returns without pausing, or a pause that never comes, means the store
// does not call the pause hook where TEST-6 needs it.
func waitPaused(t testing.TB, p *pauser, aErr <-chan error) {
	t.Helper()
	select {
	case <-p.paused:
	case err := <-aErr:
		t.Fatalf("A's commit returned (%v) without reaching pause point %v: the store must call the pause hook at both points of every commit", err, p.at)
	case <-time.After(hangFor):
		t.Fatalf("A's commit did not reach pause point %v within %v: the store must call the pause hook at both points of every commit", p.at, hangFor)
	}
}

// within receives from c, failing the test if nothing arrives in hangFor.
func within[T any](t testing.TB, c <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(hangFor):
		t.Fatalf("%s did not return within %v", what, hangFor)
		var zero T
		return zero
	}
}

// pauser pauses the first commit by ownerA at one pause point.
type pauser struct {
	paused chan struct{}
	resume chan struct{}
	armed  atomic.Bool
	at     store.PausePoint
}

func newPauser(at store.PausePoint) *pauser {
	p := &pauser{at: at, paused: make(chan struct{}), resume: make(chan struct{})}
	p.armed.Store(true)
	return p
}

func (p *pauser) hook(_ context.Context, at store.PausePoint, f store.Fence) {
	if at == p.at && f.Owner == ownerA && p.armed.CompareAndSwap(true, false) {
		close(p.paused)
		<-p.resume
	}
}

// aWrites are A's fenced writes, one of each kind TEST-6 names: an effect
// (an object write), an execution-record append, a checkpoint and a
// run-record change. None conflicts with anything B writes, so only the
// fence can stop them.
func aWrites() []store.Op {
	return []store.Op{
		store.PutObjectJSON(ns, "submission", 0, json.RawMessage(`{"by":"a"}`)),
		store.AppendEntryOp(entry(1, "result")),
		store.PutCheckpointOp(checkpoint(1, 1)),
		store.UpdateRunOp(store.RunUpdate{Run: runID, ExpectVersion: 1, Status: store.StatusSucceeded, Reason: store.Reason{Code: "changed"}}),
	}
}

// aLanded reports which of A's writes are visible; it fails the test if
// only some are, since a commit is atomic.
func aLanded(t testing.TB, s store.Store) bool {
	t.Helper()
	ctx := context.Background()
	obj, okObj := object(t, s, "submission")
	okObj = okObj && string(obj.JSON) == `{"by":"a"}`
	es := entries(t, s, runID, 0)
	okEntry := len(es) == 1 && es[0].Kind == "result"
	cp, okCP, err := s.LatestCheckpoint(ctx, instance)
	if err != nil {
		t.Fatalf("LatestCheckpoint: %v", err)
	}
	okCP = okCP && cp.Seq == 1
	okRun := getRun(t, s, runID).Status == store.StatusSucceeded
	if okObj != okEntry || okEntry != okCP || okCP != okRun {
		t.Fatalf("A's commit landed partly: object %v, entry %v, checkpoint %v, run %v", okObj, okEntry, okCP, okRun)
	}
	return okObj
}

// staleOwnerBeforeCommit is TEST-6's first pause point: A is paused before
// it starts the commit; ownership moves to B, and B commits; A resumes and
// none of A's fenced writes may land. It fails if ownership is not checked
// at commit time at all.
func staleOwnerBeforeCommit(t testing.TB, h Harness) {
	p := newPauser(store.PauseBeforeCommit)
	s, clk := open(t, h, p.hook)
	ctx := context.Background()
	createRun(t, s, instance, runID, "trigger-1")
	a := claim(t, s, instance, ownerA)

	aErr := make(chan error, 1)
	go func() { aErr <- s.Commit(ctx, a.Fence(), aWrites()) }()
	waitPaused(t, p, aErr)

	clk.Advance(lease)
	b := claim(t, s, instance, ownerB)
	commit(t, s, b.Fence(), store.PutObjectJSON(ns, "b-was-here", 0, json.RawMessage(`true`)))

	close(p.resume)
	err := within(t, aErr, "A's commit")
	wantErr(t, err, store.ErrOwnership)
	if aLanded(t, s) {
		t.Fatal("A's fenced writes landed after B's transfer committed")
	}
	if own, _ := s.Ownership(ctx, instance); own.Owner != ownerB || own.Epoch != b.Epoch {
		t.Fatalf("ownership = %+v, want B's", own)
	}
}

// staleOwnerInsideCommit is TEST-6's second pause point: A is paused after
// reading the ownership record and before writing, while B attempts the
// transfer. Either order is correct: B's claim waits for A's commit (A's
// writes land and B's claim follows), or B's claim commits first and A's
// commit is rejected. The test fails only if A's writes land after B's
// claim has committed, which a check and write not serialized against the
// transfer allows.
func staleOwnerInsideCommit(t testing.TB, h Harness) {
	p := newPauser(store.PauseAfterOwnershipRead)
	s, clk := open(t, h, p.hook)
	ctx := context.Background()
	createRun(t, s, instance, runID, "trigger-1")
	a := claim(t, s, instance, ownerA)

	aErr := make(chan error, 1)
	go func() { aErr <- s.Commit(ctx, a.Fence(), aWrites()) }()
	waitPaused(t, p, aErr)

	// A's lease has expired; its epoch is still current, so its commit is
	// legitimate until B's claim commits.
	clk.Advance(lease)
	type claimResult struct {
		err error
		own store.Ownership
	}
	bDone := make(chan claimResult, 1)
	go func() {
		own, err := s.Claim(ctx, instance, ownerB, lease)
		bDone <- claimResult{own: own, err: err}
	}()

	var b claimResult
	bFirst := false
	select {
	case b = <-bDone:
		bFirst = true
	case <-time.After(blockedFor):
	}
	close(p.resume)
	errA := within(t, aErr, "A's commit")
	if !bFirst {
		b = within(t, bDone, "B's claim, after A's commit")
	}

	if bFirst {
		// B's transfer committed while A was paused: A must be rejected.
		if b.err != nil {
			t.Fatalf("B's claim returned while A was paused, with error %v", b.err)
		}
		wantErr(t, errA, store.ErrOwnership)
		if aLanded(t, s) {
			t.Fatal("A's fenced writes landed after B's transfer committed")
		}
		return
	}
	// B's claim waited for A's commit to finish.
	switch {
	case errA == nil:
		if !aLanded(t, s) {
			t.Fatal("A's commit returned nil but its writes are not visible")
		}
	case errors.Is(errA, store.ErrOwnership):
		if aLanded(t, s) {
			t.Fatal("A's commit was rejected but its writes are visible")
		}
	default:
		t.Fatalf("A's commit: %v", errA)
	}
	if b.err != nil || b.own.Owner != ownerB || b.own.Epoch != a.Epoch+1 {
		t.Fatalf("B's claim after A's commit = %+v, %v", b.own, b.err)
	}
	err := s.Commit(ctx, a.Fence(), []store.Op{store.AppendEntryOp(entry(2, "turn"))})
	wantErr(t, err, store.ErrOwnership)
}

// durability checks a reopened store: records survive, owners and epochs
// survive, and a reopen never extends a lease. A store may drop every lease
// on reopen, as the file store does because its lock proves the previous
// process gone (ADR 0005, item 16), or keep each lease, as a store shared
// by several processes must; either way the instance is claimable once the
// lease is gone, and the next claim increments the epoch.
func durability(t testing.TB, h Harness) {
	clk := NewClock()
	o := Options{Now: clk.Now}
	s := h.Open(t, o)
	ctx := context.Background()
	createRun(t, s, instance, runID, "trigger-1")
	a := claim(t, s, instance, ownerA)
	content := []byte("statement")
	commit(t, s, a.Fence(),
		store.PutObjectJSON(ns, "k", 0, json.RawMessage(`{"v":1}`)),
		store.PutArtifactOp(store.Artifact{Digest: digest(content), MediaType: "text/plain", Content: content}),
		store.AppendEntryOp(entry(1, "activation_started")),
		store.PutCheckpointOp(checkpoint(1, 1)),
		store.UpdateRunOp(store.RunUpdate{Run: runID, ExpectVersion: 1, Status: store.StatusRunnable, Reason: store.Reason{Code: "shutdown"}}),
	)
	commit(t, s, a.Fence(), store.DeleteObjectOp(ns, "k", 1))
	if _, err := s.EnqueueTrigger(ctx, store.Trigger{Instance: instance, ID: "queued"}); err != nil {
		t.Fatalf("EnqueueTrigger: %v", err)
	}

	s = h.Reopen(t, s, o)
	t.Cleanup(func() { _ = s.Close() })

	own, err := s.Ownership(ctx, instance)
	if err != nil || own.Owner != ownerA || own.Epoch != a.Epoch || own.LeaseExpiry.After(a.LeaseExpiry) {
		t.Fatalf("Ownership after reopen = %+v, %v; want owner A at epoch %d, lease no later than %v",
			own, err, a.Epoch, a.LeaseExpiry)
	}
	if own.Live(clk.Now()) {
		// The store kept the lease: let it run out.
		clk.Advance(lease)
	}
	ready, err := s.Claimable(ctx)
	if err != nil || len(ready) != 1 || ready[0].Run != runID {
		t.Fatalf("Claimable after reopen = %+v, %v", ready, err)
	}
	if obj, ok := object(t, s, "k"); ok || obj.Version != 2 {
		t.Fatalf("deleted object after reopen = %+v, %v; want absent at version 2", obj, ok)
	}
	if _, got, rerr := s.Artifact(ctx, digest(content)); rerr != nil || string(got) != "statement" {
		t.Fatalf("Artifact after reopen = %q, %v", got, rerr)
	}
	if es := entries(t, s, runID, 0); len(es) != 1 {
		t.Fatalf("entries after reopen = %+v", es)
	}
	if cp, ok, rerr := s.LatestCheckpoint(ctx, instance); rerr != nil || !ok || cp.Seq != 1 {
		t.Fatalf("checkpoint after reopen = %+v, %v, %v", cp, ok, rerr)
	}
	if run := getRun(t, s, runID); run.Version != 2 || run.Reason.Code != "shutdown" {
		t.Fatalf("run after reopen = %+v", run)
	}
	if tr, rerr := s.Triggers(ctx, instance); rerr != nil || len(tr) != 1 || tr[0].ID != "queued" {
		t.Fatalf("triggers after reopen = %+v, %v", tr, rerr)
	}
	b := claim(t, s, instance, ownerB)
	if b.Epoch != a.Epoch+1 {
		t.Fatalf("claim after reopen = epoch %d, want %d", b.Epoch, a.Epoch+1)
	}
	err = s.Commit(ctx, a.Fence(), []store.Op{store.AppendEntryOp(entry(2, "turn"))})
	wantErr(t, err, store.ErrOwnership)
	commit(t, s, b.Fence(), store.AppendEntryOp(entry(2, "activation_started")))
}
