package mcptools_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/mcp/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/mcptools"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

const stdioServerEnv = "MCPTOOLS_TEST_STDIO_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(stdioServerEnv) == "1" {
		if err := newServer(&serverState{}).Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			fmt.Fprintln(os.Stderr, "stdio server:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// serverState is shared by every session a test server opens.
type serverState struct {
	release chan struct{} // "block" waits on it
	started chan struct{} // "block" signals it has started
	extra   func(*mcp.Server)
	calls   atomic.Int32
}

// newServer builds a go-sdk server with the tools the tests call.
func newServer(st *serverState) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	obj := raw(`{"type":"object"}`)
	s.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "Echo the msg argument.",
		InputSchema: raw(`{"type":"object","properties":{"msg":{"type":"string"}},"required":["msg"]}`),
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		st.calls.Add(1)
		var args struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return text("echo: " + args.Msg), nil
	})
	s.AddTool(&mcp.Tool{Name: "meta", InputSchema: obj},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			b, err := json.Marshal(req.Params.Meta)
			if err != nil {
				return nil, err
			}
			return text(string(b)), nil
		})
	s.AddTool(&mcp.Tool{Name: "refuse", InputSchema: obj},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			res := text("no such document")
			res.IsError = true
			return res, nil
		})
	s.AddTool(&mcp.Tool{Name: "explode", InputSchema: obj},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, errors.New("handler exploded")
		})
	s.AddTool(&mcp.Tool{Name: "block", InputSchema: obj},
		func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			st.started <- struct{}{}
			select {
			case <-st.release:
				return text("released"), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
	if st.extra != nil {
		st.extra(s)
	}
	return s
}

// testServer is a Streamable HTTP server whose sessions share one MCP
// server, whose sessions can be forgotten (as a restarted server forgets
// them), and whose next tools/call can be cut off after it is read, with no
// answer.
type testServer struct {
	st      *serverState
	ts      *httptest.Server
	server  atomic.Pointer[mcp.Server]
	handler atomic.Pointer[http.Handler]
	last    atomic.Pointer[[]byte] // the last tools/call request body, as sent
	cut     atomic.Bool
}

func startServer(t *testing.T, st *serverState) *testServer {
	t.Helper()
	srv := &testServer{st: st}
	srv.restart()
	srv.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			isCall := bytes.Contains(body, []byte(`"tools/call"`))
			if isCall {
				srv.last.Store(&body)
			}
			if isCall && srv.cut.CompareAndSwap(true, false) {
				// The request arrived; the connection drops before an answer.
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Error("response writer is not a Hijacker")
					return
				}
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		(*srv.handler.Load()).ServeHTTP(w, r)
	}))
	t.Cleanup(srv.ts.Close)
	return srv
}

// restart replaces the server and the handler, so every session the server
// had is unknown to it (a 404, as after a server restart).
func (srv *testServer) restart() {
	srv.server.Store(newServer(srv.st))
	var h http.Handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.server.Load() }, nil)
	srv.handler.Store(&h)
}

// dial returns a Dial to the server that counts its dials.
func (srv *testServer) dial(n *atomic.Int32) mcptools.Dial {
	return func(context.Context) (mcpclient.Transport, error) {
		n.Add(1)
		return mcpclient.StreamableHTTP(srv.ts.URL, srv.ts.Client()), nil
	}
}

