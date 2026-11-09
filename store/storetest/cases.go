package storetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

func cases() []testCase {
	return []testCase{
		{name: "ClaimAndLease", fn: testClaimAndLease},
		{name: "ExpiredOwnerStillCommits", fn: testExpiredOwnerStillCommits},
		{name: "Heartbeat", fn: testHeartbeat},
		{name: "ReleaseControlPlane", fn: testReleaseControlPlane},
		{name: "ReleaseInCommit", fn: testReleaseInCommit},
		{name: "Claimable", fn: testClaimable},
		{name: "CreateRun", fn: testCreateRun},
		{name: "Triggers", fn: testTriggers},
		{name: "CommitValidation", fn: testCommitValidation},
		{name: "ForeignRecords", fn: testForeignRecords},
		{name: "Entries", fn: testEntries},
		{name: "Checkpoints", fn: testCheckpoints},
		{name: "UpdateRun", fn: testUpdateRun},
		{name: "Artifacts", fn: testArtifacts},
		{name: "Objects", fn: testObjects},
		{name: "ObjectVersionsNeverRepeat", fn: testObjectVersionsNeverRepeat},
		{name: "DeleteAndRecreateInOneCommit", fn: testDeleteAndRecreateInOneCommit},
		{name: "ObjectKeys", fn: testObjectKeys},
		{name: "Atomicity", fn: testAtomicity},
		{name: "PreconditionsInOrder", fn: testPreconditionsInOrder},
		{name: "ConcurrentObjectWriters", fn: testConcurrentObjectWriters},
		{name: "EmptyBytes", fn: testEmptyBytes},
		{name: "ConflictReportsCommittedVersion", fn: testConflictReportsCommittedVersion},
		{name: "InvalidUTF8", fn: testInvalidUTF8},
	}
}

func testClaimAndLease(t testing.TB, h Harness) {
	s, clk := open(t, h, nil)
	ctx := context.Background()
	own, err := s.Ownership(ctx, instance)
	if err != nil || own.Owner != "" || own.Epoch != 0 {
		t.Fatalf("unclaimed Ownership = %+v, %v; want no owner at epoch 0", own, err)
	}
	a := claim(t, s, instance, "owner-a")
	if a.Owner != "owner-a" || a.Epoch != 1 || !a.Live(clk.Now()) || !a.LeaseExpiry.Equal(clk.Now().Add(lease)) {
		t.Fatalf("first claim = %+v", a)
	}
	_, err = s.Claim(ctx, instance, "owner-b", lease)
	if !errors.Is(err, store.ErrNotClaimable) {
		t.Fatalf("claim of a live instance = %v, want ErrNotClaimable", err)
	}
	clk.Advance(lease)
	b := claim(t, s, instance, "owner-b")
	if b.Epoch != 2 || b.Owner != "owner-b" {
		t.Fatalf("claim after expiry = %+v, want owner-b at epoch 2", b)
	}
	got, err := s.Ownership(ctx, instance)
	if err != nil || got != b {
		t.Fatalf("Ownership = %+v, %v; want %+v", got, err, b)
	}
	if _, err := s.Claim(ctx, instance, "", lease); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("claim with no owner = %v, want ErrInvalid", err)
	}
	if _, err := s.Claim(ctx, instance, "x", 0); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("claim with no lease = %v, want ErrInvalid", err)
	}
}

// An owner whose lease expired but whose epoch is current may still commit:
// fencing is by epoch, not by lease (ADR 0003, item 9).
func testExpiredOwnerStillCommits(t testing.TB, h Harness) {
	e := setup(t, h)
	e.clk.Advance(2 * lease)
	commit(t, e.s, e.own.Fence(), store.AppendEntryOp(entry(1, "activation_started")))
	// Once someone else claims, the old owner is rejected.
	b := claim(t, e.s, instance, "owner-b")
	err := e.s.Commit(context.Background(), e.own.Fence(), []store.Op{store.AppendEntryOp(entry(2, "turn"))})
	wantErr(t, err, store.ErrOwnership)
	commit(t, e.s, b.Fence(), store.AppendEntryOp(entry(2, "activation_started")))
}

func testHeartbeat(t testing.TB, h Harness) {
	e := setup(t, h)
	ctx := context.Background()
	e.clk.Advance(lease / 2)
	got, err := e.s.Heartbeat(ctx, e.own.Fence(), lease)
	if err != nil || !got.LeaseExpiry.Equal(e.clk.Now().Add(lease)) || got.Epoch != e.own.Epoch {
		t.Fatalf("Heartbeat = %+v, %v", got, err)
	}
	// A heartbeat after expiry still succeeds while nobody has claimed.
	e.clk.Advance(2 * lease)
	if _, err = e.s.Heartbeat(ctx, e.own.Fence(), lease); err != nil {
		t.Fatalf("heartbeat after expiry, unclaimed: %v", err)
	}
	if _, err = e.s.Claim(ctx, instance, "owner-b", lease); !errors.Is(err, store.ErrNotClaimable) {
		t.Fatalf("claim after heartbeat = %v, want ErrNotClaimable", err)
	}
	e.clk.Advance(2 * lease)
	claim(t, e.s, instance, "owner-b")
	_, err = e.s.Heartbeat(ctx, e.own.Fence(), lease)
	wantErr(t, err, store.ErrOwnership)
}

