package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// errAbort stops toolloop after the runtime has decided the activation's
// ending (ADR 0004, item 8): the ending is in modelRun.abort.
var errAbort = errors.New("agent: activation ending")

// errStepLimit stops toolloop at a step boundary when the activation has
// completed the host's step limit (STM-7).
var errStepLimit = errors.New("agent: step limit reached")

// modelRun runs one model state within an activation (ADR 0008): it drives
// toolloop.Run with a client decorator that records each attempt and turn
// before the loop sees it, and tool wrappers that record each call's
// permission, attempt and result.
type modelRun struct {
	// params is the state's request without messages.
	params llms.ChatRequest
	client llms.ChatClient
	a      *activation
	st     *erasedState
	ms     *erasedModel
	// abort is the activation's ending, decided inside the loop: a
	// commit that did not land, or the run's end.
	abort *ending
	// next is the ending of a tool's transition: nil for a non-terminal
	// one.
	next *ending
	// turn is the turn whose calls are being resolved, nil between turns.
	turn *turnState
	// paramsDigest is params' artifact, stored once paramsStored, and
	// paramsRaw its content.
	paramsDigest string
	paramsRaw    []byte
	// transcript and digests are the state's transcript as of its latest
	// checkpoint (or empty before its first turn lands).
	transcript []llms.Message
	digests    []string
	// pending are calls whose result toolloop synthesized itself (a call
	// naming no offered tool), awaiting their result entries (ADR 0004,
	// item 8).
	pending      []int
	progress     modelProgress
	paramsStored bool
	transitioned bool
	stoppedLimit bool
}

// turnState is a recorded turn and how far its calls are resolved.
type turnState struct {
	resp    llms.ChatResponse
	msg     string // the assistant message's artifact
	delta   []llms.Message
	ddelta  []string
	calls   []llms.ToolCall
	perm    []*PermissionEntry
	attempt []*AttemptEntry
	// results holds each resolved call's result as the model receives it:
	// recorded, or a denial from a recorded permission.
	results []*resultRecord
	step    uint64
	cursor  int
}

func (t *turnState) complete() bool {
	for _, r := range t.results {
		if r == nil {
			return false
		}
	}
	return true
}

// resultRecord is a result as the model receives it, stored as an artifact.
type resultRecord struct {
	Content string             `json:"content,omitempty"`
	Parts   []llms.ContentPart `json:"parts,omitempty"`
	IsError bool               `json:"is_error,omitempty"`
}

func (r *resultRecord) toolResult(id string) llms.ToolResult {
	return llms.ToolResult{ToolCallID: id, Content: r.Content, Parts: r.Parts, IsError: r.IsError}
}

// responseRecord is a response as recorded: llms.ChatResponse without its
// provider-specific Raw.
type responseRecord struct {
	AppliedReasoning *llms.ReasoningConfig
	Usage            llms.Usage
	Text             string
	StopReason       llms.StopReason
	Message          llms.Message
	ToolCalls        []llms.ToolCall
}

func (r *responseRecord) response() llms.ChatResponse {
	return llms.ChatResponse{
		Message: r.Message, Text: r.Text, ToolCalls: r.ToolCalls, StopReason: r.StopReason,
		Usage: r.Usage, AppliedReasoning: r.AppliedReasoning,
	}
}

// refusedContent is the result of a call naming no offered tool. It is
// toolloop's own text for such a call, so a call refused by recovery reads
// as one refused by the loop.
func refusedContent(name string) string { return fmt.Sprintf("tool %q is not available", name) }

func deniedContent(p *PermissionEntry) string {
	msg := fmt.Sprintf("tool call not permitted (%s)", p.Permission)
	if p.Reason != "" {
		msg += ": " + p.Reason
	}
	return msg
}

func notExecutedContent(by string) string {
	return fmt.Sprintf("not executed: tool loop ended by %q", by)
}

func digestOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// artifact adds a JSON artifact to the commit, once per digest, and returns
// its digest.
func (b *entries) artifact(v any) string {
	if b.err != nil {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		b.err = storeFailure(fmt.Errorf("agent: encode artifact: %w", err))
		return ""
	}
	return b.rawArtifact(raw)
}

func (b *entries) rawArtifact(raw []byte) string {
	d := digestOf(raw)
	if b.stored == nil {
		b.stored = map[string]bool{}
	}
	if !b.stored[d] {
		b.stored[d] = true
		b.ops = append(b.ops, store.PutArtifactOp(store.Artifact{Digest: d, MediaType: "application/json", Content: raw}))
	}
	return d
}

// readArtifact decodes a JSON artifact. A missing or undecodable artifact
// the record references is a store failure: the commit that referenced it
// stored it (ADR 0004, item 1).
func (a *activation) readArtifact(ctx context.Context, digest string, v any) error {
	_, raw, err := a.c.store.Artifact(durable(ctx), digest)
	if err != nil {
		return storeFailure(fmt.Errorf("agent: read artifact %s: %w", digest, err))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return storeFailure(fmt.Errorf("agent: decode artifact %s: %w", digest, err))
	}
	return nil
}