func newSource(t *testing.T, d mcptools.Dial, opts mcptools.Options) *mcptools.Source {
	t.Helper()
	s := mcptools.New(d, opts)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

var runCtx = agent.RunContext{
	Scope:     store.Scope{Kind: store.ScopeRun, ID: "run-1"},
	Instance:  "inst-1",
	AgentType: "intake",
	Principal: "alice",
}

func served(name, args string) *agent.ServedCall {
	action := agent.ActionID{Scope: runCtx.Scope, Step: 3, Call: 1}
	return &agent.ServedCall{
		Run:      runCtx,
		Fence:    store.Fence{Instance: runCtx.Instance, Owner: "worker-7", Epoch: 1 << 60},
		ToolCall: llms.ToolCall{ID: "c1", Name: name, Parameters: raw(args)},
		Action:   action,
	}
}

// class returns the runtime's class for a Call error.
func class(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, agent.ErrSourceUnreachable) && errors.Is(err, agent.ErrSourceError):
		return "both"
	case errors.Is(err, agent.ErrSourceUnreachable):
		return "unreachable"
	case errors.Is(err, agent.ErrSourceError):
		return "source_error"
	}
	return "unknown"
}

func wantClass(t *testing.T, err error, want string) {
	t.Helper()
	if got := class(err); got != want {
		t.Fatalf("Call error %v: class %s, want %s", err, got, want)
	}
}

func TestToolsAndCall(t *testing.T) {
	srv := startServer(t, &serverState{})
	var dials atomic.Int32
	src := newSource(t, srv.dial(&dials), mcptools.Options{})
	ctx := context.Background()

	defs, err := src.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var echo *llms.ToolDefinition
	for i := range defs {
		if defs[i].Name == "echo" {
			echo = &defs[i]
		}
	}
	if echo == nil || echo.Description != "Echo the msg argument." || !strings.Contains(string(echo.InputSchema), `"msg"`) {
		t.Fatalf("Tools = %+v", defs)
	}

	res, err := src.Call(ctx, served("echo", `{"msg":"hi"}`))
	if err != nil || res.Content != "echo: hi" || res.IsError {
		t.Fatalf("Call = %+v, %v", res, err)
	}

	// A result with isError is the server's tool telling the model: no error.
	res, err = src.Call(ctx, served("refuse", `{}`))
	if err != nil || !res.IsError || res.Content != "no such document" {
		t.Fatalf("Call(refuse) = %+v, %v", res, err)
	}
	if n := dials.Load(); n != 1 {
		t.Errorf("dials = %d, want 1", n)
	}
}

func TestCallListsOnFirstUse(t *testing.T) {
	// A Call before any Tools dials and lists itself.
	srv := startServer(t, &serverState{})
	var dials atomic.Int32
	src := newSource(t, srv.dial(&dials), mcptools.Options{})
	res, err := src.Call(context.Background(), served("echo", `{"msg":"x"}`))
	if err != nil || res.Content != "echo: x" {
		t.Fatalf("Call = %+v, %v", res, err)
	}
}

func metaOf(t *testing.T, src *mcptools.Source, call *agent.ServedCall) map[string]json.RawMessage {
	t.Helper()
	res, err := src.Call(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(res.Content), &m); err != nil {
		t.Fatalf("meta %q: %v", res.Content, err)
	}
	for k := range m {
		switch {
		case strings.HasPrefix(k, "io.modelcontextprotocol/"):
			delete(m, k) // the SDK's own
		case !strings.HasPrefix(k, "maestro-agents/"):
			t.Errorf("unexpected _meta key %q", k)
		}
	}
	return m
}

// sameJSON reports whether got and want encode the same JSON value.
func sameJSON(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(g, w)
}

