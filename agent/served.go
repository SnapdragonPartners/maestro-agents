package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// ToolSource serves tools to a run from outside the process (TOOL-6, ADR
// 0025, item 3): an MCP server, say, adapted by the host. The host builds
// it for the run's principal and owns it: its transport, its token and the
// token's renewal, reconnecting after a dropped session, and closing it.
// The runtime never closes a source. It must be safe for concurrent use.
type ToolSource interface {
	// Tools lists the definitions the source offers now.
	Tools(ctx context.Context) ([]llms.ToolDefinition, error)
	// Call runs one call and returns what the model receives. A result
	// with IsError is model-visible. An error is classified by what it
	// wraps (ADR 0025, item 6): ErrSourceUnreachable for a call never
	// sent, ErrSourceError for a call the server answered with an error;
	// any other error leaves the call's effect unknown. A source unsure
	// whether a call was sent must not report it unreachable.
	Call(ctx context.Context, call *ServedCall) (toolloop.ToolResult, error)
}

// ServedCall is one call to a served tool, with what the host and server
// need to tie it to the run (ADR 0025, item 5).
type ServedCall struct {
	// Run is the call's run context (TOOL-8): the server re-checks the
	// principal.
	Run RunContext
	// IdempotencyKey is a write's idempotency key (TOOL-5), stable across
	// every issue of the call; a read's is empty.
	IdempotencyKey string
	// ToolCall is the call as the model emitted it.
	ToolCall llms.ToolCall
	// Action is the call's action identity (ADR 0003, item 4).
	Action ActionID
	// WrapUp reports that the call is in a wrap-up turn (ADR 0010).
	WrapUp bool
}

// The errors a ToolSource's Call wraps to classify a failure (ADR 0025,
// item 6).
var (
	// ErrSourceUnreachable: the call was never sent, so it had no effect.
	ErrSourceUnreachable = errors.New("agent: tool source unreachable")
	// ErrSourceError: the server answered the call with an error, or with
	// a result the source or the runtime finds malformed.
	ErrSourceError = errors.New("agent: tool source error")
)

// Failure classifies a served call's failure (ADR 0025, item 6), as
// ResultEntry.Failure and the result event's ErrorKind give it.
type Failure string

// The failure classes.
const (
	// FailureUnreachable: the call was never sent.
	FailureUnreachable Failure = "unreachable"
	// FailureSourceError: the server answered with an error.
	FailureSourceError Failure = "source_error"
	// FailureUnknown: the call may have been sent and was not answered.
	FailureUnknown Failure = "unknown"
)

// classify returns a served call's failure class.
func classify(err error) Failure {
	switch {
	case errors.Is(err, ErrSourceUnreachable):
		return FailureUnreachable
	case errors.Is(err, ErrSourceError):
		return FailureSourceError
	}
	return FailureUnknown
}

// failureContent is what the model receives for a served call that failed
// and that it may try again: a fixed message per class, so nothing from
// the error reaches the model or the record unredacted.
func failureContent(name string, f Failure) string {
	switch f {
	case FailureUnreachable:
		return fmt.Sprintf("tool %q could not be reached; the call was not made", name)
	case FailureSourceError:
		return fmt.Sprintf("tool %q failed", name)
	}
	return fmt.Sprintf("tool %q did not answer", name)
}

// CheckToolSources checks the host's tool sources for a run of agentType
// without a run (ADR 0025, item 4): the check a run's start makes, by the
// same code path. Each source the type declares must be present, list
// its tools, and offer every tool the type grants from it. It returns
// every problem it finds, in a stable order, and none for a type with no
// served tools. Like CheckStart it is advice: the start checks again.
func (r *Registry) CheckToolSources(ctx context.Context, agentType string, sources map[string]ToolSource) []StartProblem {
	m, ok := r.lookup(agentType)
	if !ok {
		return []StartProblem{{Check: CheckAgentType, Detail: fmt.Sprintf("agent type %q is not registered", agentType)}}
	}
	_, ps := m.listServed(ctx, sources)
	return ps
}