// modelStep runs a model state from the latest checkpoint until a tool
// transitions, the activation stops at a step boundary, or the run ends. It
// returns nil when the run continues (a non-terminal transition, the step
// limit or the context's end, which the caller handles).
func (a *activation) modelStep(ctx context.Context, st *erasedState) *ending {
	if a.client == nil {
		return a.fail(ctx, ReasonModelError, fmt.Sprintf("model state %q has no model client: ActivationInput.Client is nil", st.name))
	}
	r := &modelRun{a: a, st: st, ms: st.model, client: a.client}
	if e := r.load(ctx); e != nil {
		return e
	}
	if a.tail.turn != nil {
		if e := r.recoverTurn(ctx); e != nil || r.transitioned || r.stoppedLimit || ctx.Err() != nil {
			return e
		}
	}
	for {
		if e, stop := r.loop(ctx); stop {
			return e
		}
	}
}

// load restores the state's request and transcript from the checkpoint, or
// from the tail's recorded attempt, or runs Prompt for the state's first
// attempt.
func (r *modelRun) load(ctx context.Context) *ending {
	a := r.a
	if p := a.cs.Model; p != nil {
		r.progress = *p
		if e := r.loadParams(ctx, p.Params); e != nil {
			return e
		}
		r.digests = append([]string(nil), p.Transcript...)
		r.transcript = make([]llms.Message, len(p.Transcript))
		for i, d := range p.Transcript {
			if err := a.readArtifact(ctx, d, &r.transcript[i]); err != nil {
				return &ending{err: err}
			}
		}
		return nil
	}
	if t := a.tail.turn; t != nil {
		// The state's first turn is recorded but not complete: its request
		// is the recorded one, so Prompt does not run again.
		return r.loadParams(ctx, t.attempt.Params)
	}
	if ctx.Err() != nil {
		return a.endByCause(ctx)
	}
	var req llms.ChatRequest
	err := guard(func() (err error) {
		req, err = r.ms.prompt(ctx, a.env(), a.cs.Data)
		return err
	})
	switch {
	case err != nil:
		return a.codeFailed(ctx, fmt.Errorf("state %q prompt: %w", r.st.name, err))
	case len(req.Messages) == 0:
		return a.fail(ctx, ReasonStateError, fmt.Sprintf("state %q prompt returned no messages", r.st.name))
	case len(req.Tools) > 0:
		return a.fail(ctx, ReasonStateError, fmt.Sprintf("state %q prompt returned tools; a model state offers its Tools", r.st.name))
	}
	if ctx.Err() != nil {
		return a.endByCause(ctx)
	}
	r.transcript = req.Messages
	req.Messages = nil
	req.Tools = make([]llms.ToolDefinition, len(r.ms.tools))
	for i, t := range r.ms.tools {
		req.Tools[i] = t.def
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return a.fail(ctx, ReasonStateError, fmt.Sprintf("state %q prompt: encode request: %v", r.st.name, err))
	}
	r.params, r.paramsRaw, r.paramsDigest = req, raw, digestOf(raw)
	return nil
}

func (r *modelRun) loadParams(ctx context.Context, digest string) *ending {
	var req llms.ChatRequest
	if err := r.a.readArtifact(ctx, digest, &req); err != nil {
		return &ending{err: err}
	}
	r.params, r.paramsDigest, r.paramsStored = req, digest, true
	return nil
}

// loop runs toolloop once from the durable transcript and decides what
// follows. stop reports that modelStep returns e.
func (r *modelRun) loop(ctx context.Context) (e *ending, stop bool) {
	a := r.a
	send := r.transcript
	if r.progress.Answered {
		if r.progress.Nudged {
			return a.fail(ctx, ReasonWrapUpFailed, fmt.Sprintf(
				"state %q: the model answered without calling a tool after it was nudged (LOOP-3)", r.st.name)), true
		}
		send = append(send[:len(send):len(send)], llms.UserText(r.ms.nudge))
	}
	if ctx.Err() != nil {
		return nil, true
	}
	req := r.params
	req.Messages = send
	req.Tools = nil
	tools := make([]toolloop.Tool, len(r.ms.tools))
	for i, t := range r.ms.tools {
		tools[i] = toolloop.Tool{Definition: t.def, Execute: r.execute}
	}
	out := toolloop.Run(ctx, toolloop.Config{
		Request:       req,
		Client:        (*recordingClient)(r),
		Tools:         tools,
		MaxIterations: toolloop.NoIterationLimit,
		OnToolCall:    func(ev toolloop.ToolCallEvent) { r.onToolCall(&ev) },
	})
	if r.abort != nil {
		return r.abort, true
	}
	if e := r.flush(ctx); e != nil {
		return e, true
	}
	switch out.Kind {
	case toolloop.OutcomeTerminalTool:
		return r.next, true
	case toolloop.OutcomeFinalAnswer, toolloop.OutcomeEmptyResponse:
		// The turn was recorded with its checkpoint; the next loop nudges.
		return nil, false
	case toolloop.OutcomeCanceled, toolloop.OutcomeDeadline:
		if ctx.Err() != nil {
			return nil, true
		}
		// The wrappers return a context's error only once the loop's
		// context has ended, so this came from the client.
		return a.fail(ctx, ReasonModelError, fmt.Sprintf("state %q: %v", r.st.name, out.Err)), true
	case toolloop.OutcomeLLMError, toolloop.OutcomeContextExceeded:
		if r.stoppedLimit || ctx.Err() != nil {
			return nil, true
		}
		return a.fail(ctx, ReasonModelError, fmt.Sprintf("state %q: %v", r.st.name, out.Err)), true
	case toolloop.OutcomeToolError:
		// The runtime's wrappers stop the loop only through abort, so this
		// is toolloop refusing the state's configuration (a tool choice
		// naming no tool, say) or a result it could not use.
		return a.fail(ctx, ReasonStateError, fmt.Sprintf("state %q: %v", r.st.name, out.Err)), true
	}
	return a.fail(ctx, ReasonModelError, fmt.Sprintf("state %q: tool loop ended with %s: %v", r.st.name, out.Kind, out.Err)), true
}