func TestMeta(t *testing.T) {
	srv := startServer(t, &serverState{})
	src := newSource(t, srv.dial(new(atomic.Int32)), mcptools.Options{})

	t.Run("write", func(t *testing.T) {
		call := served("meta", `{}`)
		call.IdempotencyKey = call.Action.IdempotencyKey()
		m := metaOf(t, src, call)
		want := map[string]string{
			mcptools.MetaVersion:        `1`,
			mcptools.MetaAgentInstance:  `"inst-1"`,
			mcptools.MetaAgentType:      `"intake"`,
			mcptools.MetaPrincipal:      `"alice"`,
			mcptools.MetaOwner:          `"worker-7"`,
			mcptools.MetaAction:         `{"scope":{"kind":"run","id":"run-1"},"step":3,"call":1}`,
			mcptools.MetaIdempotencyKey: `"` + call.IdempotencyKey + `"`,
		}
		for k, v := range want {
			if !sameJSON(t, m[k], v) {
				t.Errorf("_meta[%q] = %s, want %s", k, m[k], v)
			}
		}
		// The epoch is a JSON integer on the wire, exact beyond float64's
		// range. (The go-sdk server decodes _meta into map[string]any, so
		// the echo above sees a float64.)
		var req struct {
			Params struct {
				Meta map[string]json.RawMessage `json:"_meta"`
			} `json:"params"`
		}
		if err := json.Unmarshal(*srv.last.Load(), &req); err != nil {
			t.Fatal(err)
		}
		if got := string(req.Params.Meta[mcptools.MetaEpoch]); got != "1152921504606846976" {
			t.Errorf("sent _meta[%q] = %s, want the integer 2^60", mcptools.MetaEpoch, got)
		}
		if len(m) != len(want)+1 {
			t.Errorf("_meta has %d of our keys, want %d: %v", len(m), len(want)+1, m)
		}
	})
	t.Run("read by the host's principal", func(t *testing.T) {
		call := served("meta", `{}`)
		call.Run.Principal = ""
		m := metaOf(t, src, call)
		if v, ok := m[mcptools.MetaIdempotencyKey]; ok {
			t.Errorf("a read sent an idempotency key %s", v)
		}
		// The principal is always sent: empty is the host's own, explicitly.
		if v, ok := m[mcptools.MetaPrincipal]; !ok || string(v) != `""` {
			t.Errorf("_meta[%q] = %s (present %v), want \"\"", mcptools.MetaPrincipal, v, ok)
		}
		for _, k := range []string{mcptools.MetaOwner, mcptools.MetaEpoch, mcptools.MetaAgentInstance, mcptools.MetaVersion} {
			if _, ok := m[k]; !ok {
				t.Errorf("a read lacks _meta[%q]", k)
			}
		}
	})
	t.Run("missing context is never sent", func(t *testing.T) {
		for name, mutate := range map[string]func(*agent.ServedCall){
			"no owner":    func(c *agent.ServedCall) { c.Fence.Owner = "" },
			"no epoch":    func(c *agent.ServedCall) { c.Fence.Epoch = 0 },
			"no instance": func(c *agent.ServedCall) { c.Run.Instance = "" },
			"no type":     func(c *agent.ServedCall) { c.Run.AgentType = "" },
			"no run":      func(c *agent.ServedCall) { c.Action.Scope.ID = "" },
		} {
			t.Run(name, func(t *testing.T) {
				before := srv.st.calls.Load()
				call := served("echo", `{"msg":"x"}`)
				mutate(call)
				_, err := src.Call(context.Background(), call)
				wantClass(t, err, "unreachable")
				if srv.st.calls.Load() != before {
					t.Error("the call reached the server")
				}
			})
		}
	})
}

