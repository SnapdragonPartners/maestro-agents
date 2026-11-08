package filestore_test

import (
	"context"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/filestore"
	"github.com/SnapdragonPartners/maestro-agents/store/storetest"
)

func TestConformance(t *testing.T) {
	dirs := map[store.Store]string{}
	open := func(t testing.TB, dir string, o storetest.Options) store.Store {
		t.Helper()
		s, err := filestore.Open(dir, filestore.WithClock(o.Now), filestore.WithPauseHook(o.Pause))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		dirs[s] = dir
		return s
	}
	storetest.Run(t, storetest.Harness{
		Open: func(t testing.TB, o storetest.Options) store.Store {
			return open(t, t.TempDir(), o)
		},
		Reopen: func(t testing.TB, s store.Store, o storetest.Options) store.Store {
			if err := s.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			return open(t, dirs[s], o)
		},
	})
}

// The file store drops every lease on reopen: its lock proves the previous
// process gone (ADR 0005, item 16). The suite accepts a store that keeps
// leases, so this property is tested here.
func TestReopenDropsLeases(t *testing.T) {
	dir := t.TempDir()
	clk := storetest.NewClock()
	ctx := context.Background()
	s, err := filestore.Open(dir, filestore.WithClock(clk.Now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	a, err := s.Claim(ctx, "inst", "owner-a", time.Hour)
	if err != nil || !a.Live(clk.Now()) {
		t.Fatalf("Claim = %+v, %v", a, err)
	}
	if err = s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s, err = filestore.Open(dir, filestore.WithClock(clk.Now))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	own, err := s.Ownership(ctx, "inst")
	if err != nil || own.Owner != "owner-a" || own.Epoch != a.Epoch || !own.LeaseExpiry.IsZero() {
		t.Fatalf("Ownership after reopen = %+v, %v; want owner-a at epoch %d with no lease", own, err, a.Epoch)
	}
	b, err := s.Claim(ctx, "inst", "owner-b", time.Hour)
	if err != nil || b.Epoch != a.Epoch+1 {
		t.Fatalf("Claim after reopen without advancing the clock = %+v, %v", b, err)
	}
}