func testReleaseControlPlane(t testing.TB, h Harness) {
	e := setup(t, h)
	ctx := context.Background()
	stale := e.own.Fence()
	stale.Epoch++
	wantErr(t, e.s.Release(ctx, stale), store.ErrOwnership)
	if err := e.s.Release(ctx, e.own.Fence()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	got, err := e.s.Ownership(ctx, instance)
	if err != nil || got.Owner != "" || got.Epoch != e.own.Epoch || !got.LeaseExpiry.IsZero() {
		t.Fatalf("Ownership after release = %+v, %v; want no owner at epoch %d", got, err, e.own.Epoch)
	}
	err = e.s.Commit(ctx, e.own.Fence(), []store.Op{store.AppendEntryOp(entry(1, "turn"))})
	wantErr(t, err, store.ErrOwnership)
	if b := claim(t, e.s, instance, "owner-b"); b.Epoch != e.own.Epoch+1 {
		t.Fatalf("claim after release = epoch %d, want %d", b.Epoch, e.own.Epoch+1)
	}
}

func testReleaseInCommit(t testing.TB, h Harness) {
	e := setup(t, h)
	ctx := context.Background()
	commit(t, e.s, e.own.Fence(),
		store.AppendEntryOp(entry(1, "outcome")),
		store.UpdateRunOp(store.RunUpdate{Run: runID, ExpectVersion: 1, Status: store.StatusRunnable, Reason: store.Reason{Code: "step_limit"}}),
		store.ReleaseOp(),
	)
	got, err := e.s.Ownership(ctx, instance)
	if err != nil || got.Owner != "" || got.Live(e.clk.Now()) {
		t.Fatalf("Ownership after releasing commit = %+v, %v", got, err)
	}
	// The run is ready again at once, without waiting out the lease.
	ready, err := e.s.Claimable(ctx)
	if err != nil || len(ready) != 1 || ready[0].Run != runID {
		t.Fatalf("Claimable after release = %+v, %v", ready, err)
	}
	err = e.s.Commit(ctx, e.own.Fence(), []store.Op{store.AppendEntryOp(entry(2, "turn"))})
	wantErr(t, err, store.ErrOwnership)
}

func testClaimable(t testing.TB, h Harness) {
	s, clk := open(t, h, nil)
	ctx := context.Background()
	wantReady := func(want ...string) {
		t.Helper()
		got, err := s.Claimable(ctx)
		if err != nil {
			t.Fatalf("Claimable: %v", err)
		}
		var ids []string
		for _, c := range got {
			ids = append(ids, c.Instance+"/"+c.Run)
		}
		if strings.Join(ids, ",") != strings.Join(want, ",") {
			t.Fatalf("Claimable = %v, want %v", ids, want)
		}
	}
	wantReady()
	createRun(t, s, "inst-b", "run-b", "t")
	createRun(t, s, "inst-a", "run-a", "t")
	wantReady("inst-a/run-a", "inst-b/run-b")
	claim(t, s, "inst-a", "owner")
	wantReady("inst-b/run-b")
	clk.Advance(lease)
	wantReady("inst-a/run-a", "inst-b/run-b")
	a := claim(t, s, "inst-a", "owner")
	commit(t, s, a.Fence(), store.UpdateRunOp(store.RunUpdate{
		Run: "run-a", ExpectVersion: 1, Status: store.StatusSucceeded, Reason: store.Reason{Code: "changed"},
	}), store.ReleaseOp())
	wantReady("inst-b/run-b")
}

func testCreateRun(t testing.TB, h Harness) {
	s, clk := open(t, h, nil)
	ctx := context.Background()
	run := createRun(t, s, instance, runID, "trigger-1")
	if run.Status != store.StatusRunnable || run.Version != 1 || !run.CreatedAt.Equal(clk.Now()) {
		t.Fatalf("new run = %+v", run)
	}
	// A retried creation finds the existing run, whatever else it says.
	again, created, err := s.CreateRun(ctx, store.Run{ID: "run-other", Instance: instance, AgentType: "x", TriggerID: "trigger-1"})
	if err != nil || created || again.ID != runID {
		t.Fatalf("retried CreateRun = %+v, created %v, %v; want the existing run", again, created, err)
	}
	// Even a retry naming only the instance and the trigger.
	again, created, err = s.CreateRun(ctx, store.Run{Instance: instance, TriggerID: "trigger-1"})
	if err != nil || created || again.ID != runID {
		t.Fatalf("CreateRun with only the trigger = %+v, created %v, %v; want the existing run", again, created, err)
	}
	_, _, err = s.CreateRun(ctx, store.Run{ID: "run-2", Instance: instance, AgentType: "x", TriggerID: "trigger-2"})
	wantErr(t, err, store.ErrInstanceBusy)
	_, _, err = s.CreateRun(ctx, store.Run{ID: runID, Instance: "inst-2", AgentType: "x", TriggerID: "trigger-9"})
	wantErr(t, err, store.ErrInvalid)
	_, _, err = s.CreateRun(ctx, store.Run{ID: "run-3", Instance: "inst-2", TriggerID: "t"})
	wantErr(t, err, store.ErrInvalid)
	if _, err = s.Run(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Run(missing) = %v, want ErrNotFound", err)
	}
	active, ok, err := s.ActiveRun(ctx, instance)
	if err != nil || !ok || active.ID != runID {
		t.Fatalf("ActiveRun = %+v, %v, %v", active, ok, err)
	}
	// Once the run is terminal, the instance takes a new run.
	own := claim(t, s, instance, "owner")
	commit(t, s, own.Fence(), store.UpdateRunOp(store.RunUpdate{
		Run: runID, ExpectVersion: 1, Status: store.StatusFailed, Reason: store.Reason{Code: "budget_exhausted", Budget: "tokens"},
	}), store.ReleaseOp())
	if _, ok, err := s.ActiveRun(ctx, instance); err != nil || ok {
		t.Fatalf("ActiveRun after terminal = %v, %v; want none", ok, err)
	}
	createRun(t, s, instance, "run-2", "trigger-2")
	if got := getRun(t, s, runID); got.Status != store.StatusFailed || got.Reason.Budget != "tokens" {
		t.Fatalf("terminal run = %+v", got)
	}
}

func testTriggers(t testing.TB, h Harness) {
	s, _ := open(t, h, nil)
	ctx := context.Background()
	enqueue := func(id string) store.Trigger {
		t.Helper()
		tr, err := s.EnqueueTrigger(ctx, store.Trigger{Instance: instance, ID: id, Payload: json.RawMessage(`{"id":"` + id + `"}`)})
		if err != nil {
			t.Fatalf("EnqueueTrigger(%s): %v", id, err)
		}
		return tr
	}
	a, b := enqueue("a"), enqueue("b")
	if a.Seq >= b.Seq {
		t.Fatalf("arrival order: a.Seq %d, b.Seq %d", a.Seq, b.Seq)
	}
	if again := enqueue("a"); again.Seq != a.Seq || !again.EnqueuedAt.Equal(a.EnqueuedAt) {
		t.Fatalf("re-enqueue = %+v, want %+v", again, a)
	}
	c := enqueue("c")
	if err := s.RemoveTrigger(ctx, instance, "b"); err != nil {
		t.Fatalf("RemoveTrigger: %v", err)
	}
	if err := s.RemoveTrigger(ctx, instance, "b"); err != nil {
		t.Fatalf("RemoveTrigger of a removed trigger: %v", err)
	}
	got, err := s.Triggers(ctx, instance)
	if err != nil || len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" || got[1].Seq != c.Seq {
		t.Fatalf("Triggers = %+v, %v; want a then c", got, err)
	}
	if !bytes.Equal(got[0].Payload, []byte(`{"id":"a"}`)) {
		t.Fatalf("payload = %s", got[0].Payload)
	}
	// A queued trigger's run is created on its identity; then the trigger
	// is removed.
	run := createRun(t, s, instance, "run-a", "a")
	if err := s.RemoveTrigger(ctx, instance, "a"); err != nil {
		t.Fatalf("RemoveTrigger: %v", err)
	}
	if again, created, err := s.CreateRun(ctx, store.Run{ID: "run-a2", Instance: instance, AgentType: "x", TriggerID: "a"}); err != nil || created || again.ID != run.ID {
		t.Fatalf("CreateRun after removal = %+v, %v, %v", again, created, err)
	}
}