// recordingClient is the client decorator (ADR 0004, items 6 to 8): it
// commits each attempt before sending it and its response or failure before
// returning to the loop.
type recordingClient modelRun

func (c *recordingClient) Model() llms.ModelRef { return c.client.Model() }

func (c *recordingClient) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) { //nolint:gocritic // hugeParam: llms.ChatClient's signature
	r := (*modelRun)(c)
	if r.abort != nil {
		return llms.ChatResponse{}, errAbort
	}
	// Results the loop synthesized commit first, with the turn's checkpoint
	// if they complete it, in the same commit as the attempt unless the
	// activation stops here.
	b := r.a.entries()
	completes := r.addPending(b)
	if err := r.stopBeforeAttempt(ctx, b, completes); err != nil {
		return llms.ChatResponse{}, err
	}
	delta, ddelta, err := r.recordAttempt(ctx, b, completes, &req)
	if err != nil {
		return llms.ChatResponse{}, err
	}
	resp, err := c.client.Complete(ctx, req)
	if err != nil {
		return llms.ChatResponse{}, r.recordFailure(ctx, err)
	}
	return r.recordTurn(ctx, &resp, delta, ddelta)
}

// stopBeforeAttempt stops the loop before a new attempt when the context
// has ended or the activation has completed the host's step limit,
// committing b (pending results) first. A nil error admits the attempt.
func (r *modelRun) stopBeforeAttempt(ctx context.Context, b *entries, completes bool) error {
	a := r.a
	steps := a.steps
	if completes {
		steps++
	}
	limit := a.maxSteps > 0 && steps >= a.maxSteps
	if ctx.Err() == nil && !limit {
		return nil
	}
	if len(b.ops) > 0 && !r.commitTurnEntries(ctx, b, completes) {
		return errAbort
	}
	if ctx.Err() != nil {
		return ctx.Err() //nolint:wrapcheck // toolloop classifies the context's error
	}
	r.stoppedLimit = true
	return errStepLimit
}

// recordAttempt commits the model_attempt entry, with b's pending results
// and, if they complete the turn, its checkpoint (ADR 0004, items 6 and 8).
// It returns the request's messages past the checkpoint's transcript and
// their artifacts.
func (r *modelRun) recordAttempt(ctx context.Context, b *entries, completes bool, req *llms.ChatRequest) ([]llms.Message, []string, error) {
	a := r.a
	// The transcript the pending results complete is the request's base.
	cs := r.pendingCheckpoint(b, completes)
	base := r.digests
	if cs != nil {
		base = cs.Model.Transcript
	}
	if len(req.Messages) < len(base) {
		r.abort = a.fail(ctx, ReasonStateError, "agent: tool loop sent a shorter transcript than the checkpoint holds")
		return nil, nil, errAbort
	}
	delta := req.Messages[len(base):]
	entry := ModelAttemptEntry{Model: modelRef(r.client.Model()), State: r.st.name, Base: len(base), Params: r.paramsDigest}
	if !r.paramsStored {
		entry.Params = b.rawArtifact(r.paramsRaw)
	}
	for i := range delta {
		entry.Messages = append(entry.Messages, b.artifact(&delta[i]))
	}
	b.add(a.step1()+boolStep(cs != nil), EntryModelAttempt, entry)
	if !r.land(ctx, b, cs) {
		return nil, nil, errAbort
	}
	r.paramsStored = true
	return delta, entry.Messages, nil
}

// recordFailure commits the attempt's model_failure entry and returns the
// error for the loop to classify.
func (r *modelRun) recordFailure(ctx context.Context, err error) error {
	f := ModelFailureEntry{Message: err.Error()}
	var pe *llms.ProviderError
	if errors.As(err, &pe) {
		f.Kind, f.Status = string(pe.Kind), pe.StatusCode
	}
	b := r.a.entries()
	b.add(r.a.step1(), EntryModelFailure, f)
	if !r.land(ctx, b, nil) {
		return errAbort
	}
	return err
}

