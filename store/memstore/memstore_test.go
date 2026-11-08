package memstore_test

import (
	"testing"

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