func testCommitValidation(t testing.TB, h Harness) {
	e := setup(t, h)
	ctx := context.Background()
	wantErr(t, e.s.Commit(ctx, e.own.Fence(), nil), store.ErrInvalid)
	bad := []store.Op{
		{Kind: store.OpPutObject},
		{Kind: "nope", Release: &store.Release{}},
		{Kind: store.OpRelease, PutObject: &store.PutObject{}},
		{Kind: store.OpPutObject, PutObject: &store.PutObject{Namespace: ns, Key: "k"}},
		{Kind: store.OpPutObject, PutObject: &store.PutObject{Namespace: ns, Key: "k", JSON: json.RawMessage(`{`)}},
		store.PutObjectJSON("", "k", 0, json.RawMessage(`1`)),
		store.PutObjectJSON(ns, "", 0, json.RawMessage(`1`)),
		store.PutObjectJSON(ns, strings.Repeat("K", 200), 0, json.RawMessage(`1`)),
		store.AppendEntryOp(store.Entry{Run: runID, Seq: 1}),
		store.AppendEntryOp(store.Entry{Run: runID, Seq: 0, Kind: "turn"}),
		store.PutCheckpointOp(store.Checkpoint{Instance: instance, Seq: 1}),
		store.UpdateRunOp(store.RunUpdate{Run: runID, ExpectVersion: 1, Status: "running"}),
		store.PutArtifactOp(store.Artifact{Digest: "abc", MediaType: "text/plain", Content: []byte("x")}),
		store.PutArtifactOp(store.Artifact{Digest: digest([]byte("y")), MediaType: "text/plain", Content: []byte("x")}),
		store.PutArtifactOp(store.Artifact{Digest: digest([]byte("x")), Content: []byte("x")}),
	}
	for _, op := range bad {
		wantErr(t, e.s.Commit(ctx, e.own.Fence(), []store.Op{op}), store.ErrInvalid)
	}
	noOwner := e.own.Fence()
	noOwner.Owner = ""
	wantErr(t, e.s.Commit(ctx, noOwner, []store.Op{store.ReleaseOp()}), store.ErrOwnership)
	other := e.own.Fence()
	other.Owner = "owner-z"
	wantErr(t, e.s.Commit(ctx, other, []store.Op{store.ReleaseOp()}), store.ErrOwnership)
	wantErr(t, e.s.Commit(ctx, store.Fence{Instance: "never", Owner: "o", Epoch: 1}, []store.Op{store.ReleaseOp()}), store.ErrOwnership)
	if got := entries(t, e.s, runID, 0); len(got) != 0 {
		t.Fatalf("entries after rejected commits = %+v", got)
	}
}