func boolStep(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

func modelRef(m llms.ModelRef) ModelRef { return ModelRef{Provider: string(m.Provider), Name: m.Name} }

// recordTurn commits the turn entry before the loop executes any of its
// calls (ADR 0004, item 7). A turn with no calls is complete once recorded,
// so its checkpoint commits with it.
func (r *modelRun) recordTurn(ctx context.Context, resp *llms.ChatResponse, delta []llms.Message, ddelta []string) (llms.ChatResponse, error) {
	a := r.a
	rec := responseRecord{
		Message: resp.Message, Text: resp.Text, ToolCalls: resp.ToolCalls, StopReason: resp.StopReason,
		Usage: resp.Usage, AppliedReasoning: resp.AppliedReasoning,
	}
	b := a.entries()
	t := &turnState{
		resp: rec.response(), delta: delta, ddelta: ddelta, step: a.step1(),
		calls:   cloneCalls(resp.ToolCalls),
		perm:    make([]*PermissionEntry, len(resp.ToolCalls)),
		attempt: make([]*AttemptEntry, len(resp.ToolCalls)),
		results: make([]*resultRecord, len(resp.ToolCalls)),
	}
	t.msg = b.artifact(&resp.Message)
	b.add(t.step, EntryTurn, TurnEntry{
		Model: modelRef(r.client.Model()), Response: b.artifact(&rec), Message: t.msg,
		StopReason: string(resp.StopReason), Usage: usageOf(&resp.Usage), Calls: len(resp.ToolCalls),
	})
	r.turn = t
	if !r.commitTurnEntries(ctx, b, t.complete()) {
		return llms.ChatResponse{}, errAbort
	}
	return *resp, nil
}

func cloneCalls(calls []llms.ToolCall) []llms.ToolCall {
	out := make([]llms.ToolCall, len(calls))
	for i, c := range calls {
		out[i] = llms.ToolCall{
			ID: c.ID, Name: c.Name,
			Parameters:        append(json.RawMessage(nil), c.Parameters...),
			ProviderSignature: append([]byte(nil), c.ProviderSignature...),
		}
	}
	return out
}

// isEmpty mirrors toolloop: a response with no tool calls and no
// non-empty text is not appended to the transcript.
func isEmpty(resp *llms.ChatResponse) bool {
	if len(resp.ToolCalls) > 0 {
		return false
	}
	for i := range resp.Message.Content {
		p := &resp.Message.Content[i]
		switch {
		case p.Type == llms.ContentReasoning:
		case p.Type != llms.ContentText || p.Text != "":
			return false
		}
	}
	return true
}

// turnCheckpoint returns the checkpoint that completes the current turn:
// the transcript extended by the turn's request messages, its response and
// its results, at the next step.
func (r *modelRun) turnCheckpoint(b *entries) *checkpointState {
	t := r.turn
	p := r.progress
	p.Params = r.paramsDigest
	p.Transcript = append(append([]string(nil), r.digests...), t.ddelta...)
	if !isEmpty(&t.resp) {
		p.Transcript = append(p.Transcript, t.msg)
	}
	if len(t.calls) > 0 {
		msg := r.resultsMessage()
		p.Transcript = append(p.Transcript, b.artifact(&msg))
		p.Answered, p.Nudged = false, false
	} else {
		p.Answered = true
		// A nudge in this turn's request means the model was nudged.
		p.Nudged = r.progress.Answered
	}
	cs := r.a.cs
	cs.Model, cs.Step = &p, t.step+1
	return &cs
}

func (r *modelRun) resultsMessage() llms.Message {
	t := r.turn
	results := make([]llms.ToolResult, len(t.calls))
	for i, c := range t.calls {
		results[i] = t.results[i].toolResult(c.ID)
	}
	return llms.ToolResultMessage(results...)
}

// pendingCheckpoint adds the turn's checkpoint to b when the entries added
// complete it.
func (r *modelRun) pendingCheckpoint(b *entries, completes bool) *checkpointState {
	if !completes {
		return nil
	}
	cs := r.turnCheckpoint(b)
	b.checkpoint(cs)
	return cs
}

// commitTurnEntries commits b, with the turn's checkpoint when complete.
// It reports whether the commit landed; when it did not, abort holds the
// ending.
func (r *modelRun) commitTurnEntries(ctx context.Context, b *entries, complete bool) bool {
	cs := r.pendingCheckpoint(b, complete)
	return r.land(ctx, b, cs)
}

// land commits b. When cs is set the commit publishes it, completing the
// turn: the durable transcript advances and the step is counted.
func (r *modelRun) land(ctx context.Context, b *entries, cs *checkpointState) bool {
	a := r.a
	if b.err != nil {
		r.abort = &ending{err: b.err}
		return false
	}
	if err := a.commit(ctx, b.ops, b.next); err != nil {
		r.abort = commitErr(err)
		return false
	}
	if cs != nil {
		r.completed(cs)
	}
	return true
}

// completed advances the durable transcript to the checkpoint cs published.
func (r *modelRun) completed(cs *checkpointState) {
	t := r.turn
	r.transcript = append(append([]llms.Message(nil), r.transcript...), t.delta...)
	if !isEmpty(&t.resp) {
		r.transcript = append(r.transcript, t.resp.Message)
	}
	if len(t.calls) > 0 {
		r.transcript = append(r.transcript, r.resultsMessage())
	}
	r.digests = cs.Model.Transcript
	r.progress = *cs.Model
	r.paramsStored = true
	r.turn = nil
	r.a.checkpointed(cs)
	r.a.steps++
}

// onToolCall captures the results toolloop synthesizes for calls naming no
// offered tool (ADR 0004, item 8): they never reach a wrapper.
func (r *modelRun) onToolCall(ev *toolloop.ToolCallEvent) {
	if r.abort != nil || r.turn == nil || ev.Err != nil || r.ms.byName[ev.Call.Name] != nil {
		return
	}
	i := r.turn.find(&ev.Call)
	if i < 0 {
		return
	}
	res := ev.Result
	r.turn.results[i] = &resultRecord{Content: res.Content, Parts: res.Parts, IsError: res.IsError}
	r.pending = append(r.pending, i)
}

// find returns the first unresolved call at or after the cursor matching
// c, and moves the cursor past it. toolloop executes calls in order.
func (t *turnState) find(c *llms.ToolCall) int {
	for i := t.cursor; i < len(t.calls); i++ {
		if t.calls[i].ID == c.ID && t.calls[i].Name == c.Name {
			t.cursor = i + 1
			return i
		}
	}
	return -1
}

// addPending adds result entries for the pending synthesized results and
// reports whether, with them, the turn is complete.
func (r *modelRun) addPending(b *entries) bool {
	if len(r.pending) == 0 || r.turn == nil {
		return false
	}
	for _, i := range r.pending {
		res := r.turn.results[i]
		b.add(r.turn.step, EntryResult, ResultEntry{
			Call: i, Tool: r.turn.calls[i].Name, Status: ResultRefused, Result: b.artifact(res), IsError: res.IsError,
		})
	}
	r.pending = nil
	return r.turn.complete()
}

// flush commits pending synthesized results when the loop returns, unless
// the activation is already ending.
func (r *modelRun) flush(ctx context.Context) *ending {
	if len(r.pending) == 0 {
		return nil
	}
	b := r.a.entries()
	completes := r.addPending(b)
	if !r.commitTurnEntries(ctx, b, completes) {
		return r.abort
	}
	return nil
}

// execute is every offered tool's toolloop executor: the wrapper of
// ADR 0004 item 8.
func (r *modelRun) execute(ctx context.Context, call llms.ToolCall) (toolloop.ToolResult, error) { //nolint:gocritic // hugeParam: toolloop.Tool.Execute's signature
	if r.abort != nil || r.turn == nil {
		return toolloop.ToolResult{}, errAbort
	}
	i := r.turn.find(&call)
	if i < 0 {
		r.abort = r.a.fail(ctx, ReasonToolError, fmt.Sprintf("tool call %q (%s) is not in the recorded turn", call.ID, call.Name))
		return toolloop.ToolResult{}, errAbort
	}
	res, stop, err := r.resolve(ctx, i)
	if err != nil {
		return toolloop.ToolResult{}, err
	}
	return toolloop.ToolResult{Content: res.Content, Parts: res.Parts, IsError: res.IsError, Stop: stop}, nil
}

// resolve resolves a recorded call naming an offered tool (STM-6): its
// permission, then its attempt, execution and result. It returns what the
// model receives and whether the call transitioned. A non-nil error stops
// the loop: errAbort with abort set, or the context's error.
func (r *modelRun) resolve(ctx context.Context, i int) (*resultRecord, bool, error) {
	a, t := r.a, r.turn
	call := t.calls[i]
	tool := r.ms.byName[call.Name]
	action := ActionID{Scope: a.scope, Step: t.step, Call: i}
	if t.perm[i] == nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err() //nolint:wrapcheck // toolloop classifies the context's error
		}
		p, e := r.permission(ctx, tool, &call, action, i)
		if e != nil {
			r.abort = e
			return nil, false, errAbort
		}
		b := a.entries()
		completes := r.addPending(b)
		b.add(t.step, EntryPermission, p)
		t.perm[i] = &p
		if !p.Permission.Sendable() {
			t.results[i] = &resultRecord{Content: deniedContent(&p), IsError: true}
			completes = t.complete()
		}
		if !r.commitTurnEntries(ctx, b, completes) {
			return nil, false, errAbort
		}
	}
	if p := t.perm[i]; !p.Permission.Sendable() {
		return &resultRecord{Content: deniedContent(p), IsError: true}, false, nil
	}
	args := digestOf(call.Parameters)
	if prev := t.attempt[i]; prev != nil {
		if prev.Args != args {
			r.abort = a.fail(ctx, ReasonToolError, fmt.Sprintf(
				"call %d (%s): reissued with arguments that differ from its recorded attempt", i, call.Name))
			return nil, false, errAbort
		}
		if !tool.reissuable() {
			r.abort = r.unknown(ctx, i, tool)
			return nil, false, errAbort
		}
	}
	if ctx.Err() != nil {
		return nil, false, ctx.Err() //nolint:wrapcheck // toolloop classifies the context's error
	}
	at := AttemptEntry{Call: i, Tool: call.Name, Action: action, Args: args}
	if tool.effect == EffectWrite {
		at.Key = action.IdempotencyKey()
	}
	b := a.entries()
	completes := r.addPending(b)
	b.add(t.step, EntryAttempt, at)
	if !r.commitTurnEntries(ctx, b, completes) {
		return nil, false, errAbort
	}
	t.attempt[i] = &at
	return r.run(ctx, i, tool, &at)
}

