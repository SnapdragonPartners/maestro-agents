package storehttp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/memstore"
	"github.com/SnapdragonPartners/maestro-agents/store/storehttp"
)

// failing is a memory store whose Claim and Ownership return err.
type failing struct {
	store.Store
	err error
}

func (f failing) Claim(context.Context, string, string, time.Duration) (store.Ownership, error) {
	return store.Ownership{}, f.err
}

func (f failing) Ownership(context.Context, string) (store.Ownership, error) {
	return store.Ownership{}, f.err
}

// secret is text an inner error carries that must never reach the wire.
const secret = "run belongs to instance other-tenant"

// Every class keeps its identity and definiteness across the wire, and no
// inner error's text crosses it (ADR 0026, item 6).
func TestErrorClasses(t *testing.T) {
	for _, tc := range []struct {
		err      error
		name     string
		definite bool
	}{
		{name: "ownership", err: fmt.Errorf("%w: %s", store.ErrOwnership, secret), definite: true},
		{name: "invalid", err: fmt.Errorf("%w: %s", store.ErrInvalid, secret), definite: true},
		{name: "not found", err: fmt.Errorf("%w: %s", store.ErrNotFound, secret), definite: true},
		{name: "not claimable", err: fmt.Errorf("%w: %s", store.ErrNotClaimable, secret), definite: true},
		{name: "instance busy", err: fmt.Errorf("%w: %s", store.ErrInstanceBusy, secret), definite: true},
		{name: "conflict", err: &store.ConflictError{Kind: store.OpPutObject, Op: 2, Current: 7, Detail: secret}, definite: true},
		{name: "unusable", err: fmt.Errorf("%w: %s", store.ErrUnusable, secret)},
		{name: "other", err: errors.New(secret)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := serve(t, failing{Store: memstore.New(), err: tc.err}, storehttp.HandlerOptions{})
			for _, call := range []func() error{
				func() error { _, err := c.Claim(context.Background(), "i", "o", time.Minute); return err },
				func() error { _, err := c.Ownership(context.Background(), "i"); return err },
			} {
				err := call()
				var remote *storehttp.RemoteError
				if !errors.As(err, &remote) || remote.RequestID == "" {
					t.Fatalf("error %v is not a RemoteError with a request ID", err)
				}
				if store.IsDefinite(err) != tc.definite {
					t.Fatalf("IsDefinite(%v) = %v, want %v", err, !tc.definite, tc.definite)
				}
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error %q carries the inner error's text", err)
				}
				var conflict *store.ConflictError
				switch {
				case errors.As(tc.err, &conflict):
					var got *store.ConflictError
					if !errors.As(err, &got) || got.Kind != store.OpPutObject || got.Op != 2 || got.Current != 7 {
						t.Fatalf("conflict = %v, want put_object op 2 current 7", err)
					}
				case !tc.definite:
					if !errors.Is(err, storehttp.ErrUnavailable) || errors.Is(err, store.ErrUnusable) {
						t.Fatalf("error = %v, want ErrUnavailable and never ErrUnusable", err)
					}
				default:
					if !errors.Is(err, errors.Unwrap(tc.err)) && !errors.Is(err, tc.err) {
						t.Fatalf("error = %v, want %v", err, tc.err)
					}
				}
			}
		})
	}
}