func TestServerErrors(t *testing.T) {
	st := &serverState{}
	srv := startServer(t, st)
	var dials atomic.Int32
	src := newSource(t, srv.dial(&dials), mcptools.Options{})
	ctx := context.Background()

	_, err := src.Call(ctx, served("explode", `{}`))
	wantClass(t, err, "source_error")
	if !errors.Is(err, mcpclient.ErrServerError) || !strings.Contains(err.Error(), "handler exploded") {
		t.Errorf("error %q: want the client's server error kept", err)
	}

	// A tool the server does not offer is never sent.
	before := st.calls.Load()
	_, err = src.Call(ctx, served("missing", `{}`))
	wantClass(t, err, "source_error")
	if st.calls.Load() != before {
		t.Error("a call to a tool the server lacks reached the server")
	}

	// The server's answer does not retire the session.
	if _, err := src.Call(ctx, served("echo", `{"msg":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if n := dials.Load(); n != 1 {
		t.Errorf("dials = %d, want 1: a server's error retired the session", n)
	}
}

func TestInvalidArgumentsNotSent(t *testing.T) {
	var dials atomic.Int32
	src := newSource(t, func(context.Context) (mcpclient.Transport, error) {
		dials.Add(1)
		return mcpclient.Transport{}, errors.New("unused")
	}, mcptools.Options{})
	_, err := src.Call(context.Background(), served("echo", `{"msg":`))
	wantClass(t, err, "unreachable")
	if dials.Load() != 0 {
		t.Error("invalid arguments dialed the server")
	}
	_, err = src.Call(context.Background(), nil)
	wantClass(t, err, "unreachable")
}

func TestUnreachable(t *testing.T) {
	ctx := context.Background()
	t.Run("dial fails", func(t *testing.T) {
		src := newSource(t, func(context.Context) (mcpclient.Transport, error) {
			return mcpclient.Transport{}, errors.New("no route")
		}, mcptools.Options{})
		_, err := src.Call(ctx, served("echo", `{"msg":"x"}`))
		wantClass(t, err, "unreachable")
		if _, err := src.Tools(ctx); err == nil || !strings.Contains(err.Error(), "no route") {
			t.Errorf("Tools error = %v", err)
		}
	})
	t.Run("server down", func(t *testing.T) {
		srv := startServer(t, &serverState{})
		srv.ts.Close()
		src := newSource(t, srv.dial(new(atomic.Int32)), mcptools.Options{})
		_, err := src.Call(ctx, served("echo", `{"msg":"x"}`))
		wantClass(t, err, "unreachable")
	})
	t.Run("nil dial", func(t *testing.T) {
		src := newSource(t, nil, mcptools.Options{})
		_, err := src.Call(ctx, served("echo", `{"msg":"x"}`))
		wantClass(t, err, "unreachable")
	})
}

func TestRedialAfterRestart(t *testing.T) {
	srv := startServer(t, &serverState{})
	var dials atomic.Int32
	src := newSource(t, srv.dial(&dials), mcptools.Options{})
	ctx := context.Background()
	if _, err := src.Call(ctx, served("echo", `{"msg":"1"}`)); err != nil {
		t.Fatal(err)
	}

	srv.restart()
	// The session is gone: the call may have been sent, so its outcome is
	// unknown, never unreachable.
	_, err := src.Call(ctx, served("echo", `{"msg":"2"}`))
	wantClass(t, err, "unknown")

	res, err := src.Call(ctx, served("echo", `{"msg":"3"}`))
	if err != nil || res.Content != "echo: 3" {
		t.Fatalf("Call after the restart = %+v, %v", res, err)
	}
	if n := dials.Load(); n != 2 {
		t.Errorf("dials = %d, want 2", n)
	}
}

func TestDroppedMidCallIsUnknown(t *testing.T) {
	st := &serverState{}
	srv := startServer(t, st)
	var dials atomic.Int32
	src := newSource(t, srv.dial(&dials), mcptools.Options{})
	ctx := context.Background()
	if _, err := src.Tools(ctx); err != nil {
		t.Fatal(err)
	}
	srv.cut.Store(true)
	_, err := src.Call(ctx, served("echo", `{"msg":"x"}`))
	wantClass(t, err, "unknown")
	if !errors.Is(err, mcpclient.ErrOutcomeUnknown) {
		t.Errorf("error %q: want the client's unknown outcome kept", err)
	}
	if _, err := src.Call(ctx, served("echo", `{"msg":"y"}`)); err != nil {
		t.Fatal(err)
	}
	if n := dials.Load(); n != 2 {
		t.Errorf("dials = %d, want 2: an unknown outcome retires the session", n)
	}
}

func TestRetiredSessionOutlivesCallsInFlight(t *testing.T) {
	// A call running when another call's failure retires the session runs
	// to its end: retiring closes the session only after its last use.
	st := &serverState{release: make(chan struct{}), started: make(chan struct{}, 1)}
	srv := startServer(t, st)
	var dials atomic.Int32
	src := newSource(t, srv.dial(&dials), mcptools.Options{})
	ctx := context.Background()
	if _, err := src.Tools(ctx); err != nil {
		t.Fatal(err)
	}

	type result struct {
		err     error
		content string
	}
	done := make(chan result, 1)
	go func() {
		res, err := src.Call(ctx, served("block", `{}`))
		done <- result{content: res.Content, err: err}
	}()
	<-st.started

	srv.cut.Store(true)
	_, err := src.Call(ctx, served("echo", `{"msg":"x"}`))
	wantClass(t, err, "unknown")

	close(st.release)
	r := <-done
	if r.err != nil || r.content != "released" {
		t.Fatalf("the call in flight = %q, %v; want it to finish on the retired session", r.content, r.err)
	}
	if _, err := src.Call(ctx, served("echo", `{"msg":"y"}`)); err != nil {
		t.Fatal(err)
	}
	if n := dials.Load(); n != 2 {
		t.Errorf("dials = %d, want 2", n)
	}
}

func TestCancelledContextRetiresNothing(t *testing.T) {
	st := &serverState{release: make(chan struct{}), started: make(chan struct{}, 1)}
	srv := startServer(t, st)
	var dials atomic.Int32
	src := newSource(t, srv.dial(&dials), mcptools.Options{})
	if _, err := src.Tools(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-st.started
		cancel()
	}()
	_, err := src.Call(ctx, served("block", `{}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Call error = %v, want the context's", err)
	}
	if _, err := src.Call(context.Background(), served("echo", `{"msg":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if n := dials.Load(); n != 1 {
		t.Errorf("dials = %d, want 1: the caller's cancellation retired the session", n)
	}
}

func TestInvalidToolAndFilter(t *testing.T) {
	st := &serverState{extra: func(s *mcp.Server) {
		// A tool mcpclient cannot offer: its schema is not an object.
		s.AddTool(&mcp.Tool{Name: "bad", InputSchema: raw(`{"type":"object","properties":{"x":{"$ref":"https://example.com/x"}}}`)},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return text("bad"), nil })
	}}
	srv := startServer(t, st)
	ctx := context.Background()

	var dials atomic.Int32
	src := newSource(t, srv.dial(&dials), mcptools.Options{})
	_, err := src.Tools(ctx)
	if _, ok := errors.AsType[*mcpclient.InvalidToolError](err); !ok {
		t.Fatalf("Tools error = %v, want an InvalidToolError", err)
	}
	_, _ = src.Tools(ctx)
	if n := dials.Load(); n != 1 {
		t.Errorf("dials = %d, want 1: a tool that can't be offered retired the session", n)
	}

	filtered := newSource(t, srv.dial(new(atomic.Int32)), mcptools.Options{Filter: func(name string) bool { return name != "bad" }})
	defs, err := filtered.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range defs {
		if d.Name == "bad" {
			t.Error("Filter kept the filtered tool")
		}
	}
}

func TestToolAddedAfterListing(t *testing.T) {
	// A Call for a tool the session's listing lacks lists again on the same
	// session, and a tool still missing is the server's error.
	srv := startServer(t, &serverState{})
	ctx := context.Background()
	var dials atomic.Int32
	src := newSource(t, srv.dial(&dials), mcptools.Options{})
	if _, err := src.Tools(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := src.Call(ctx, served("late", `{}`))
	wantClass(t, err, "source_error")

	srv.server.Load().AddTool(&mcp.Tool{Name: "late", InputSchema: raw(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return text("late"), nil })
	// The client learns of the change from the server's list-changed
	// notification, which arrives asynchronously.
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := src.Call(ctx, served("late", `{}`))
		if err == nil {
			if res.Content != "late" {
				t.Fatalf("Call(late) = %+v", res)
			}
			break
		}
		wantClass(t, err, "source_error")
		if time.Now().After(deadline) {
			t.Fatalf("the added tool was never listed: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := dials.Load(); n != 1 {
		t.Errorf("dials = %d, want 1", n)
	}
}

func TestClose(t *testing.T) {
	srv := startServer(t, &serverState{})
	src := mcptools.New(srv.dial(new(atomic.Int32)), mcptools.Options{})
	ctx := context.Background()
	if _, err := src.Call(ctx, served("echo", `{"msg":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	_, err := src.Call(ctx, served("echo", `{"msg":"x"}`))
	wantClass(t, err, "unreachable")
	if !errors.Is(err, mcptools.ErrClosed) {
		t.Errorf("error %q: want ErrClosed", err)
	}
	if _, err := src.Tools(ctx); !errors.Is(err, mcptools.ErrClosed) {
		t.Errorf("Tools error = %v, want ErrClosed", err)
	}
	// Closing a source never used dials nothing.
	if err := mcptools.New(nil, mcptools.Options{}).Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentFirstUseDialsOnce(t *testing.T) {
	srv := startServer(t, &serverState{})
	var dials atomic.Int32
	src := newSource(t, srv.dial(&dials), mcptools.Options{})
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Go(func() {
			if i%2 == 0 {
				_, err := src.Tools(context.Background())
				errs <- err
				return
			}
			res, err := src.Call(context.Background(), served("echo", fmt.Sprintf(`{"msg":"%d"}`, i)))
			if err == nil && res.Content != fmt.Sprintf("echo: %d", i) {
				err = fmt.Errorf("call %d got %q", i, res.Content)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if n := dials.Load(); n != 1 {
		t.Errorf("dials = %d, want 1", n)
	}
}

func TestQueuedUseHonorsContext(t *testing.T) {
	// A use waiting for another's dial gives up when its context ends.
	entered := make(chan struct{})
	unblock := make(chan struct{})
	src := newSource(t, func(context.Context) (mcpclient.Transport, error) {
		close(entered)
		<-unblock
		return mcpclient.Transport{}, errors.New("gave up")
	}, mcptools.Options{})
	go func() { _, _ = src.Tools(context.Background()) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := src.Call(ctx, served("echo", `{"msg":"x"}`))
	wantClass(t, err, "unreachable")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %q: want the context's", err)
	}
	close(unblock)
}

func TestStdio(t *testing.T) {
	// A local server: each dial starts a fresh process, so a source
	// redials after the process exits.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int32
	var mu sync.Mutex
	var cmds []*exec.Cmd
	src := newSource(t, func(context.Context) (mcpclient.Transport, error) {
		dials.Add(1)
		cmd := exec.Command(exe, "-test.run=^$") //nolint:gosec // the test binary itself
		cmd.Env = append(os.Environ(), stdioServerEnv+"=1")
		mu.Lock()
		cmds = append(cmds, cmd)
		mu.Unlock()
		return mcpclient.Command(cmd), nil
	}, mcptools.Options{})
	ctx := context.Background()
	res, err := src.Call(ctx, served("echo", `{"msg":"local"}`))
	if err != nil || res.Content != "echo: local" {
		t.Fatalf("Call = %+v, %v", res, err)
	}

	mu.Lock()
	_ = cmds[0].Process.Kill()
	mu.Unlock()
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err = src.Call(ctx, served("echo", `{"msg":"again"}`))
		if err == nil {
			break
		}
		if c := class(err); c != "unknown" && c != "unreachable" {
			t.Fatalf("Call after the process died: %v (%s)", err, c)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no redial: %v", err)
		}
	}
	if res.Content != "echo: again" || dials.Load() != 2 {
		t.Fatalf("Call = %+v after %d dials, want a second process", res, dials.Load())
	}
}