// permission asks the policy hook for a call's permission (TOOL-3).
func (r *modelRun) permission(ctx context.Context, tool *erasedTool, call *llms.ToolCall, action ActionID, i int) (PermissionEntry, *ending) {
	a := r.a
	var d Decision
	err := guard(func() (err error) {
		d, err = a.c.policy(ctx, &PolicyRequest{
			Instance: a.run.Instance, AgentType: a.m.name, State: r.st.name, Tool: call.Name,
			Effect: tool.effect, Call: cloneCalls([]llms.ToolCall{*call})[0], Action: action,
		})
		return err
	})
	p := PermissionEntry{Call: i, Tool: call.Name, Reason: d.Reason}
	switch {
	case err != nil:
		return p, a.fail(ctx, ReasonToolError, fmt.Sprintf("call %d (%s): policy: %v", i, call.Name, err))
	case d.Verdict == VerdictAllow:
		p.Permission = PermissionAllowed
	case d.Verdict == VerdictDeny:
		p.Permission = PermissionDenied
	case d.Verdict == VerdictPending:
		return p, a.fail(ctx, ReasonToolError, fmt.Sprintf(
			"call %d (%s): policy answered pending; approval is not built yet (TOOL-4)", i, call.Name))
	default:
		return p, a.fail(ctx, ReasonToolError, fmt.Sprintf("call %d (%s): policy answered %q", i, call.Name, d.Verdict))
	}
	return p, nil
}

