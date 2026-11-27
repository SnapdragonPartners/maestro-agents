package storehttp_test

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/filestore"
	"github.com/SnapdragonPartners/maestro-agents/store/memstore"
	"github.com/SnapdragonPartners/maestro-agents/store/storehttp"
	"github.com/SnapdragonPartners/maestro-agents/store/storetest"
)

// quiet discards the handler's log.
var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// serve serves inner over httptest with o (AllowAll and a quiet log unless
// set) and returns a client for it. The server and the client close with
// the test; closing the client does not close inner.
func serve(t testing.TB, inner store.Store, o storehttp.HandlerOptions) *storehttp.Client {
	t.Helper()
	if o.Authorizer == nil {
		o.Authorizer = storehttp.AllowAll{}
	}
	if o.Logger == nil {
		o.Logger = quiet
	}
	h, err := storehttp.NewHandler(inner, o)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := storehttp.NewClient(srv.URL, storehttp.ClientOptions{HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// wired is a client that also closes the store it serves, as the suite
// expects of the store it opens.
type wired struct {
	*storehttp.Client
	inner store.Store
}

func (w wired) Close() error {
	_ = w.Client.Close()
	return w.inner.Close()
}

// The wire form proves the same contract as the store behind it (ADR
// 0024, item 9): the suite runs against the client, the pause hook being
// the inner store's, so TEST-6 exercises the real store across the wire.
func TestConformanceMemstore(t *testing.T) {
	storetest.Run(t, storetest.Harness{
		Open: func(t testing.TB, o storetest.Options) store.Store {
			inner := memstore.New(memstore.WithClock(o.Now), memstore.WithPauseHook(o.Pause))
			return wired{Client: serve(t, inner, storehttp.HandlerOptions{}), inner: inner}
		},
	})
}

func TestConformanceFilestore(t *testing.T) {
	dirs := map[store.Store]string{}
	open := func(t testing.TB, dir string, o storetest.Options) store.Store {
		t.Helper()
		inner, err := filestore.Open(dir, filestore.WithClock(o.Now), filestore.WithPauseHook(o.Pause))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		s := wired{Client: serve(t, inner, storehttp.HandlerOptions{}), inner: inner}
		dirs[s] = dir
		return s
	}
	storetest.Run(t, storetest.Harness{
		Open: func(t testing.TB, o storetest.Options) store.Store { return open(t, t.TempDir(), o) },
		Reopen: func(t testing.TB, s store.Store, o storetest.Options) store.Store {
			if err := s.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			return open(t, dirs[s], o)
		},
	})
}