// An owner may write only its own instance's records.
func testForeignRecords(t testing.TB, h Harness) {
	e := setup(t, h)
	ctx := context.Background()
	createRun(t, e.s, "inst-2", "run-2", "t")
	claim(t, e.s, "inst-2", "owner-b")
	foreign := []store.Op{
		store.AppendEntryOp(store.Entry{Run: "run-2", Seq: 1, Kind: "turn"}),
		store.AppendEntryOp(store.Entry{Run: "missing", Seq: 1, Kind: "turn"}),
		store.UpdateRunOp(store.RunUpdate{Run: "run-2", ExpectVersion: 1, Status: store.StatusCancelled}),
		store.PutCheckpointOp(store.Checkpoint{Instance: "inst-2", Seq: 1, Scope: store.Scope{Kind: store.ScopeRun, ID: "run-2"}, State: json.RawMessage(`{}`)}),
		store.PutCheckpointOp(store.Checkpoint{Instance: instance, Seq: 1, Scope: store.Scope{Kind: store.ScopeRun, ID: "run-2"}, State: json.RawMessage(`{}`)}),
	}
	for _, op := range foreign {
		wantErr(t, e.s.Commit(ctx, e.own.Fence(), []store.Op{op}), store.ErrInvalid)
	}
	if got := entries(t, e.s, "run-2", 0); len(got) != 0 {
		t.Fatalf("foreign entries landed: %+v", got)
	}
}

func testEntries(t testing.TB, h Harness) {
	e := setup(t, h)
	ctx := context.Background()
	commit(t, e.s, e.own.Fence(), store.AppendEntryOp(entry(1, "activation_started")), store.AppendEntryOp(entry(2, "run_started")))
	err := e.s.Commit(ctx, e.own.Fence(), []store.Op{store.AppendEntryOp(entry(2, "turn"))})
	wantConflict(t, err, 0, 0)
	err = e.s.Commit(ctx, e.own.Fence(), []store.Op{store.AppendEntryOp(entry(4, "turn"))})
	c := wantConflict(t, err, 0, 0)
	if c.Kind != store.OpAppendEntry || c.IsObjectConflict() {
		t.Fatalf("conflict = %+v", c)
	}
	commit(t, e.s, e.own.Fence(), store.AppendEntryOp(entry(3, "model_attempt")))
	got := entries(t, e.s, runID, 1)
	if len(got) != 2 || got[0].Seq != 2 || got[1].Seq != 3 || got[1].Kind != "model_attempt" ||
		got[1].Step != 1 || !bytes.Equal(got[1].Payload, []byte(`{"n":3}`)) {
		t.Fatalf("Entries(after 1) = %+v", got)
	}
	if got := entries(t, e.s, runID, 3); len(got) != 0 {
		t.Fatalf("Entries(after 3) = %+v", got)
	}
}

func testCheckpoints(t testing.TB, h Harness) {
	e := setup(t, h)
	ctx := context.Background()
	if _, ok, err := e.s.LatestCheckpoint(ctx, instance); err != nil || ok {
		t.Fatalf("LatestCheckpoint with none = %v, %v", ok, err)
	}
	err := e.s.Commit(ctx, e.own.Fence(), []store.Op{store.PutCheckpointOp(checkpoint(2, 0))})
	wantConflict(t, err, 0, 0)
	commit(t, e.s, e.own.Fence(), store.AppendEntryOp(entry(1, "turn")), store.PutCheckpointOp(checkpoint(1, 1)))
	commit(t, e.s, e.own.Fence(), store.AppendEntryOp(entry(2, "turn")), store.PutCheckpointOp(checkpoint(2, 2)))
	err = e.s.Commit(ctx, e.own.Fence(), []store.Op{store.PutCheckpointOp(checkpoint(2, 2))})
	wantConflict(t, err, 0, 0)
	latest, ok, err := e.s.LatestCheckpoint(ctx, instance)
	if err != nil || !ok || latest.Seq != 2 || latest.RecordSeq != 2 || latest.Format != 1 ||
		latest.Scope != (store.Scope{Kind: store.ScopeRun, ID: runID}) || !bytes.Equal(latest.State, []byte(`{"cp":2}`)) {
		t.Fatalf("LatestCheckpoint = %+v, %v, %v", latest, ok, err)
	}
	first, err := e.s.Checkpoint(ctx, instance, 1)
	if err != nil || first.Seq != 1 {
		t.Fatalf("Checkpoint(1) = %+v, %v", first, err)
	}
	if _, err := e.s.Checkpoint(ctx, instance, 3); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Checkpoint(3) = %v, want ErrNotFound", err)
	}
	// A checkpoint whose scope is not a run (a later chat turn) is kept as
	// given.
	commit(t, e.s, e.own.Fence(), store.PutCheckpointOp(store.Checkpoint{
		Instance: instance, Seq: 3, Scope: store.Scope{Kind: "chat_turn", ID: "c/1"}, State: json.RawMessage(`{}`),
	}))
}