// listServed lists the type's tool sources and returns the definitions of
// the served tools it grants, by name, or the problems it found.
func (m *machine) listServed(ctx context.Context, sources map[string]ToolSource) (map[string]llms.ToolDefinition, []StartProblem) {
	if len(m.sources) == 0 {
		return nil, nil
	}
	var ps []StartProblem
	problem := func(format string, args ...any) {
		ps = append(ps, StartProblem{Check: CheckToolSources, Reason: ReasonConfigError, Detail: fmt.Sprintf(format, args...)})
	}
	defs := map[string]llms.ToolDefinition{}
	for _, name := range m.sources {
		src := sources[name]
		if src == nil {
			problem("the host provides no tool source %q", name)
			continue
		}
		var listed []llms.ToolDefinition
		err := guard(func() (err error) {
			listed, err = src.Tools(ctx)
			return err //nolint:wrapcheck // the error becomes the problem's detail, prefixed with the source
		})
		if err != nil {
			problem("tool source %q: list tools: %v", name, err)
			continue
		}
		offered := map[string]llms.ToolDefinition{}
		for i := range listed {
			d := listed[i]
			if _, dup := offered[d.Name]; dup {
				problem("tool source %q lists tool %q twice", name, d.Name)
			}
			offered[d.Name] = d
		}
		for _, tool := range m.servedFrom(name) {
			d, ok := offered[tool]
			switch {
			case !ok:
				problem("tool source %q does not offer tool %q, which agent type %q grants", name, tool, m.name)
				continue
			case !utf8.ValidString(d.Description):
				problem("tool source %q: tool %q description is not valid UTF-8", name, tool)
				continue
			}
			if d.InputSchema != nil {
				if err := store.ValidJSON(d.InputSchema); err != nil {
					problem("tool source %q: tool %q input schema: %v", name, tool, err)
					continue
				}
			}
			nd, err := normalizeDef(llms.ToolDefinition{Name: tool, Description: d.Description, InputSchema: d.InputSchema})
			if err != nil {
				problem("tool source %q: tool %q: %v", name, tool, err)
				continue
			}
			defs[tool] = nd
		}
	}
	if len(ps) > 0 {
		return nil, ps
	}
	return defs, nil
}

// normalizeDef returns a definition in the form the record restores it
// in: encoded and decoded as the served artifact is (which escapes <, >
// and & and writes a nil schema as null), the schema compacted, and a
// null schema nil. A listing and a restored record compare equal exactly
// when their definitions do (ADR 0025, item 4).
func normalizeDef(d llms.ToolDefinition) (llms.ToolDefinition, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return d, fmt.Errorf("encode definition: %w", err)
	}
	var out llms.ToolDefinition
	if err := json.Unmarshal(raw, &out); err != nil {
		return d, fmt.Errorf("decode definition: %w", err)
	}
	out.InputSchema = compactJSON(out.InputSchema)
	if string(out.InputSchema) == "null" {
		out.InputSchema = nil
	}
	return out, nil
}

// servedFrom returns the served tools the type grants from a source,
// sorted.
func (m *machine) servedFrom(source string) []string {
	var out []string
	for tool, s := range m.served {
		if s == source {
			out = append(out, tool)
		}
	}
	slices.Sort(out)
	return out
}

// servedArtifact encodes the run's served definitions, sorted by name,
// as the artifact run_started and the checkpoint name (ADR 0025, item 4).
func servedArtifact(defs map[string]llms.ToolDefinition) (*store.Artifact, error) {
	list := make([]llms.ToolDefinition, 0, len(defs))
	for _, d := range defs {
		list = append(list, d)
	}
	slices.SortFunc(list, func(a, b llms.ToolDefinition) int { return strings.Compare(a.Name, b.Name) })
	raw, err := json.Marshal(list)
	if err != nil {
		return nil, fmt.Errorf("agent: encode served tool definitions: %w", err)
	}
	sum := sha256.Sum256(raw)
	return &store.Artifact{Digest: hex.EncodeToString(sum[:]), MediaType: "application/json", Content: raw}, nil
}

// startServed lists the tool sources at the run's start, refusing the
// start with config_error for any problem, and prepares the definitions'
// artifact for the start's commit. It returns nil if the type has no
// served tools or the sources pass.
func (a *activation) startServed(ctx context.Context, start *runStart) *ending {
	if len(a.m.sources) == 0 {
		return nil
	}
	defs, ps := a.m.listServed(ctx, a.sources)
	if ctx.Err() != nil {
		return a.endByCause(ctx)
	}
	if e := a.refuseStart(ctx, ps); e != nil {
		return e
	}
	art, err := servedArtifact(defs)
	if err != nil {
		return &ending{err: storeFailure(err)}
	}
	start.served, start.entry.Served, a.served = art, art.Digest, defs
	return nil
}

// restoreServed reads the served definitions recorded at the run's start.
func (a *activation) restoreServed(ctx context.Context) error {
	if a.cs.Served == "" {
		return nil
	}
	_, raw, err := a.c.store.Artifact(durable(ctx), a.cs.Served)
	if err != nil {
		return storeFailure(fmt.Errorf("agent: read served tool definitions %s: %w", a.cs.Served, err))
	}
	var list []llms.ToolDefinition
	if err := json.Unmarshal(raw, &list); err != nil {
		return storeFailure(fmt.Errorf("agent: decode served tool definitions %s: %w", a.cs.Served, err))
	}
	a.served = make(map[string]llms.ToolDefinition, len(list))
	for _, d := range list {
		nd, err := normalizeDef(d)
		if err != nil {
			return storeFailure(fmt.Errorf("agent: served tool definition %q: %w", d.Name, err))
		}
		a.served[nd.Name] = nd
	}
	return nil
}