// unknown records a write's unknown outcome and fails the run (ADR 0004,
// item 11).
func (r *modelRun) unknown(ctx context.Context, i int, tool *erasedTool) *ending {
	a, t := r.a, r.turn
	b := a.entries()
	r.addPending(b)
	res := &resultRecord{Content: "outcome unknown", IsError: true}
	b.add(t.step, EntryResult, ResultEntry{Call: i, Tool: tool.def.Name, Status: ResultUnknown, Result: b.artifact(res), IsError: true})
	out := OutcomeEntry{Status: store.StatusFailed, Reason: store.Reason{Code: ReasonToolError}, Detail: fmt.Sprintf(
		"call %d (%s): a %s write was attempted and its outcome is unknown (TOOL-12)", i, tool.def.Name, tool.contract)}
	if ctx.Err() != nil {
		return a.endByCause(ctx)
	}
	b.add(t.step, EntryOutcome, out)
	if b.err != nil {
		return &ending{err: b.err}
	}
	if err := a.commit(ctx, append(b.ops, a.finalOps(&out)...), b.next); err != nil {
		return commitErr(err)
	}
	return &ending{out: a.outcome(&out)}
}

// run executes an attempted call and commits its result, re-running an
// atomic local transaction whose operations conflict (ADR 0005, item 10).
func (r *modelRun) run(ctx context.Context, i int, tool *erasedTool, at *AttemptEntry) (*resultRecord, bool, error) {
	a, t := r.a, r.turn
	call := t.calls[i]
	for attempt := 0; ; attempt++ {
		var res erasedResult
		err := guard(func() (err error) {
			res, err = tool.exec(ctx, a.env(), a.cs.Data, cloneCalls([]llms.ToolCall{call})[0], at.Action, at.Key)
			return err
		})
		// A read's or an atomic local transaction's result after the
		// context ended is discarded: its effect commits only with its
		// result, so recovery issues the call again (STM-6).
		if ctx.Err() != nil && tool.reissuable() {
			return nil, false, ctx.Err() //nolint:wrapcheck // toolloop classifies the context's error
		}
		var read *ReadError
		switch {
		case errors.As(err, &read):
			// A store read that failed is a store failure, not the run's
			// (ADR 0003, item 15; ADR 0007, item 13).
			r.abort = &ending{err: storeFailure(read.Err)}
			return nil, false, errAbort
		case err != nil:
			r.abort = a.fail(ctx, ReasonToolError, fmt.Sprintf("call %d (%s): %v", i, call.Name, err))
			return nil, false, errAbort
		}
		if err := r.checkResult(tool, &res); err != nil {
			r.abort = a.fail(ctx, ReasonToolError, fmt.Sprintf("call %d (%s): %v", i, call.Name, err))
			return nil, false, errAbort
		}
		rec := &resultRecord{Content: res.content, Parts: res.parts, IsError: res.isError}
		landed, conflict := r.commitResult(ctx, i, tool, &res, rec)
		switch {
		case landed:
			if res.to != "" {
				return rec, true, nil
			}
			return rec, false, nil
		case conflict != nil && attempt < a.c.retries:
			if ctx.Err() != nil {
				return nil, false, ctx.Err() //nolint:wrapcheck // toolloop classifies the context's error
			}
			continue
		case conflict != nil:
			// The persistent conflict is the call's result, a tool error
			// the model receives (ADR 0005, item 10); nothing was written.
			res = erasedResult{content: fmt.Sprintf("the write conflicted with another commit after %d re-runs: %v", attempt, conflict), isError: true}
			rec = &resultRecord{Content: res.content, IsError: true}
			if landed, _ := r.commitResult(ctx, i, tool, &res, rec); !landed {
				return nil, false, errAbort
			}
			return rec, false, nil
		}
		return nil, false, errAbort
	}
}

