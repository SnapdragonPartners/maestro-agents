// Package agenttest provides stand-ins for the runtime's host seams, so an
// agent type is testable without the services a host connects it to.
//
// Source stands in for a tool source (TOOL-6, ADR 0025, item 7): an MCP
// server serving an agent type's served tools, without a server or a
// network.
package agenttest

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"

	"github.com/SnapdragonPartners/maestro-agents/agent"
)

// Handler runs one call to a Source's tool.
type Handler func(ctx context.Context, call *agent.ServedCall) (toolloop.ToolResult, error)

// Source is a scripted agent.ToolSource: the tools added to it, each with
// its handler, and a log of every call it receives. Its zero value offers
// no tools. It is safe for concurrent use.
type Source struct {
	tools   map[string]sourceTool
	listErr error
	calls   []agent.ServedCall
	mu      sync.Mutex
}

type sourceTool struct {
	handler Handler
	def     llms.ToolDefinition
}

var _ agent.ToolSource = (*Source)(nil)

// Add offers a tool, replacing one of the same name (a changed definition,
// say), and returns s.
func (s *Source) Add(def llms.ToolDefinition, h Handler) *Source {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tools == nil {
		s.tools = map[string]sourceTool{}
	}
	def.InputSchema = append(json.RawMessage(nil), def.InputSchema...)
	if len(def.InputSchema) == 0 {
		def.InputSchema = nil
	}
	s.tools[def.Name] = sourceTool{def: def, handler: h}
	return s
}

// Remove stops offering the named tool. A call to it then fails with
// agent.ErrSourceError, as a server answers a call to a tool it lacks.
func (s *Source) Remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tools, name)
}

// FailList makes Tools fail with err; nil restores it.
func (s *Source) FailList(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listErr = err
}

// Calls returns the calls the source has received, in order.
func (s *Source) Calls() []agent.ServedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// Tools returns the offered definitions, sorted by name, as the go-sdk's
// server lists them.
func (s *Source) Tools(context.Context) ([]llms.ToolDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]llms.ToolDefinition, 0, len(s.tools))
	for _, t := range s.tools {
		d := t.def
		d.InputSchema = append(json.RawMessage(nil), d.InputSchema...)
		if len(d.InputSchema) == 0 {
			d.InputSchema = nil
		}
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b llms.ToolDefinition) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Call logs the call and runs the named tool's handler.
func (s *Source) Call(ctx context.Context, call *agent.ServedCall) (toolloop.ToolResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, *call)
	t, ok := s.tools[call.ToolCall.Name]
	s.mu.Unlock()
	if !ok {
		return toolloop.ToolResult{}, fmt.Errorf("%w: unknown tool %q", agent.ErrSourceError, call.ToolCall.Name)
	}
	if t.handler == nil {
		return toolloop.ToolResult{}, nil
	}
	return t.handler(ctx, call)
}

// Text returns a handler that answers every call with content.
func Text(content string) Handler {
	return func(context.Context, *agent.ServedCall) (toolloop.ToolResult, error) {
		return toolloop.ToolResult{Content: content}, nil
	}
}

// Fail returns a handler that fails every call with err: wrap
// agent.ErrSourceUnreachable or agent.ErrSourceError to classify it, or
// pass any other error for a call whose effect is unknown.
func Fail(err error) Handler {
	return func(context.Context, *agent.ServedCall) (toolloop.ToolResult, error) {
		return toolloop.ToolResult{}, err
	}
}

// Sequence returns a handler that runs hs in turn, one per call, and the
// last for every call after.
func Sequence(hs ...Handler) Handler {
	var (
		mu sync.Mutex
		n  int
	)
	return func(ctx context.Context, call *agent.ServedCall) (toolloop.ToolResult, error) {
		mu.Lock()
		h := hs[min(n, len(hs)-1)]
		n++
		mu.Unlock()
		return h(ctx, call)
	}
}
