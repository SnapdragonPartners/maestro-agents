// Package mcptools adapts an MCP server's tools to agent.ToolSource (TOOL-6,
// ADR 0025, item 8), so an agent type's served tools run on a server reached
// through maestro-llms' mcpclient.
//
// A Source dials its server on first use, lists its tools, and runs each
// call with the call's run context, action identity and, for a write, its
// idempotency key in the request's MCP _meta (the keys below). It maps
// mcpclient's three classes of call error to the runtime's: a call never
// sent wraps agent.ErrSourceUnreachable, a call the server answered with an
// error wraps agent.ErrSourceError, and a call whose outcome is unknown
// wraps neither. After a failure that may have dropped the session, the
// next use dials again.
//
// The host builds a Source for a run's principal and owns it (ADR 0025,
// item 3): the credentials travel in the transport the dial function
// returns (an http.Client whose transport adds and renews the token), and
// the host closes the Source. The runtime never closes it.
//
// This is a nested module, v0, tagged mcptools/vX.Y.Z, so only programs
// that import it take on the MCP SDK.
package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
	"github.com/SnapdragonPartners/maestro-llms/mcp/mcpclient"

	"github.com/SnapdragonPartners/maestro-agents/agent"
)

// The _meta keys a Source sends with each tools/call request, for the server
// to tie the call to its run and claim (ADR 0025, item 5; docs/contract.md,
// section 7). Their values are JSON:
//
//   - MetaVersion: this key set's version, the integer MetaVersionValue;
//   - MetaAgentInstance: the agent instance, a string; it does not change
//     when the run resumes on another worker;
//   - MetaAgentType: the agent type, a string;
//   - MetaPrincipal: the run's principal, a string, always sent; the empty
//     string is a run under the host's own authority, by the host's
//     choice, never a dropped field;
//   - MetaOwner: the owner of the activation's claim, a string;
//   - MetaEpoch: the claim's owner epoch, an integer (an unsigned 64-bit
//     value, 1 or more): a server that keeps the store refuses a call
//     whose epoch is not the instance's current one (a server decoding
//     _meta into map[string]any reads it as a float64, exact below 2^53,
//     which an epoch, one per claim, never reaches in practice);
//   - MetaAction: the call's action identity,
//     {"scope":{"kind":"run","id":"<run ID>"},"step":N,"call":N}: the run
//     is its scope; step is 1-based and per run, call the 0-based index of
//     the call in the recorded model turn; unique within the run and the
//     same on every issue of the call;
//   - MetaIdempotencyKey: a write's idempotency key, a string, the same on
//     every issue of the call, whichever claim issues it; sent only for a
//     write.
//
// Every key but MetaIdempotencyKey is sent on every call, reads included.
// They are claims the host makes, not credentials: a server re-checks the
// principal against the token the request carries. The fence check is on
// the epoch alone: a newer claim's reissue carries a new owner and epoch
// and the same key, which the server deduplicates by.
const (
	MetaVersion        = "maestro-agents/v"
	MetaAgentInstance  = "maestro-agents/agent_instance"
	MetaAgentType      = "maestro-agents/agent_type"
	MetaPrincipal      = "maestro-agents/principal"
	MetaOwner          = "maestro-agents/owner"
	MetaEpoch          = "maestro-agents/epoch"
	MetaAction         = "maestro-agents/action"
	MetaIdempotencyKey = "maestro-agents/idempotency_key"
)

// MetaVersionValue is the version of the key set above that a Source sends
// as MetaVersion. A change a server must know of increments it.
const MetaVersionValue = 1

// Dial returns the transport for a new session to the server. A Source calls
// it on first use and after a failure that may have dropped the session, so
// it must return a fresh transport each time (mcpclient.Command needs a new,
// unstarted exec.Cmd). ctx is the context of the call or listing that needs
// the session: it bounds the handshake, and a session outlives it, so a
// local server's command must not be built with exec.CommandContext(ctx).
type Dial func(ctx context.Context) (mcpclient.Transport, error)