func testUpdateRun(t testing.TB, h Harness) {
	e := setup(t, h)
	ctx := context.Background()
	commit(t, e.s, e.own.Fence(), store.UpdateRunOp(store.RunUpdate{
		Run: runID, ExpectVersion: 1, Status: store.StatusRunnable, Reason: store.Reason{Code: "step_limit"},
		Consumed: json.RawMessage(`{"turns":1}`),
	}))
	err := e.s.Commit(ctx, e.own.Fence(), []store.Op{store.UpdateRunOp(store.RunUpdate{Run: runID, ExpectVersion: 1, Status: store.StatusRunnable})})
	c := wantConflict(t, err, 0, 2)
	if c.Kind != store.OpUpdateRun || c.IsObjectConflict() {
		t.Fatalf("conflict = %+v", c)
	}
	e.clk.Advance(1)
	commit(t, e.s, e.own.Fence(), store.UpdateRunOp(store.RunUpdate{
		Run: runID, ExpectVersion: 2, Status: store.StatusSucceeded, Reason: store.Reason{Code: "changed"},
	}))
	got := getRun(t, e.s, runID)
	if got.Version != 3 || got.Status != store.StatusSucceeded || got.Reason.Code != "changed" ||
		!bytes.Equal(got.Consumed, []byte(`{"turns":1}`)) || !got.UpdatedAt.Equal(e.clk.Now()) ||
		got.TriggerID != "trigger-1" || got.AgentType != "test-agent" {
		t.Fatalf("run after updates = %+v", got)
	}
	err = e.s.Commit(ctx, e.own.Fence(), []store.Op{store.UpdateRunOp(store.RunUpdate{Run: runID, ExpectVersion: 3, Status: store.StatusRunnable})})
	wantErr(t, err, store.ErrInvalid)
	// A host override of a terminal outcome stays terminal (RUN-4).
	commit(t, e.s, e.own.Fence(), store.UpdateRunOp(store.RunUpdate{
		Run: runID, ExpectVersion: 3, Status: store.StatusFailed, Reason: store.Reason{Code: "tool_error"},
	}))
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func testArtifacts(t testing.TB, h Harness) {
	e := setup(t, h)
	ctx := context.Background()
	content := []byte("%PDF-1.7 statement")
	d := digest(content)
	if _, _, err := e.s.Artifact(ctx, d); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Artifact before store = %v, want ErrNotFound", err)
	}
	commit(t, e.s, e.own.Fence(), store.PutArtifactOp(store.Artifact{Digest: d, MediaType: "application/pdf", Content: content}),
		store.AppendEntryOp(store.Entry{Run: runID, Seq: 1, Kind: "run_started", Payload: json.RawMessage(`{"doc":"` + d + `"}`)}))
	// Storing it again is a no-op whatever its metadata, inside one commit
	// and across commits.
	commit(t, e.s, e.own.Fence(),
		store.PutArtifactOp(store.Artifact{Digest: d, MediaType: "text/plain", Content: content}),
		store.PutArtifactOp(store.Artifact{Digest: d, MediaType: "text/csv", Content: content}))
	meta, got, err := e.s.Artifact(ctx, d)
	if err != nil || !bytes.Equal(got, content) || meta != (store.ArtifactMeta{Digest: d, MediaType: "application/pdf", Size: int64(len(content))}) {
		t.Fatalf("Artifact = %+v, %q, %v", meta, got, err)
	}
	// A no-op commit is still fenced.
	stale := e.own.Fence()
	stale.Epoch++
	wantErr(t, e.s.Commit(ctx, stale, []store.Op{store.PutArtifactOp(store.Artifact{Digest: d, MediaType: "application/pdf", Content: content})}), store.ErrOwnership)
	empty := digest(nil)
	commit(t, e.s, e.own.Fence(), store.PutArtifactOp(store.Artifact{Digest: empty, MediaType: "text/plain"}))
	if meta, got, err := e.s.Artifact(ctx, empty); err != nil || len(got) != 0 || meta.Size != 0 {
		t.Fatalf("empty Artifact = %+v, %q, %v", meta, got, err)
	}
	if _, _, err := e.s.Artifact(ctx, "../etc/passwd"); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("Artifact(bad digest) = %v, want ErrInvalid", err)
	}
}

func testObjects(t testing.TB, h Harness) {
	e := setup(t, h)
	ctx := context.Background()
	f := e.own.Fence()
	if obj, ok := object(t, e.s, "k"); ok || obj.Version != 0 {
		t.Fatalf("absent object = %+v, %v", obj, ok)
	}
	commit(t, e.s, f, store.PutObjectJSON(ns, "k", 0, json.RawMessage(`{"v":1}`)))
	err := e.s.Commit(ctx, f, []store.Op{store.PutObjectJSON(ns, "k", 0, json.RawMessage(`{"v":2}`))})
	c := wantConflict(t, err, 0, 1)
	if !c.IsObjectConflict() {
		t.Fatalf("conflict %+v is not an object conflict", c)
	}
	// A read after a conflict observes at least the reported version.
	if obj, ok := object(t, e.s, "k"); !ok || obj.Version < c.Current {
		t.Fatalf("read after conflict = %+v, %v; want version >= %d", obj, ok, c.Current)
	}
	commit(t, e.s, f, store.PutObjectJSON(ns, "k", 1, json.RawMessage(`{"v":2}`)))
	obj, ok := object(t, e.s, "k")
	if !ok || obj.Version != 2 || !bytes.Equal(obj.JSON, []byte(`{"v":2}`)) || obj.Bytes != nil || obj.Namespace != ns || obj.Key != "k" {
		t.Fatalf("object = %+v, %v", obj, ok)
	}
	commit(t, e.s, f, store.PutObjectBytes(ns, "b", 0, []byte{0, 1, 2}))
	if obj, ok := object(t, e.s, "b"); !ok || !bytes.Equal(obj.Bytes, []byte{0, 1, 2}) || obj.JSON != nil {
		t.Fatalf("bytes object = %+v, %v", obj, ok)
	}
	err = e.s.Commit(ctx, f, []store.Op{store.DeleteObjectOp(ns, "k", 1)})
	wantConflict(t, err, 0, 2)
	err = e.s.Commit(ctx, f, []store.Op{store.DeleteObjectOp(ns, "missing", 0)})
	wantConflict(t, err, 0, 0)
	// Other instances share the namespace (STORE-2).
	createRun(t, e.s, "inst-2", "run-2", "t")
	b := claim(t, e.s, "inst-2", "owner-b")
	commit(t, e.s, b.Fence(), store.DeleteObjectOp(ns, "k", 2))
	if obj, ok := object(t, e.s, "k"); ok || obj.Version != 3 {
		t.Fatalf("deleted object = %+v, %v; want absent at version 3", obj, ok)
	}
}

