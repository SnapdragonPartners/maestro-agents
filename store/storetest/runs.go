package storetest

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// An instance's runs, paged (ADR 0024, item 7), and each entry's commit
// time, which a run's status reports as its last activity.

// finish makes the instance's active run terminal and releases it.
func finish(t testing.TB, s store.Store, inst, run string, st store.Status) {
	t.Helper()
	own := claim(t, s, inst, "owner")
	r := getRun(t, s, run)
	commit(t, s, own.Fence(), store.UpdateRunOp(store.RunUpdate{Run: run, ExpectVersion: r.Version, Status: st}), store.ReleaseOp())
}

// runIDs pages through the instance's runs as q selects them, with page
// size limit, checking each page's size and more.
func runIDs(t testing.TB, s store.Store, inst string, q store.RunQuery) []string {
	t.Helper()
	var ids []string
	for {
		page, more, err := s.Runs(context.Background(), inst, q)
		if err != nil {
			t.Fatalf("Runs(%s, %+v): %v", inst, q, err)
		}
		if q.Limit > 0 && len(page) > q.Limit {
			t.Fatalf("Runs(%+v) returned %d runs, over the limit", q, len(page))
		}
		if more && len(page) == 0 {
			t.Fatalf("Runs(%+v) returned an empty page with more", q)
		}
		for _, r := range page {
			if r.Instance != inst {
				t.Fatalf("Runs(%s) returned run %s of instance %s", inst, r.ID, r.Instance)
			}
			ids = append(ids, r.ID)
		}
		if !more {
			return ids
		}
		q.After = page[len(page)-1].ID
	}
}

func testRuns(t testing.TB, h Harness) {
	s, clk := open(t, h, nil)
	ctx := context.Background()
	if page, more, err := s.Runs(ctx, instance, store.RunQuery{}); err != nil || more || len(page) != 0 {
		t.Fatalf("Runs of an instance with none = %v, %v, %v", page, more, err)
	}
	// Five runs: r-b and r-a share a creation time, so the run ID orders
	// them; the trigger IDs order differently from the runs.
	steps := []struct {
		id, trigger string
		status      store.Status
		advance     bool
	}{
		{"r-c", "t-5", store.StatusSucceeded, true},
		{"r-b", "t-4", store.StatusFailed, true},
		{"r-a", "t-3", store.StatusSucceeded, false},
		{"r-e", "t-2", store.StatusCancelled, true},
		{"r-d", "t-1", store.StatusRunnable, true},
	}
	for _, st := range steps {
		if st.advance {
			clk.Advance(time.Second)
		}
		createRun(t, s, instance, st.id, st.trigger)
		if st.status != store.StatusRunnable {
			finish(t, s, instance, st.id, st.status)
		}
	}
	createRun(t, s, "inst-2", "other", "t-1")

	oldest := []string{"r-c", "r-a", "r-b", "r-e", "r-d"}
	newest := slices.Clone(oldest)
	slices.Reverse(newest)
	for _, limit := range []int{0, 1, 2, 5, 6} {
		if got := runIDs(t, s, instance, store.RunQuery{Limit: limit}); !slices.Equal(got, oldest) {
			t.Fatalf("Runs oldest first, limit %d = %v, want %v", limit, got, oldest)
		}
		if got := runIDs(t, s, instance, store.RunQuery{Limit: limit, Newest: true}); !slices.Equal(got, newest) {
			t.Fatalf("Runs newest first, limit %d = %v, want %v", limit, got, newest)
		}
	}
	page, more, err := s.Runs(ctx, instance, store.RunQuery{Limit: 2})
	if err != nil || !more || len(page) != 2 || page[0].Status != store.StatusSucceeded || page[0].TriggerID != "t-5" {
		t.Fatalf("first page of 2 = %+v, more %v, %v", page, more, err)
	}
	// A page that ends exactly at the last run has no more.
	if page, more, err = s.Runs(ctx, instance, store.RunQuery{After: "r-b", Limit: 2}); err != nil || more || len(page) != 2 {
		t.Fatalf("last full page = %+v, more %v, %v", page, more, err)
	}

	// The status filter, and the last run of a status in one query.
	failed := store.RunQuery{Status: []store.Status{store.StatusFailed}}
	if got := runIDs(t, s, instance, failed); !slices.Equal(got, []string{"r-b"}) {
		t.Fatalf("failed runs = %v", got)
	}
	done := store.RunQuery{Status: []store.Status{store.StatusSucceeded, store.StatusCancelled}, Limit: 1}
	if got := runIDs(t, s, instance, done); !slices.Equal(got, []string{"r-c", "r-a", "r-e"}) {
		t.Fatalf("succeeded or cancelled runs, a page of 1 = %v", got)
	}
	last, more, err := s.Runs(ctx, instance, store.RunQuery{Status: []store.Status{store.StatusSucceeded}, Newest: true, Limit: 1})
	if err != nil || !more || len(last) != 1 || last[0].ID != "r-a" {
		t.Fatalf("last succeeded = %+v, more %v, %v", last, more, err)
	}
	// After need not match the filter: the page continues past it.
	if got := runIDs(t, s, instance, store.RunQuery{After: "r-b", Status: []store.Status{store.StatusSucceeded}}); len(got) != 0 {
		t.Fatalf("succeeded runs after r-b = %v, want none", got)
	}
	if got := runIDs(t, s, instance, store.RunQuery{After: "r-b", Newest: true, Status: []store.Status{store.StatusSucceeded}}); !slices.Equal(got, []string{"r-a", "r-c"}) {
		t.Fatalf("succeeded runs after r-b, newest first = %v", got)
	}
	if got := runIDs(t, s, instance, store.RunQuery{After: "r-d"}); len(got) != 0 {
		t.Fatalf("runs after the newest = %v", got)
	}

	for _, q := range []store.RunQuery{
		{Limit: -1},
		{Status: []store.Status{"running"}},
		{After: "other"},
	} {
		_, _, qerr := s.Runs(ctx, instance, q)
		wantErr(t, qerr, store.ErrInvalid)
	}
	_, _, err = s.Runs(ctx, instance, store.RunQuery{After: "no-such-run"})
	wantErr(t, err, store.ErrNotFound)
	_, _, err = s.Runs(ctx, "", store.RunQuery{})
	wantErr(t, err, store.ErrInvalid)
}

