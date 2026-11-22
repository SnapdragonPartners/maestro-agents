package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Cancel requests (ADR 0024, item 6): a record of their own, unfenced,
// idempotent with the first cause standing, returned by the owner's
// heartbeat, ignored by a terminal run, and never a conflict for the
// owner's commits.

// requestCancel records a cancel request and checks it was recorded.
func requestCancel(t testing.TB, s store.Store, run string, cause store.CancelCause) store.CancelRequest {
	t.Helper()
	req, recorded, err := s.RequestCancel(context.Background(), run, cause)
	if err != nil || !recorded || req.Run != run {
		t.Fatalf("RequestCancel(%s, %s) = %+v, recorded %v, %v", run, cause, req, recorded, err)
	}
	return req
}

// wantNoCancel checks a run has no cancel request.
func wantNoCancel(t testing.TB, s store.Store, run string) {
	t.Helper()
	req, ok, err := s.CancelRequest(context.Background(), run)
	if err != nil || ok {
		t.Fatalf("CancelRequest(%s) = %+v, %v, %v; want none", run, req, ok, err)
	}
}

func testCancelRequest(t testing.TB, h Harness) {
	e := setup(t, h)
	ctx := context.Background()
	wantNoCancel(t, e.s, runID)
	_, _, err := e.s.RequestCancel(ctx, runID, "bored")
	wantErr(t, err, store.ErrInvalid)
	_, _, err = e.s.RequestCancel(ctx, runID, "")
	wantErr(t, err, store.ErrInvalid)
	_, _, err = e.s.RequestCancel(ctx, "", store.CancelOperator)
	wantErr(t, err, store.ErrInvalid)
	_, _, err = e.s.RequestCancel(ctx, "no-such-run", store.CancelOperator)
	wantErr(t, err, store.ErrNotFound)
	_, _, err = e.s.CancelRequest(ctx, "no-such-run")
	wantErr(t, err, store.ErrNotFound)
	wantNoCancel(t, e.s, runID)

	e.clk.Advance(1)
	req := requestCancel(t, e.s, runID, store.CancelUnauthorized)
	if req.Cause != store.CancelUnauthorized || !req.RequestedAt.Equal(e.clk.Now()) {
		t.Fatalf("request = %+v; want cause unauthorized at %v", req, e.clk.Now())
	}
	got, ok, err := e.s.CancelRequest(ctx, runID)
	if err != nil || !ok || got != req {
		t.Fatalf("CancelRequest = %+v, %v, %v; want %+v", got, ok, err, req)
	}
	// The first cause stands.
	e.clk.Advance(1)
	again, recorded, err := e.s.RequestCancel(ctx, runID, store.CancelOperator)
	if err != nil || !recorded || again != req {
		t.Fatalf("second RequestCancel = %+v, recorded %v, %v; want the first, %+v", again, recorded, err, req)
	}
	// The run record is unchanged: same version, still runnable.
	if r := getRun(t, e.s, runID); r.Version != 1 || r.Status != store.StatusRunnable {
		t.Fatalf("run after cancel request = %+v; want runnable at version 1", r)
	}
	// The owner's heartbeat returns it.
	_, beat, err := e.s.Heartbeat(ctx, e.own.Fence(), lease)
	if err != nil || beat == nil || *beat != req {
		t.Fatalf("Heartbeat's cancel request = %+v, %v; want %+v", beat, err, req)
	}
	// The owner's commits expect the run record's version as before.
	commit(t, e.s, e.own.Fence(),
		store.AppendEntryOp(entry(1, "outcome")),
		store.UpdateRunOp(store.RunUpdate{Run: runID, ExpectVersion: 1, Status: store.StatusCancelled, Reason: store.Reason{Code: "unauthorized"}}),
		store.ReleaseOp(),
	)
	if got, ok, cerr := e.s.CancelRequest(ctx, runID); cerr != nil || !ok || got != req {
		t.Fatalf("CancelRequest after the run ended = %+v, %v, %v; want %+v", got, ok, cerr, req)
	}
	// A later run on the instance has its own: the old run's request is
	// not returned with its heartbeat.
	createRun(t, e.s, instance, "run-2", "trigger-2")
	b := claim(t, e.s, instance, ownerB)
	if _, beat, berr := e.s.Heartbeat(ctx, b.Fence(), lease); berr != nil || beat != nil {
		t.Fatalf("Heartbeat for a later run = %+v, %v; want no cancel request", beat, berr)
	}
	wantNoCancel(t, e.s, "run-2")
	// Another instance's request is not returned either.
	createRun(t, e.s, "inst-2", "run-3", "trigger-3")
	requestCancel(t, e.s, "run-3", store.CancelOperator)
	if _, beat, berr := e.s.Heartbeat(ctx, b.Fence(), lease); berr != nil || beat != nil {
		t.Fatalf("Heartbeat with another instance's request = %+v, %v; want none", beat, berr)
	}
	// A cancel request leaves a ready run claimable: the next activation
	// claims it to cancel it.
	ready, err := e.s.Claimable(ctx)
	if err != nil || len(ready) != 1 || ready[0].Run != "run-3" {
		t.Fatalf("Claimable = %+v, %v; want run-3", ready, err)
	}
}