// A deletion is a version, so no writer holding a version from before the
// deletion can write after a re-creation.
func testObjectVersionsNeverRepeat(t testing.TB, h Harness) {
	e := setup(t, h)
	ctx := context.Background()
	f := e.own.Fence()
	commit(t, e.s, f, store.PutObjectJSON(ns, "k", 0, json.RawMessage(`"a"`)))
	commit(t, e.s, f, store.DeleteObjectOp(ns, "k", 1))
	err := e.s.Commit(ctx, f, []store.Op{store.PutObjectJSON(ns, "k", 0, json.RawMessage(`"b"`))})
	wantConflict(t, err, 0, 2)
	commit(t, e.s, f, store.PutObjectJSON(ns, "k", 2, json.RawMessage(`"b"`)))
	err = e.s.Commit(ctx, f, []store.Op{store.PutObjectJSON(ns, "k", 1, json.RawMessage(`"stale"`))})
	wantConflict(t, err, 0, 3)
	if obj, ok := object(t, e.s, "k"); !ok || obj.Version != 3 || string(obj.JSON) != `"b"` {
		t.Fatalf("object = %+v, %v", obj, ok)
	}
}

// A deletion and a re-creation in one commit keep the version climbing: the
// re-creation expects the tombstone's version, and the versions before it
// stay unusable (ADR 0006, item 4).
func testDeleteAndRecreateInOneCommit(t testing.TB, h Harness) {
	e := setup(t, h)
	ctx := context.Background()
	f := e.own.Fence()
	commit(t, e.s, f,
		store.PutObjectJSON(ns, "k", 0, json.RawMessage(`"a"`)),
		store.DeleteObjectOp(ns, "k", 1),
		store.PutObjectJSON(ns, "k", 2, json.RawMessage(`"b"`)),
	)
	if obj, ok := object(t, e.s, "k"); !ok || obj.Version != 3 || string(obj.JSON) != `"b"` {
		t.Fatalf("object = %+v, %v; want version 3", obj, ok)
	}
	commit(t, e.s, f, store.DeleteObjectOp(ns, "k", 3), store.PutObjectJSON(ns, "k", 4, json.RawMessage(`"c"`)))
	for _, stale := range []uint64{0, 1, 3} {
		err := e.s.Commit(ctx, f, []store.Op{store.DeleteObjectOp(ns, "k", 5), store.PutObjectJSON(ns, "k", stale, json.RawMessage(`"x"`))})
		wantConflict(t, err, 1, 5)
	}
	if obj, _ := object(t, e.s, "k"); obj.Version != 5 || string(obj.JSON) != `"c"` {
		t.Fatalf("object = %+v; want version 5", obj)
	}
}

func testObjectKeys(t testing.TB, h Harness) {
	e := setup(t, h)
	ctx := context.Background()
	keys := []string{"a", "A", "review/2026/a.pdf", "review/2026/B.pdf", "review/x", "..", ".hidden", "%41", "a b", "ü"}
	var ops []store.Op
	for i, k := range keys {
		ops = append(ops, store.PutObjectJSON(ns, k, 0, json.RawMessage(fmt.Sprint(i))))
	}
	commit(t, e.s, e.own.Fence(), ops...)
	commit(t, e.s, e.own.Fence(), store.PutObjectJSON("other", "review/elsewhere", 0, json.RawMessage(`1`)))
	commit(t, e.s, e.own.Fence(), store.DeleteObjectOp(ns, "review/x", 1))
	for i, k := range keys {
		obj, ok := object(t, e.s, k)
		if k == "review/x" {
			if ok {
				t.Fatalf("deleted key %q is present", k)
			}
			continue
		}
		if !ok || string(obj.JSON) != fmt.Sprint(i) {
			t.Fatalf("key %q = %+v, %v; want %d", k, obj, ok, i)
		}
	}
	list, err := e.s.ListObjects(ctx, ns, "review/")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	want := []store.ObjectInfo{{Key: "review/2026/B.pdf", Version: 1}, {Key: "review/2026/a.pdf", Version: 1}}
	if len(list) != len(want) || list[0] != want[0] || list[1] != want[1] {
		t.Fatalf("ListObjects(review/) = %+v, want %+v", list, want)
	}
	all, err := e.s.ListObjects(ctx, ns, "")
	if err != nil || len(all) != len(keys)-1 {
		t.Fatalf("ListObjects(all) = %d keys, %v; want %d", len(all), err, len(keys)-1)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].Key >= all[i].Key {
			t.Fatalf("ListObjects not ordered by key: %q before %q", all[i-1].Key, all[i].Key)
		}
	}
	if none, err := e.s.ListObjects(ctx, "empty", ""); err != nil || len(none) != 0 {
		t.Fatalf("ListObjects(empty namespace) = %+v, %v", none, err)
	}
}