// checkResult refuses a result the runtime cannot record or send.
func (r *modelRun) checkResult(tool *erasedTool, res *erasedResult) error {
	if res.to != "" {
		if res.isError {
			return errors.New("result both transitions and is an error")
		}
		if !r.st.next[res.to] {
			return fmt.Errorf("result transitions to %q, which state %q does not declare", res.to, r.st.name)
		}
	}
	if len(res.ops) > 0 && tool.contract != ContractAtomicLocal {
		return errors.New("result carries object operations, which only an atomic local transaction may")
	}
	for i, op := range res.ops {
		if op.Kind != store.OpPutObject && op.Kind != store.OpDeleteObject {
			return fmt.Errorf("operation %d has kind %q; a tool returns object operations only", i, op.Kind)
		}
		if err := op.Validate(); err != nil {
			return fmt.Errorf("operation %d: %w", i, err)
		}
	}
	return checkParts(res.content, res.parts)
}

// checkParts applies toolloop's rules for a result's parts (maestro-llms
// ADR-0041) before the result is recorded, so the loop never refuses a
// recorded result.
func checkParts(content string, parts []llms.ContentPart) error {
	if len(parts) == 0 {
		return nil
	}
	if content != "" {
		return errors.New("result sets both Content and Parts")
	}
	for i := range parts {
		p := &parts[i]
		switch p.Type {
		case llms.ContentText:
		case llms.ContentImage, llms.ContentDocument:
			if p.Media == nil || p.Media.MediaType == "" || len(p.Media.Data) == 0 {
				return fmt.Errorf("part %d (%s) has no media type or data", i, p.Type)
			}
		default:
			return fmt.Errorf("part %d has type %q; a result holds text, image and document parts only", i, p.Type)
		}
	}
	return nil
}

// commitResult commits a call's result with its operations and, for a
// transition, the turn's other results, the transition entry and the new
// state's checkpoint, and entering a terminal state, the run's ending
// (ADR 0004, item 9). It reports whether the commit landed, or the
// conflict that rejected an atomic local transaction's operations; on any
// other failure abort holds the ending.
func (r *modelRun) commitResult(ctx context.Context, i int, tool *erasedTool, res *erasedResult, rec *resultRecord) (bool, *store.ConflictError) {
	a, t := r.a, r.turn
	b := a.entries()
	r.addPendingSnapshot(b)
	b.ops = append(b.ops, res.ops...)
	b.add(t.step, EntryResult, ResultEntry{
		Call: i, Tool: tool.def.Name, Status: ResultExecuted, Result: b.artifact(rec), Ops: res.ops, IsError: rec.IsError,
	})
	prev := t.results[i]
	t.results[i] = rec
	var (
		cs       *checkpointState
		out      OutcomeEntry
		terminal bool
	)
	switch {
	case res.to != "":
		cs, out, terminal = r.addTransition(ctx, b, i, tool, res)
	case t.complete():
		cs = r.turnCheckpoint(b)
		b.checkpoint(cs)
	}
	if b.err != nil {
		r.abort = &ending{err: b.err}
		return false, nil //nolint:nilerr // abort holds the ending
	}
	err := a.commit(ctx, b.ops, b.next)
	var conflict *store.ConflictError
	switch {
	case err == nil:
		r.pending = nil
		if res.to == "" {
			if cs != nil {
				r.completed(cs)
			}
			return true, nil
		}
		a.checkpointed(cs)
		a.steps++
		r.turn, r.transitioned = nil, true
		if terminal {
			r.next = &ending{out: a.outcome(&out)}
		}
		return true, nil
	case errors.As(err, &conflict) && conflict.IsObjectConflict() && len(res.ops) > 0:
		t.results[i] = prev
		return false, conflict
	case errors.Is(err, store.ErrInvalid) && len(res.ops) > 0:
		t.results[i] = prev
		r.abort = a.fail(ctx, ReasonToolError, fmt.Sprintf("call %d (%s): %v", i, tool.def.Name, err))
		return false, nil //nolint:nilerr // abort holds the ending
	}
	r.abort = commitErr(err)
	return false, nil
}