// Options configure a Source. The zero value lists every tool the server
// offers.
type Options struct {
	// Filter selects the server's tools by name; nil keeps every tool.
	// mcpclient fails a listing in which any kept tool can't be offered to
	// a model, so a host whose server offers tools its agent types don't
	// grant may keep only the granted ones.
	Filter func(name string) bool
}

// ErrClosed is wrapped by every error from a closed Source.
var ErrClosed = errors.New("mcptools: source closed")

// Source is an agent.ToolSource over one MCP server. Build one with New. It
// is safe for concurrent use.
type Source struct {
	dial Dial
	opts Options
	// gate (one slot) serializes dialing and listing, so concurrent first
	// uses dial once; a waiter gives up when its context ends. Calls on an
	// open session don't take it.
	gate chan struct{}
	// life ends when the Source closes; every dial, listing and call runs
	// under a context it also ends, so Close never waits on a server.
	life   context.Context //nolint:containedctx // the Source's own lifetime, not a request's: Close ends it
	end    context.CancelFunc
	cur    *conn              // the session in use; nil until dialed, or after a drop
	conns  map[*conn]struct{} // every session not yet closed, retired ones included
	mu     sync.Mutex
	closed bool
}

// conn is one session and the tools its last listing returned. A session
// found broken is retired: no new use takes it, and it is closed when the
// last use in flight ends, so retiring it cannot cut off a call that is
// still running on it.
type conn struct {
	session *mcpclient.Session
	tools   map[string]toolloop.Tool // by name; nil until listed
	uses    int
	retired bool
}

var _ agent.ToolSource = (*Source)(nil)

// New returns a Source that reaches its server through dial. It dials
// nothing until first used.
func New(dial Dial, opts Options) *Source {
	life, end := context.WithCancel(context.Background())
	return &Source{dial: dial, opts: opts, gate: make(chan struct{}, 1), life: life, end: end, conns: map[*conn]struct{}{}}
}

// bind returns ctx, also ended when the Source closes.
func (s *Source) bind(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.life, cancel)
	return ctx, func() { stop(); cancel() }
}

// Tools lists the server's tools and returns their definitions, in the
// server's order. A listing that fails retires the session (unless the
// server listed a tool that can't be offered, an *mcpclient.InvalidToolError
// in the chain), so the next use dials again.
func (s *Source) Tools(ctx context.Context) ([]llms.ToolDefinition, error) {
	ctx, done := s.bind(ctx)
	defer done()
	if err := s.enter(ctx); err != nil {
		return nil, err
	}
	defer s.leave()
	c, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	defer s.release(c)
	tools, err := s.list(ctx, c)
	if err != nil {
		return nil, err
	}
	defs := make([]llms.ToolDefinition, len(tools))
	for i := range tools {
		defs[i] = tools[i].Definition
	}
	return defs, nil
}

// Call runs one call on the server (ADR 0025, items 5 and 6). It wraps
// agent.ErrSourceUnreachable when the call was never sent (the server could
// not be reached, the session had ended, the source is closed) and
// agent.ErrSourceError when the server answered with an error or a
// malformed result, or does not offer the tool; any other error leaves the
// call's outcome unknown. A failure other than the server's answer retires
// the session, so the next use dials again.
func (s *Source) Call(ctx context.Context, call *agent.ServedCall) (toolloop.ToolResult, error) {
	if call == nil {
		return toolloop.ToolResult{}, fmt.Errorf("mcptools: %w: nil call", agent.ErrSourceUnreachable)
	}
	ctx, done := s.bind(ctx)
	defer done()
	name := call.ToolCall.Name
	if args := call.ToolCall.Parameters; len(args) > 0 && !json.Valid(args) {
		return toolloop.ToolResult{}, fmt.Errorf("mcptools: call %q: %w: the arguments are not valid JSON", name, agent.ErrSourceUnreachable)
	}
	c, tool, err := s.tool(ctx, name)
	if err != nil {
		return toolloop.ToolResult{}, err
	}
	defer s.release(c)
	res, err := tool.Execute(context.WithValue(ctx, callKey{}, call), call.ToolCall)
	if err == nil {
		return res, nil
	}
	switch {
	case errors.Is(err, mcpclient.ErrServerError):
		return toolloop.ToolResult{}, fmt.Errorf("mcptools: %w: %w", agent.ErrSourceError, err)
	case errors.Is(err, mcpclient.ErrNotSent):
		s.retire(ctx, c)
		return toolloop.ToolResult{}, fmt.Errorf("mcptools: %w: %w", agent.ErrSourceUnreachable, err)
	}
	s.retire(ctx, c)
	return toolloop.ToolResult{}, fmt.Errorf("mcptools: %w", err)
}