// A commit applies all of its operations or none.
func testAtomicity(t testing.TB, h Harness) {
	e := setup(t, h)
	ctx := context.Background()
	content := []byte("artifact")
	err := e.s.Commit(ctx, e.own.Fence(), []store.Op{
		store.PutObjectJSON(ns, "k", 0, json.RawMessage(`1`)),
		store.PutArtifactOp(store.Artifact{Digest: digest(content), MediaType: "text/plain", Content: content}),
		store.AppendEntryOp(entry(1, "turn")),
		store.PutCheckpointOp(checkpoint(1, 1)),
		store.UpdateRunOp(store.RunUpdate{Run: runID, ExpectVersion: 1, Status: store.StatusSucceeded}),
		store.ReleaseOp(),
		store.AppendEntryOp(entry(3, "turn")),
	})
	wantConflict(t, err, 6, 0)
	if _, ok := object(t, e.s, "k"); ok {
		t.Fatal("object landed from a rejected commit")
	}
	if _, _, err := e.s.Artifact(ctx, digest(content)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("artifact from a rejected commit: %v", err)
	}
	if got := entries(t, e.s, runID, 0); len(got) != 0 {
		t.Fatalf("entries from a rejected commit: %+v", got)
	}
	if _, ok, _ := e.s.LatestCheckpoint(ctx, instance); ok {
		t.Fatal("checkpoint from a rejected commit")
	}
	if run := getRun(t, e.s, runID); run.Version != 1 || run.Status != store.StatusRunnable {
		t.Fatalf("run changed by a rejected commit: %+v", run)
	}
	if own, _ := e.s.Ownership(ctx, instance); own.Owner != e.own.Owner {
		t.Fatalf("ownership released by a rejected commit: %+v", own)
	}
}

// Preconditions are checked in order against the commit's earlier
// operations (ADR 0005, item 13).
func testPreconditionsInOrder(t testing.TB, h Harness) {
	e := setup(t, h)
	commit(t, e.s, e.own.Fence(),
		store.PutObjectJSON(ns, "k", 0, json.RawMessage(`1`)),
		store.PutObjectJSON(ns, "k", 1, json.RawMessage(`2`)),
		store.AppendEntryOp(entry(1, "permission")),
		store.AppendEntryOp(entry(2, "attempt")),
		store.AppendEntryOp(entry(3, "result")),
		store.PutCheckpointOp(checkpoint(1, 3)),
		store.UpdateRunOp(store.RunUpdate{Run: runID, ExpectVersion: 1, Status: store.StatusRunnable}),
		store.UpdateRunOp(store.RunUpdate{Run: runID, ExpectVersion: 2, Status: store.StatusSucceeded}),
	)
	if obj, _ := object(t, e.s, "k"); obj.Version != 2 || string(obj.JSON) != "2" {
		t.Fatalf("object = %+v", obj)
	}
	if run := getRun(t, e.s, runID); run.Version != 3 || run.Status != store.StatusSucceeded {
		t.Fatalf("run = %+v", run)
	}
	if got := entries(t, e.s, runID, 0); len(got) != 3 {
		t.Fatalf("entries = %+v", got)
	}
}