func testEntryTimes(t testing.TB, h Harness) {
	e := setup(t, h)
	clk := e.clk
	clk.Advance(time.Second)
	t1 := clk.Now()
	// A given time is ignored; one commit's entries share its time.
	given := entry(1, "activation_started")
	given.At = t1.Add(-time.Hour)
	commit(t, e.s, e.own.Fence(), store.AppendEntryOp(given), store.AppendEntryOp(entry(2, "run_started")))
	clk.Advance(time.Second)
	t2 := clk.Now()
	commit(t, e.s, e.own.Fence(), store.AppendEntryOp(entry(3, "model_attempt")))
	// A clock stepped back stamps the previous entry's time.
	clk.Advance(-time.Minute)
	commit(t, e.s, e.own.Fence(), store.AppendEntryOp(entry(4, "turn")))
	got := entries(t, e.s, runID, 0)
	want := []time.Time{t1, t1, t2, t2}
	if len(got) != len(want) {
		t.Fatalf("Entries = %+v", got)
	}
	for i, en := range got {
		if !en.At.Equal(want[i]) {
			t.Fatalf("entry %d committed at %v, want %v", en.Seq, en.At, want[i])
		}
	}
}

// Every ownership a store returns carries its clock's reading, by which a
// reader judges the lease (ADR 0024, item 7).
func testOwnershipAsOf(t testing.TB, h Harness) {
	s, clk := open(t, h, nil)
	ctx := context.Background()
	createRun(t, s, instance, runID, "trigger-1")
	clk.Advance(time.Second)
	own, err := s.Claim(ctx, instance, "owner-a", lease)
	if err != nil || !own.AsOf.Equal(clk.Now()) || !own.Live(own.AsOf) {
		t.Fatalf("Claim = %+v, %v; want as of %v and live", own, err, clk.Now())
	}
	clk.Advance(time.Second)
	hb, _, err := s.Heartbeat(ctx, own.Fence(), lease)
	if err != nil || !hb.AsOf.Equal(clk.Now()) {
		t.Fatalf("Heartbeat = %+v, %v; want as of %v", hb, err, clk.Now())
	}
	clk.Advance(2 * lease)
	read, err := s.Ownership(ctx, instance)
	if err != nil || !read.AsOf.Equal(clk.Now()) || read.Live(read.AsOf) {
		t.Fatalf("Ownership after the lease = %+v, %v; want as of %v and not live", read, err, clk.Now())
	}
}