// Anything the client cannot classify is indeterminate (ADR 0003, item
// 14): an unknown class, a body that is not the error body, a status with
// no class, a conflict with no conflict body, a dropped connection.
func TestUnclassifiedIsIndeterminate(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"unknown class": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"class":"brand_new","message":"x","request_id":"r"}`)
		},
		"garbage body": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `<html>bad gateway</html>`)
		},
		"5xx without class": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		},
		"conflict without body": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"class":"conflict","message":"x","request_id":"r"}`)
		},
		"200 with garbage": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"owner":`)
		},
		"dropped connection": func(w http.ResponseWriter, _ *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)
			c, err := storehttp.NewClient(srv.URL, storehttp.ClientOptions{HTTPClient: srv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Claim(context.Background(), "i", "o", time.Minute)
			if err == nil || store.IsDefinite(err) {
				t.Fatalf("Claim = %v, want an indeterminate error", err)
			}
		})
	}
}

// A connection refused is indeterminate too, and a closed client is
// unusable.
func TestClientTransportAndClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	c, err := storehttp.NewClient("http://"+addr, storehttp.ClientOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Release(context.Background(), store.Fence{Instance: "i", Owner: "o", Epoch: 1}); err == nil || store.IsDefinite(err) {
		t.Fatalf("Release to a closed port = %v, want indeterminate", err)
	}
	_ = c.Close()
	if _, err = c.Run(context.Background(), "r"); !errors.Is(err, store.ErrUnusable) {
		t.Fatalf("Run after Close = %v, want ErrUnusable", err)
	}
}

func TestConstructors(t *testing.T) {
	if _, err := storehttp.NewHandler(memstore.New(), storehttp.HandlerOptions{}); err == nil {
		t.Fatal("NewHandler without an Authorizer succeeded")
	}
	if _, err := storehttp.NewHandler(nil, storehttp.HandlerOptions{Authorizer: storehttp.AllowAll{}}); err == nil {
		t.Fatal("NewHandler without a store succeeded")
	}
	for _, u := range []string{"", "localhost:8080", "ftp://h/", "http:///x", "http://h/?q=1", "http://h/#f"} {
		if _, err := storehttp.NewClient(u, storehttp.ClientOptions{}); err == nil {
			t.Fatalf("NewClient(%q) succeeded", u)
		}
	}
	if _, err := storehttp.NewClient("https://h/base/", storehttp.ClientOptions{}); err != nil {
		t.Fatalf("NewClient with a base path: %v", err)
	}
}

// An empty identifier cannot be a path segment: the client refuses it as
// the store does.
func TestEmptyIdentifier(t *testing.T) {
	c := serve(t, memstore.New(), storehttp.HandlerOptions{})
	if _, err := c.Run(context.Background(), ""); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("Run(\"\") = %v, want ErrInvalid", err)
	}
}

// A 200 the handler did not send is never success (Codex round 1): a
// proxy's page, an empty body, or JSON without the handler's request ID or
// acknowledgement is indeterminate, for writes that return nothing and for
// calls that return a value.
func TestForeign200IsIndeterminate(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"html page": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<html>signed in</html>")
		},
		"empty body": func(http.ResponseWriter, *http.Request) {},
		"empty JSON without request ID": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{}")
		},
		"empty JSON with request ID": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Maestro-Request-Id", "r")
			_, _ = io.WriteString(w, "{}")
		},
		"HTML with request ID": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Maestro-Request-Id", "r")
			_, _ = io.WriteString(w, `{"applied":true}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)
			c, err := storehttp.NewClient(srv.URL, storehttp.ClientOptions{HTTPClient: srv.Client()})
			must(t, err)
			ctx := context.Background()
			f := store.Fence{Instance: "i", Owner: "o", Epoch: 1}
			content := []byte("a")
			for call, err := range map[string]error{
				"Commit":               c.Commit(ctx, f, []store.Op{store.ReleaseOp()}),
				"Commit with artifact": c.Commit(ctx, f, []store.Op{store.PutArtifactOp(store.Artifact{Digest: digest(content), MediaType: "text/plain", Content: content})}),
				"Release":              c.Release(ctx, f),
				"RemoveTrigger":        c.RemoveTrigger(ctx, "i", "t"),
				"AppendEvents":         c.AppendEvents(ctx, []store.Event{{Run: "r", Seq: 1, Kind: "k"}}),
			} {
				if err == nil || store.IsDefinite(err) {
					t.Fatalf("%s = %v, want indeterminate", call, err)
				}
			}
			if name != "empty JSON with request ID" {
				if _, err = c.Claim(ctx, "i", "o", time.Minute); err == nil || store.IsDefinite(err) {
					t.Fatalf("Claim = %v, want indeterminate", err)
				}
			}
		})
	}
}

// Health honours the client's timeout when the caller sets no deadline
// (Codex round 1).
func TestHealthTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	c, err := storehttp.NewClient(srv.URL, storehttp.ClientOptions{HTTPClient: srv.Client(), Timeout: 50 * time.Millisecond})
	must(t, err)
	start := time.Now()
	if _, err = c.Health(context.Background()); err == nil {
		t.Fatal("Health of a stalled server succeeded")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Health took %v despite a 50ms timeout", d)
	}
}

// A response over ClientOptions.MaxResponseBytes is indeterminate, for a
// call that returns a value and for one that returns only an
// acknowledgement; a response exactly at the limit is accepted (ADR 0026,
// item 5).
func TestMaxResponseBytes(t *testing.T) {
	const limit = 64
	var size int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Maestro-Request-Id", "r")
		body := `{"applied":true}`
		_, _ = io.WriteString(w, body+strings.Repeat(" ", size-len(body)))
	}))
	t.Cleanup(srv.Close)
	c, err := storehttp.NewClient(srv.URL, storehttp.ClientOptions{HTTPClient: srv.Client(), MaxResponseBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	size = limit
	if err = c.RemoveTrigger(ctx, "i", "t"); err != nil {
		t.Fatalf("RemoveTrigger at the limit = %v, want nil", err)
	}
	size = limit + 1
	if err = c.RemoveTrigger(ctx, "i", "t"); err == nil || store.IsDefinite(err) {
		t.Fatalf("RemoveTrigger over the limit = %v, want an indeterminate error", err)
	}
	if _, err = c.Claim(ctx, "i", "o", time.Minute); err == nil || store.IsDefinite(err) {
		t.Fatalf("Claim over the limit = %v, want an indeterminate error", err)
	}
}