// verifyServed checks, at a later activation's start, that the tool
// sources still offer each granted served tool as the run's start
// recorded it (ADR 0025, item 4): the model, the policy and the record
// saw that definition. Anything else fails the run with tool_error.
func (a *activation) verifyServed(ctx context.Context) *ending {
	if len(a.m.sources) == 0 {
		return nil
	}
	now, ps := a.m.listServed(ctx, a.sources)
	if ctx.Err() != nil {
		return a.endByCause(ctx)
	}
	if len(ps) > 0 {
		details := make([]string, len(ps))
		for i, p := range ps {
			details[i] = p.Detail
		}
		return a.fail(ctx, ReasonToolError, strings.Join(details, "; "))
	}
	names := make([]string, 0, len(now))
	for name := range now {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		rec, ok := a.served[name]
		cur := now[name]
		switch {
		case !ok:
			return a.fail(ctx, ReasonToolError, fmt.Sprintf(
				"served tool %q has no definition recorded at the run's start (ADR 0025, item 4)", name))
		case rec.Description != cur.Description || !bytes.Equal(rec.InputSchema, cur.InputSchema):
			return a.fail(ctx, ReasonToolError, fmt.Sprintf(
				"tool source %q changed the definition of tool %q since the run started (ADR 0025, item 4)", a.m.served[name], name))
		}
	}
	return nil
}

// def returns the definition a tool is offered with: a served tool's is
// the one recorded at the run's start.
func (r *modelRun) def(t *erasedTool) llms.ToolDefinition {
	if t.source != "" {
		return r.a.served[t.def.Name]
	}
	return t.def
}

// definitions returns the tool definitions the state offers, in order.
func (r *modelRun) definitions() []llms.ToolDefinition {
	defs := make([]llms.ToolDefinition, len(r.ms.tools))
	for i, t := range r.ms.tools {
		defs[i] = r.def(t)
	}
	return defs
}

// runServed executes an attempted call to a served tool through its
// source and commits its result, classifying a failure (ADR 0025, items 5
// and 6).
func (r *modelRun) runServed(ctx context.Context, i int, tool *erasedTool, at *AttemptEntry) (*resultRecord, bool, error) {
	a, t := r.a, r.turn
	call := t.calls[i]
	src := a.sources[tool.source]
	var out toolloop.ToolResult
	err := guard(func() (err error) {
		if src == nil {
			return fmt.Errorf("%w: the host provides no tool source %q", ErrSourceUnreachable, tool.source)
		}
		out, err = src.Call(ctx, &ServedCall{
			ToolCall: cloneCalls([]llms.ToolCall{call})[0], Run: a.runContext(), Action: at.Action, IdempotencyKey: at.Key, WrapUp: t.wrapUp,
		})
		return err //nolint:wrapcheck // classified by what it wraps, then recorded with the source's name
	})
	// A read's result after the context ended is discarded, and recovery
	// issues it again (STM-6).
	if ctx.Err() != nil && tool.reissuable() {
		return nil, false, ctx.Err() //nolint:wrapcheck // toolloop classifies the context's error
	}
	res := erasedResult{content: out.Content, parts: out.Parts, isError: out.IsError}
	if err == nil {
		if cerr := checkParts(res.content, res.parts); cerr != nil {
			err = fmt.Errorf("%w: malformed result: %w", ErrSourceError, cerr)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			// A write interrupted by the context's end: its attempt has
			// no result, so recovery resolves it (ADR 0008, item 18).
			r.abort = a.endByCause(ctx)
			return nil, false, errAbort
		}
		f := classify(err)
		a.c.logger.Warn("agent: served tool call failed", "run", a.run.ID, "tool", call.Name, "source", tool.source, "failure", string(f))
		if tool.effect == EffectWrite && f != FailureUnreachable {
			status := ResultUnknown
			if f == FailureSourceError {
				status = ResultExecuted
			}
			r.abort = r.failCall(ctx, i, at, status, f, failureContent(call.Name, f), fmt.Sprintf(
				"call %d (%s): tool source %q: %s write: %v", i, call.Name, tool.source, f, err))
			return nil, false, errAbort
		}
		res = erasedResult{content: failureContent(call.Name, f), isError: true, failure: f}
	}
	r.addClock(&res)
	rec := &resultRecord{Content: res.content, Parts: res.parts, IsError: res.isError}
	if landed, _ := r.commitResult(ctx, i, tool, &res, rec); !landed {
		return nil, false, errAbort
	}
	return rec, false, nil
}