// tool returns the open session and its tool named name, holding a use of
// the session that the caller releases. It lists the tools if the session
// has no listing yet, or if its listing lacks the tool (the server may have
// added it since); a tool still missing wraps agent.ErrSourceError, as a
// server answers a call to a tool it lacks. Any failure to reach a session
// or list its tools wraps agent.ErrSourceUnreachable: nothing was sent.
func (s *Source) tool(ctx context.Context, name string) (*conn, toolloop.Tool, error) {
	if c, t, ok := s.listed(name); ok {
		return c, t, nil
	}
	if err := s.enter(ctx); err != nil {
		return nil, toolloop.Tool{}, fmt.Errorf("mcptools: call %q: %w: %w", name, agent.ErrSourceUnreachable, err)
	}
	defer s.leave()
	if c, t, ok := s.listed(name); ok { // another call listed while this one waited
		return c, t, nil
	}
	c, err := s.open(ctx)
	if err != nil {
		return nil, toolloop.Tool{}, fmt.Errorf("mcptools: call %q: %w: %w", name, agent.ErrSourceUnreachable, err)
	}
	tools, err := s.list(ctx, c)
	if err != nil {
		s.release(c)
		return nil, toolloop.Tool{}, fmt.Errorf("mcptools: call %q: %w: %w", name, agent.ErrSourceUnreachable, err)
	}
	for i := range tools {
		if tools[i].Definition.Name == name {
			return c, tools[i], nil
		}
	}
	s.release(c)
	return nil, toolloop.Tool{}, fmt.Errorf("mcptools: call %q: %w: the server does not offer the tool", name, agent.ErrSourceError)
}

// listed returns the current session's listed tool named name, holding a
// use of the session, if the session is open and its listing has the tool.
func (s *Source) listed(name string) (*conn, toolloop.Tool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cur
	if s.closed || c == nil || c.retired || c.tools == nil {
		return nil, toolloop.Tool{}, false
	}
	t, ok := c.tools[name]
	if !ok {
		return nil, toolloop.Tool{}, false
	}
	c.uses++
	return c, t, true
}

// enter takes the gate, giving up when ctx ends.
func (s *Source) enter(ctx context.Context) error {
	select {
	case s.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("mcptools: %w", ctx.Err())
	}
}

func (s *Source) leave() { <-s.gate }

// open returns the current session, dialing one if there is none, holding a
// use of it that the caller releases. The caller holds the gate.
func (s *Source) open(ctx context.Context) (*conn, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	if c := s.cur; c != nil && !c.retired {
		c.uses++
		s.mu.Unlock()
		return c, nil
	}
	s.mu.Unlock()
	session, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed { // Close ran while dialing
		s.mu.Unlock()
		_ = session.Close()
		return nil, ErrClosed
	}
	c := &conn{session: session, uses: 1}
	s.cur = c
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	return c, nil
}