// A run already terminal ignores a cancel request (ADR 0024, item 6).
func testCancelRequestTerminalRun(t testing.TB, h Harness) {
	e := setup(t, h)
	commit(t, e.s, e.own.Fence(),
		store.UpdateRunOp(store.RunUpdate{Run: runID, ExpectVersion: 1, Status: store.StatusSucceeded, Reason: store.Reason{Code: "changed"}}),
		store.ReleaseOp(),
	)
	req, recorded, err := e.s.RequestCancel(context.Background(), runID, store.CancelOperator)
	if err != nil || recorded || req != (store.CancelRequest{}) {
		t.Fatalf("RequestCancel of a terminal run = %+v, recorded %v, %v; want nothing recorded", req, recorded, err)
	}
	wantNoCancel(t, e.s, runID)
	if r := getRun(t, e.s, runID); r.Version != 2 || r.Status != store.StatusSucceeded {
		t.Fatalf("run = %+v; want succeeded at version 2", r)
	}
}

// A request recorded before the owner's final commit never makes it
// conflict: the run keeps the outcome it commits.
func testCancelRequestBeforeFinalCommit(t testing.TB, h Harness) {
	e := setup(t, h)
	req := requestCancel(t, e.s, runID, store.CancelOperator)
	commit(t, e.s, e.own.Fence(),
		store.AppendEntryOp(entry(1, "outcome")),
		store.UpdateRunOp(store.RunUpdate{Run: runID, ExpectVersion: 1, Status: store.StatusSucceeded, Reason: store.Reason{Code: "changed"}}),
		store.ReleaseOp(),
	)
	if r := getRun(t, e.s, runID); r.Status != store.StatusSucceeded || r.Version != 2 {
		t.Fatalf("run = %+v; want succeeded at version 2", r)
	}
	if got, ok, err := e.s.CancelRequest(context.Background(), runID); err != nil || !ok || got != req {
		t.Fatalf("CancelRequest = %+v, %v, %v; want %+v kept", got, ok, err, req)
	}
}

// A request racing the owner's final commit is serialized with it: made
// while the commit is inside its critical section, it returns only once
// the commit has landed, and then finds the run terminal and records
// nothing.
func testCancelRequestRacesFinalCommit(t testing.TB, h Harness) {
	p := newPauser(store.PauseAfterOwnershipRead)
	e := setupWith(t, h, p.hook)
	aErr := make(chan error, 1)
	go func() {
		aErr <- e.s.Commit(context.Background(), e.own.Fence(), []store.Op{
			store.AppendEntryOp(entry(1, "outcome")),
			store.UpdateRunOp(store.RunUpdate{Run: runID, ExpectVersion: 1, Status: store.StatusSucceeded, Reason: store.Reason{Code: "changed"}}),
			store.ReleaseOp(),
		})
	}()
	waitPaused(t, p, aErr)
	type result struct {
		err      error
		req      store.CancelRequest
		recorded bool
	}
	cErr := make(chan result, 1)
	go func() {
		req, recorded, err := e.s.RequestCancel(context.Background(), runID, store.CancelOperator)
		cErr <- result{req: req, recorded: recorded, err: err}
	}()
	select {
	case r := <-cErr:
		// A store may finish the request first only if the commit's
		// critical section does not cover the run's status: then the
		// request must have been recorded and the commit must still land.
		close(p.resume)
		if r.err != nil || !r.recorded {
			t.Fatalf("RequestCancel during the final commit = %+v; want it recorded or blocked", r)
		}
		if err := within(t, aErr, "the final commit"); err != nil {
			t.Fatalf("final commit after a cancel request: %v", err)
		}
	case <-time.After(blockedFor):
		close(p.resume)
		if err := within(t, aErr, "the final commit"); err != nil {
			t.Fatalf("final commit: %v", err)
		}
		r := within(t, cErr, "RequestCancel")
		if r.err != nil || r.recorded {
			t.Fatalf("RequestCancel after the final commit = %+v; want nothing recorded", r)
		}
		wantNoCancel(t, e.s, runID)
	}
	if got := getRun(t, e.s, runID); got.Status != store.StatusSucceeded || got.Version != 2 {
		t.Fatalf("run = %+v; want succeeded at version 2", got)
	}
}