// Instances sharing a namespace race on one counter; each retries on an
// object conflict. No increment is lost.
func testConcurrentObjectWriters(t testing.TB, h Harness) {
	s, _ := open(t, h, nil)
	ctx := context.Background()
	const writers, increments = 4, 10
	fences := make([]store.Fence, writers)
	for i := range fences {
		inst := fmt.Sprintf("inst-%d", i)
		createRun(t, s, inst, "run-"+inst, "t")
		fences[i] = claim(t, s, inst, "owner").Fence()
	}
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for _, f := range fences {
		wg.Go(func() {
			for n := 0; n < increments; {
				obj, _, err := s.Object(ctx, ns, "counter")
				if err != nil {
					errs <- err
					return
				}
				var v int
				if obj.JSON != nil {
					_ = json.Unmarshal(obj.JSON, &v)
				}
				err = s.Commit(ctx, f, []store.Op{store.PutObjectJSON(ns, "counter", obj.Version, json.RawMessage(fmt.Sprint(v+1)))})
				var c *store.ConflictError
				switch {
				case err == nil:
					n++
				case errors.As(err, &c) && c.IsObjectConflict():
				default:
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("writer: %v", err)
	}
	obj, ok := object(t, s, "counter")
	if !ok || string(obj.JSON) != fmt.Sprint(writers*increments) || obj.Version != writers*increments {
		t.Fatalf("counter = %+v, want %d", obj, writers*increments)
	}
}

// An empty byte value is a value: it reads back as present, with Bytes
// empty but set and JSON unset.
func testEmptyBytes(t testing.TB, h Harness) {
	e := setup(t, h)
	commit(t, e.s, e.own.Fence(), store.PutObjectBytes(ns, "empty", 0, []byte{}))
	obj, ok := object(t, e.s, "empty")
	if !ok || obj.Bytes == nil || len(obj.Bytes) != 0 || obj.JSON != nil || obj.Version != 1 {
		t.Fatalf("empty bytes object = %+v (bytes nil: %v), %v", obj, obj.Bytes == nil, ok)
	}
}

// A conflict inside a commit reports the committed version, never one only
// the rejected commit would have written (ADR 0006, item 4).
func testConflictReportsCommittedVersion(t testing.TB, h Harness) {
	e := setup(t, h)
	ctx := context.Background()
	f := e.own.Fence()
	for _, tc := range []struct {
		ops     []store.Op
		current uint64
	}{
		{ops: []store.Op{store.PutObjectJSON(ns, "k", 0, json.RawMessage(`1`)), store.PutObjectJSON(ns, "k", 0, json.RawMessage(`2`))}},
		{ops: []store.Op{store.PutObjectJSON(ns, "k", 0, json.RawMessage(`1`)), store.DeleteObjectOp(ns, "k", 0)}},
		{ops: []store.Op{store.PutObjectJSON(ns, "k", 0, json.RawMessage(`1`)), store.DeleteObjectOp(ns, "k", 1), store.DeleteObjectOp(ns, "k", 2)}},
		{ops: []store.Op{
			store.UpdateRunOp(store.RunUpdate{Run: runID, ExpectVersion: 1, Status: store.StatusRunnable}),
			store.UpdateRunOp(store.RunUpdate{Run: runID, ExpectVersion: 1, Status: store.StatusRunnable}),
		}, current: 1},
	} {
		err := e.s.Commit(ctx, f, tc.ops)
		c := wantConflict(t, err, len(tc.ops)-1, tc.current)
		if c.IsObjectConflict() {
			if obj, _ := object(t, e.s, "k"); obj.Version < c.Current {
				t.Fatalf("read after conflict = version %d, conflict reported %d", obj.Version, c.Current)
			}
		}
	}
	if run := getRun(t, e.s, runID); run.Version != 1 {
		t.Fatalf("run version = %d, want 1", run.Version)
	}
	// With a committed version to report: the conflict names it, not the
	// rejected commit's 2, and a read after the conflict sees at least it.
	commit(t, e.s, f, store.PutObjectJSON(ns, "k", 0, json.RawMessage(`1`)))
	err := e.s.Commit(ctx, f, []store.Op{
		store.PutObjectJSON(ns, "k", 1, json.RawMessage(`2`)),
		store.PutObjectJSON(ns, "k", 1, json.RawMessage(`3`)),
	})
	c := wantConflict(t, err, 1, 1)
	if obj, _ := object(t, e.s, "k"); obj.Version < c.Current || obj.Version != 1 || string(obj.JSON) != "1" {
		t.Fatalf("read after conflict = %+v, conflict reported %d", obj, c.Current)
	}
}

// Every stored string must survive JSON serialization, so invalid UTF-8 is
// refused at every write and read that names a record.
func testInvalidUTF8(t testing.TB, h Harness) {
	e := setup(t, h)
	ctx := context.Background()
	const bad = "invalid-\xff"
	_, _, err := e.s.CreateRun(ctx, store.Run{ID: bad, Instance: "inst-2", AgentType: "a", TriggerID: "t"})
	wantErr(t, err, store.ErrInvalid)
	_, _, err = e.s.CreateRun(ctx, store.Run{ID: "r", Instance: bad, AgentType: "a", TriggerID: "t"})
	wantErr(t, err, store.ErrInvalid)
	_, _, err = e.s.CreateRun(ctx, store.Run{ID: "r", Instance: "inst-2", AgentType: bad, TriggerID: "t"})
	wantErr(t, err, store.ErrInvalid)
	_, err = e.s.Claim(ctx, "inst-2", bad, lease)
	wantErr(t, err, store.ErrInvalid)
	_, err = e.s.EnqueueTrigger(ctx, store.Trigger{Instance: instance, ID: bad})
	wantErr(t, err, store.ErrInvalid)
	// Raw JSON values too: encoding/json keeps invalid UTF-8 in a raw value
	// byte for byte, which a reader decoding it would not get back.
	badJSON := json.RawMessage("\"invalid-\xff\"")
	_, err = e.s.EnqueueTrigger(ctx, store.Trigger{Instance: instance, ID: "t-bad", Payload: badJSON})
	wantErr(t, err, store.ErrInvalid)
	_, _, err = e.s.CreateRun(ctx, store.Run{ID: "r", Instance: "inst-2", AgentType: "a", TriggerID: "t", Trigger: badJSON})
	wantErr(t, err, store.ErrInvalid)
	_, _, err = e.s.CreateRun(ctx, store.Run{ID: "r", Instance: "inst-2", AgentType: "a", TriggerID: "t", Consumed: badJSON})
	wantErr(t, err, store.ErrInvalid)
	for _, op := range []store.Op{
		store.PutObjectJSON(ns, bad, 0, json.RawMessage(`1`)),
		store.PutObjectJSON(bad, "k", 0, json.RawMessage(`1`)),
		store.DeleteObjectOp(ns, bad, 1),
		store.AppendEntryOp(store.Entry{Run: runID, Seq: 1, Kind: bad}),
		store.PutCheckpointOp(store.Checkpoint{Instance: instance, Seq: 1, Scope: store.Scope{Kind: "chat_turn", ID: bad}, State: json.RawMessage(`{}`)}),
		store.UpdateRunOp(store.RunUpdate{Run: runID, ExpectVersion: 1, Status: store.StatusFailed, Reason: store.Reason{Code: bad}}),
		store.PutArtifactOp(store.Artifact{Digest: digest(nil), MediaType: bad}),
		store.PutObjectJSON(ns, "k", 0, badJSON),
		store.AppendEntryOp(store.Entry{Run: runID, Seq: 1, Kind: "k", Payload: badJSON}),
		store.PutCheckpointOp(store.Checkpoint{Instance: instance, Seq: 1, Scope: store.Scope{Kind: store.ScopeRun, ID: runID}, State: badJSON}),
		store.UpdateRunOp(store.RunUpdate{Run: runID, ExpectVersion: 1, Status: store.StatusRunnable, Consumed: badJSON}),
	} {
		wantErr(t, e.s.Commit(ctx, e.own.Fence(), []store.Op{op}), store.ErrInvalid)
	}
	if _, _, err := e.s.Object(ctx, ns, bad); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("Object(invalid key) = %v, want ErrInvalid", err)
	}
	if _, err := e.s.Run(ctx, bad); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("Run(invalid ID) = %v, want ErrInvalid", err)
	}
}