func (s *Source) connect(ctx context.Context) (*mcpclient.Session, error) {
	if s.dial == nil {
		return nil, errors.New("mcptools: dial: no dial function")
	}
	t, err := s.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("mcptools: dial: %w", err)
	}
	session, err := mcpclient.Connect(ctx, t, mcpclient.Options{Filter: s.opts.Filter, Meta: meta})
	if err != nil {
		return nil, fmt.Errorf("mcptools: %w", err)
	}
	return session, nil
}

// list lists c's tools and keeps them for calls. A failure retires c,
// unless it is the server's tools that can't be offered. The caller holds
// the gate and a use of c.
func (s *Source) list(ctx context.Context, c *conn) ([]toolloop.Tool, error) {
	tools, err := c.session.Tools(ctx)
	if err != nil {
		if _, invalid := errors.AsType[*mcpclient.InvalidToolError](err); !invalid {
			s.retire(ctx, c)
		}
		return nil, fmt.Errorf("mcptools: %w", err)
	}
	byName := make(map[string]toolloop.Tool, len(tools))
	for i := range tools {
		byName[tools[i].Definition.Name] = tools[i]
	}
	s.mu.Lock()
	c.tools = byName
	s.mu.Unlock()
	return tools, nil
}

// retire stops new uses of c after a failure that may have dropped its
// session, so the next use dials again. A failure while ctx has ended is the
// caller's context, not the session, so it retires nothing.
func (s *Source) retire(ctx context.Context, c *conn) {
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	c.retired = true
	if s.cur == c {
		s.cur = nil
	}
	s.mu.Unlock()
}

// release ends a use of c, closing its session if c is retired and this
// was the last use.
func (s *Source) release(c *conn) {
	s.mu.Lock()
	c.uses--
	done := c.retired && c.uses == 0
	if done {
		delete(s.conns, c)
	}
	s.mu.Unlock()
	if done {
		_ = c.session.Close()
	}
}

// Close closes the Source and every session it has open, retired ones
// included. It first ends the context of every dial, listing and call in
// flight, so it never waits on a server: a call in flight ends with an
// error whose outcome is unknown. Every later use fails with an error
// wrapping ErrClosed (for Call, also agent.ErrSourceUnreachable). Close is
// idempotent.
func (s *Source) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.cur = nil
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		c.retired = true
		conns = append(conns, c)
	}
	clear(s.conns)
	s.mu.Unlock()
	s.end()
	var errs []error
	for _, c := range conns {
		if err := c.session.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("mcptools: %w", err)
	}
	return nil
}

type callKey struct{}

// meta is the sessions' Options.Meta: the _meta of the call Call put in
// ctx. A call without one, or without its run context or fence, fails
// before it is sent (mcpclient's ErrNotSent, so agent.ErrSourceUnreachable),
// so no call reaches the server without them.
func meta(ctx context.Context, _ llms.ToolCall) (map[string]any, error) { //nolint:gocritic // hugeParam: mcpclient.Options.Meta's signature
	call, ok := ctx.Value(callKey{}).(*agent.ServedCall)
	if !ok || call == nil {
		return nil, errors.New("mcptools: no served call in the context")
	}
	switch {
	case call.Run.Instance == "", call.Run.AgentType == "", call.Action.Scope.ID == "":
		return nil, errors.New("mcptools: the served call has no run context")
	case call.Fence.Owner == "" || call.Fence.Epoch == 0:
		return nil, errors.New("mcptools: the served call has no fence")
	}
	m := map[string]any{
		MetaVersion:       MetaVersionValue,
		MetaAgentInstance: call.Run.Instance,
		MetaAgentType:     call.Run.AgentType,
		MetaPrincipal:     call.Run.Principal,
		MetaOwner:         call.Fence.Owner,
		MetaEpoch:         call.Fence.Epoch,
		MetaAction:        call.Action,
	}
	if call.IdempotencyKey != "" {
		m[MetaIdempotencyKey] = call.IdempotencyKey
	}
	return m, nil
}
