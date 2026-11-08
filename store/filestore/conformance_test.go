package filestore_test

import (
	"testing"

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
