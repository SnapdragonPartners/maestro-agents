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
)

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
	<-p.paused

	clk.Advance(lease)
	b := claim(t, s, instance, ownerB)
	commit(t, s, b.Fence(), store.PutObjectJSON(ns, "b-was-here", 0, json.RawMessage(`true`)))

	close(p.resume)
	err := <-aErr
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
	<-p.paused

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
	errA := <-aErr
	if !bFirst {
		b = <-bDone
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

// durability checks a reopened store: records survive, and every recorded
// owner has no lease, so its instance is claimable at once and the next
// claim increments the epoch (ADR 0005, item 16).
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
	if err != nil || own.Owner != ownerA || own.Epoch != a.Epoch || !own.LeaseExpiry.IsZero() || own.Live(clk.Now()) {
		t.Fatalf("Ownership after reopen = %+v, %v; want owner A at epoch %d with no lease", own, err, a.Epoch)
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
