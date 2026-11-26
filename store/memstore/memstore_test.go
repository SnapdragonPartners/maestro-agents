package memstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/internal/storecore"
	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/memstore"
	"github.com/SnapdragonPartners/maestro-agents/store/storetest"
)

// The memory store serializes every commit trivially, so passing TEST-6
// here is not evidence of fencing; the suite runs to hold its semantics to
// the file store's.
func TestConformance(t *testing.T) {
	storetest.Run(t, storetest.Harness{
		Open: func(_ testing.TB, o storetest.Options) store.Store {
			return memstore.New(memstore.WithClock(o.Now), memstore.WithPauseHook(o.Pause))
		},
	})
}

// The reference stores' page sizes (ADR 0024, item 7): a zero limit is
// the default page, and a larger limit is capped, each with more set.
func TestRunsPageSizes(t *testing.T) {
	s := memstore.New()
	ctx := context.Background()
	const inst = "inst-1"
	for i := range storecore.MaxRunsLimit + 1 {
		id := fmt.Sprintf("r-%04d", i)
		if _, _, err := s.CreateRun(ctx, store.Run{
			ID: id, Instance: inst, AgentType: "a", TriggerID: id, TriggerKind: "k", Trigger: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
		own, err := s.Claim(ctx, inst, "owner", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Commit(ctx, own.Fence(), []store.Op{
			store.UpdateRunOp(store.RunUpdate{Run: id, ExpectVersion: 1, Status: store.StatusSucceeded}), store.ReleaseOp(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ limit, want int }{
		{0, storecore.DefaultRunsLimit},
		{storecore.MaxRunsLimit + 1, storecore.MaxRunsLimit},
	} {
		page, more, err := s.Runs(ctx, inst, store.RunQuery{Limit: c.limit})
		if err != nil || len(page) != c.want || !more {
			t.Fatalf("Runs, limit %d = %d runs, more %v, %v; want %d with more", c.limit, len(page), more, err, c.want)
		}
	}
}