// addTransition adds a tool's transition to b (ADR 0004, item 9): a result
// for every other unresolved call in the turn, the transition entry and the
// new state's checkpoint, and entering a terminal state, the run's ending.
// A terminal transition's ending yields to the context's cause, which the
// activation commits next (ADR 0003, item 17).
func (r *modelRun) addTransition(ctx context.Context, b *entries, i int, tool *erasedTool, res *erasedResult) (*checkpointState, OutcomeEntry, bool) {
	a, t := r.a, r.turn
	for j, c := range t.calls {
		if j == i || t.results[j] != nil {
			continue
		}
		status, content := ResultNotExecuted, notExecutedContent(tool.def.Name)
		if j < i {
			status, content = ResultRefused, refusedContent(c.Name)
		}
		b.add(t.step, EntryResult, ResultEntry{
			Call: j, Tool: c.Name, Status: status, Result: b.artifact(&resultRecord{Content: content, IsError: true}), IsError: true,
		})
	}
	b.add(t.step, EntryTransition, TransitionEntry{From: r.st.name, To: res.to, Data: res.data, Ops: res.ops})
	cs := a.cs
	cs.State, cs.Data, cs.Step, cs.Model = res.to, res.data, t.step+1, nil
	next := a.m.states[res.to]
	terminal := next.end != "" && ctx.Err() == nil
	out := next.end.outcome()
	if terminal {
		b.add(t.step, EntryOutcome, out)
	}
	b.checkpoint(&cs)
	if terminal {
		b.ops = append(b.ops, a.finalOps(&out)...)
	}
	return &cs, out, terminal
}

// addPendingSnapshot adds the pending synthesized results to b without
// clearing them, for a commit that may be rejected and re-run.
func (r *modelRun) addPendingSnapshot(b *entries) {
	for _, i := range r.pending {
		res := r.turn.results[i]
		b.add(r.turn.step, EntryResult, ResultEntry{
			Call: i, Tool: r.turn.calls[i].Name, Status: ResultRefused, Result: b.artifact(res), IsError: res.IsError,
		})
	}
}

// loadTurn rebuilds a recorded turn from the tail: its request messages,
// its response, and each call's permission, attempt and result.
func (a *activation) loadTurn(ctx context.Context, tt *tailTurn) (*turnState, error) {
	t := &turnState{
		step: tt.step, msg: tt.entry.Message, ddelta: tt.attempt.Messages,
		perm:    make([]*PermissionEntry, tt.entry.Calls),
		attempt: make([]*AttemptEntry, tt.entry.Calls),
		results: make([]*resultRecord, tt.entry.Calls),
	}
	var rec responseRecord
	if err := a.readArtifact(ctx, tt.entry.Response, &rec); err != nil {
		return nil, err
	}
	if len(rec.ToolCalls) != tt.entry.Calls {
		return nil, storeFailure(fmt.Errorf("agent: run %q turn response holds %d calls; its entry says %d", a.run.ID, len(rec.ToolCalls), tt.entry.Calls))
	}
	t.resp, t.calls = rec.response(), cloneCalls(rec.ToolCalls)
	t.delta = make([]llms.Message, len(tt.attempt.Messages))
	for i, d := range tt.attempt.Messages {
		if err := a.readArtifact(ctx, d, &t.delta[i]); err != nil {
			return nil, err
		}
	}
	for i := range t.calls {
		if p, ok := tt.perms[i]; ok {
			t.perm[i] = &p
			if !p.Permission.Sendable() {
				t.results[i] = &resultRecord{Content: deniedContent(&p), IsError: true}
			}
		}
		if at, ok := tt.attempts[i]; ok {
			t.attempt[i] = &at
		}
		if res, ok := tt.results[i]; ok {
			var rr resultRecord
			if err := a.readArtifact(ctx, res.Result, &rr); err != nil {
				return nil, err
			}
			t.results[i] = &rr
		}
	}
	return t, nil
}

// recoverTurn resolves the tail's recorded turn (ADR 0004, item 10): the
// model is not asked again; each call is resolved by STM-6's partition, in
// order, through the same path a live call takes.
func (r *modelRun) recoverTurn(ctx context.Context) *ending {
	tt := r.a.tail.turn
	r.a.tail.turn = nil
	t, err := r.a.loadTurn(ctx, tt)
	if err != nil {
		return &ending{err: err}
	}
	r.turn = t
	for i := range t.calls {
		if t.results[i] != nil {
			continue
		}
		if ctx.Err() != nil {
			return r.flush(ctx)
		}
		if r.ms.byName[t.calls[i].Name] == nil {
			t.results[i] = &resultRecord{Content: refusedContent(t.calls[i].Name), IsError: true}
			r.pending = append(r.pending, i)
			continue
		}
		t.cursor = i + 1
		if _, _, err := r.resolve(ctx, i); err != nil {
			if r.abort != nil {
				return r.abort
			}
			return r.flush(ctx)
		}
		if r.transitioned {
			return r.next
		}
	}
	// Refused calls at the end of the turn complete it now, before the
	// turn continues (ADR 0004, item 10).
	return r.flush(ctx)
}
