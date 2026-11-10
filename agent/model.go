package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"unicode/utf8"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// ModelState declares a model state (STM-2): a tool loop through
// maestro-llms' toolloop (LOOP-1), left when one of its tools returns a
// transition (STM-3). Every model turn is a step; each turn and each tool
// call's permission, attempt and result are recorded before anything that
// depends on them (ADR 0004, ADR 0008).
type ModelState[D any] struct {
	// Prompt returns the state's request: its system instructions, its
	// first messages and its parameters (MaxTokens, Temperature, ToolChoice
	// and the rest). Messages must not be empty and Tools must be empty:
	// the state's tools are offered from Tools. It runs when the state's
	// first model attempt is prepared, and the request it returns is
	// recorded with that attempt; until the state's first turn lands it may
	// run again (after a crash, a failed attempt or a shutdown), so, like a
	// code state, it must have no durable effect of its own. Its errors are
	// handled as a code state's are.
	Prompt func(ctx context.Context, env *Env, data D) (llms.ChatRequest, error)

	// Nudge is the user message sent after a turn that calls no tool
	// (LOOP-3); empty uses DefaultNudge. The model is nudged once: a
	// second answer in a row that calls no tool fails the run with
	// ReasonWrapUpFailed.
	Nudge string

	// Tools are the tools offered to the model in this state (TOOL-1). A
	// call naming any other tool is refused, and the model receives the
	// refusal as a tool error (MAN-2).
	Tools []Tool[D]
}

// DefaultNudge is the message a model state sends after a turn that calls
// no tool, when the state declares none.
const DefaultNudge = "You have not called a tool. This step is complete only when you call one of the tools that finish it; call the appropriate tool now."

// Tool is a tool offered to a model state (TOOL-1, TOOL-2, TOOL-12).
type Tool[D any] struct {
	// Execute runs the call. A model-visible failure is a Result with
	// IsError set, which the model receives and may correct (LOOP-2). A
	// returned error, or a panic, fails the run with ReasonToolError.
	//
	// It may run more than once for one call: a read or an atomic local
	// transaction attempted with no recorded result is issued again by
	// recovery (STM-6), and an atomic local transaction whose operations
	// conflict runs again against fresh reads (ADR 0005, item 10).
	Execute func(ctx context.Context, call *Call[D]) (Result[D], error)

	// Effect classifies the tool (TOOL-2).
	Effect Effect

	// Contract is a write tool's write contract (TOOL-12): how it prevents
	// a duplicate effect. A read tool declares none.
	Contract WriteContract

	// Definition is the schema offered to the model.
	Definition llms.ToolDefinition
}

// Effect is a tool's effect classification (TOOL-2).
type Effect string

// The effect classes.
const (
	// EffectRead has no effect to duplicate, so a read attempted with no
	// recorded result is issued again (STM-6).
	EffectRead Effect = "read"
	// EffectWrite has an effect; it declares a write contract (TOOL-12).
	EffectWrite Effect = "write"
)

// WriteContract is how a write tool prevents a duplicate effect (TOOL-12).
type WriteContract string

// The write contracts.
const (
	// ContractAtomicLocal: the tool does not write. It returns object
	// operations, conditional on the versions it read, which the runtime
	// commits with the call's result (ADR 0005, items 7 to 10), so an
	// attempt with no result is known not to have happened and is issued
	// again.
	ContractAtomicLocal WriteContract = "atomic_local"
	// ContractIdempotent: the destination deduplicates on the call's
	// idempotency key (TOOL-5).
	ContractIdempotent WriteContract = "idempotent"
	// ContractReconcile: a reconciliation probe can find out whether the
	// effect happened. Resolving an unknown outcome by probe is not built
	// yet (MVP.md); until it is, an unknown outcome fails the run.
	ContractReconcile WriteContract = "reconcile"
	// ContractUnprotected: the tool offers none of the three, and the
	// agent type explicitly accepts the risk of a duplicate (TOOL-12).
	ContractUnprotected WriteContract = "unprotected"
)

// Call is one tool call as its tool's Execute sees it.
type Call[D any] struct {
	// Data is the state's data, decoded afresh for every execution, so a
	// tool never sees a value an earlier execution modified.
	Data D
	// Env exposes the run and object reads. An atomic local transaction
	// reads through it and writes only through Result.Ops.
	Env *Env
	// IdempotencyKey is a write call's idempotency key (TOOL-5), stable
	// across every issue of the call; a read's is empty.
	IdempotencyKey string
	// ToolCall is the call as the model emitted it.
	ToolCall llms.ToolCall
	// Action is the call's action identity (ADR 0003, item 4).
	Action ActionID
}

// Result is a tool's result: what the model receives and, optionally, a
// transition and object operations.
type Result[D any] struct {
	// Data is the next state's data when To is set.
	Data D
	// Content is the result as text. Parts carries a result with images or
	// documents instead (TOOL-11): text, image and document parts only,
	// and not together with Content.
	Content string
	// To, if set, transitions the run to the named state, which the state
	// must declare in Next (STM-1, STM-3). The call then ends its turn:
	// later calls in the turn are not executed. A transition cannot also be
	// an error.
	To    string
	Parts []llms.ContentPart
	// Ops are object operations, for a tool whose contract is
	// ContractAtomicLocal only, conditional on the versions it read. They
	// commit with the call's result, and with its transition if it has one.
	Ops []store.Op
	// IsError marks a result the model should correct.
	IsError bool
}

// TextResult returns a result holding text.
func TextResult[D any](content string) Result[D] { return Result[D]{Content: content} }

// ErrorResult returns a result the model should correct (LOOP-2).
func ErrorResult[D any](content string) Result[D] { return Result[D]{Content: content, IsError: true} }

// ActionID is a tool call's action identity (ADR 0003, item 4): the run's
// scope, the step and the call's position in the recorded model turn.
type ActionID struct {
	Scope store.Scope `json:"scope"`
	Step  uint64      `json:"step"`
	Call  int         `json:"call"`
}

// IdempotencyKey returns the action's idempotency key (TOOL-5): the hex
// SHA-256 of a canonical, domain-separated encoding of the identity, each
// field length-prefixed so no two identities encode alike.
func (a ActionID) IdempotencyKey() string {
	h := sha256.New()
	field := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	field([]byte("maestro-agents/action/v1"))
	field([]byte(a.Scope.Kind))
	field([]byte(a.Scope.ID))
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], a.Step)
	field(n[:])
	binary.BigEndian.PutUint64(n[:], uint64(a.Call)) //nolint:gosec // a call index is never negative
	field(n[:])
	return hex.EncodeToString(h.Sum(nil))
}

// Policy is the host's policy hook (TOOL-3): every tool call passes through
// it before its effect, and its decision is recorded as the call's
// permission (STM-6). A call whose recorded permission is not allowed or
// approved is never sent; the model receives the denial as data. An error
// fails the run with ReasonToolError.
type Policy func(ctx context.Context, req *PolicyRequest) (Decision, error)

// PolicyRequest is what the policy hook decides on.
type PolicyRequest struct {
	Instance  string
	AgentType string
	State     string
	Tool      string
	Effect    Effect
	Call      llms.ToolCall
	Action    ActionID
}

// Decision is the policy hook's answer.
type Decision struct {
	Verdict Verdict
	// Reason explains a denial; the model receives it.
	Reason string
}

// Verdict is the policy hook's verdict (TOOL-3).
type Verdict string

// The verdicts.
const (
	VerdictAllow Verdict = "allow"
	VerdictDeny  Verdict = "deny"
	// VerdictPending asks for approval (TOOL-4). Approval is not built
	// yet (MVP.md): a pending verdict fails the run with ReasonToolError.
	VerdictPending Verdict = "pending"
)

// AllowAll is the default policy hook: it allows every call (TOOL-3).
func AllowAll(context.Context, *PolicyRequest) (Decision, error) {
	return Decision{Verdict: VerdictAllow}, nil
}

// Permission is a call's recorded permission (STM-6). Only allowed and
// approved calls may be sent.
type Permission string

// The seven permissions. The MVP records allowed and denied; the others
// arrive with approval (TOOL-4, STM-8).
const (
	PermissionAllowed          Permission = "allowed"
	PermissionDenied           Permission = "denied"
	PermissionAwaitingApproval Permission = "awaiting_approval"
	PermissionApproved         Permission = "approved"
	PermissionRejected         Permission = "rejected"
	PermissionInvalidated      Permission = "invalidated"
	PermissionProposed         Permission = "proposed"
)

// Sendable reports whether a call with this permission may be sent.
func (p Permission) Sendable() bool { return p == PermissionAllowed || p == PermissionApproved }

func (p Permission) valid() bool {
	switch p {
	case PermissionAllowed, PermissionDenied, PermissionAwaitingApproval, PermissionApproved,
		PermissionRejected, PermissionInvalidated, PermissionProposed:
		return true
	}
	return false
}

// erasedModel is a model state with its data type erased.
type erasedModel struct {
	prompt func(ctx context.Context, env *Env, data json.RawMessage) (llms.ChatRequest, error)
	byName map[string]*erasedTool
	nudge  string
	tools  []*erasedTool
}

// definitions returns the tool definitions the state offers, in order.
func (m *erasedModel) definitions() []llms.ToolDefinition {
	defs := make([]llms.ToolDefinition, len(m.tools))
	for i, t := range m.tools {
		defs[i] = t.def
	}
	return defs
}

type erasedTool struct {
	exec     func(ctx context.Context, env *Env, data json.RawMessage, call llms.ToolCall, action ActionID, key string) (erasedResult, error)
	effect   Effect
	contract WriteContract
	def      llms.ToolDefinition
}

// reissuable reports whether an attempt with no recorded result is issued
// again (STM-6): a read, or an atomic local transaction, whose effect
// commits only with its result.
func (t *erasedTool) reissuable() bool {
	return reissuable(t.effect, t.contract)
}

func reissuable(effect Effect, contract WriteContract) bool {
	return effect == EffectRead || contract == ContractAtomicLocal
}

// compactJSON returns a compacted copy of valid JSON, nil for nil.
func compactJSON(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	return b.Bytes()
}

type erasedResult struct {
	content string
	to      string
	data    json.RawMessage
	parts   []llms.ContentPart
	ops     []store.Op
	isError bool
}

func eraseModel[D any](ms *ModelState[D]) *erasedModel {
	prompt := ms.Prompt
	em := &erasedModel{nudge: ms.Nudge, byName: make(map[string]*erasedTool, len(ms.Tools))}
	if em.nudge == "" {
		em.nudge = DefaultNudge
	}
	em.prompt = func(ctx context.Context, env *Env, raw json.RawMessage) (llms.ChatRequest, error) {
		d, err := decodeData[D](raw)
		if err != nil {
			return llms.ChatRequest{}, err
		}
		return prompt(ctx, env, d)
	}
	for i := range ms.Tools {
		t := ms.Tools[i]
		exec := t.Execute
		et := &erasedTool{def: t.Definition, effect: t.Effect, contract: t.Contract}
		// The schema's bytes are the declaration's own: registration keeps
		// a copy, compacted as the recorded parameters hold it, so what is
		// sent is what is recorded (ADR 0008, item 8).
		et.def.InputSchema = compactJSON(t.Definition.InputSchema)
		et.exec = func(ctx context.Context, env *Env, raw json.RawMessage, call llms.ToolCall, action ActionID, key string) (erasedResult, error) {
			d, err := decodeData[D](raw)
			if err != nil {
				return erasedResult{}, err
			}
			res, err := exec(ctx, &Call[D]{Env: env, Data: d, ToolCall: call, Action: action, IdempotencyKey: key})
			if err != nil {
				return erasedResult{}, err
			}
			out := erasedResult{content: res.Content, parts: res.Parts, to: res.To, ops: res.Ops, isError: res.IsError}
			if res.To != "" {
				if out.data, err = marshalData(res.Data); err != nil {
					return erasedResult{}, err
				}
			}
			return out, nil
		}
		em.tools = append(em.tools, et)
		em.byName[t.Definition.Name] = et
	}
	return em
}

func decodeData[D any](raw json.RawMessage) (D, error) {
	var d D
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, &decodeError{err: err}
	}
	return d, nil
}

type decodeError struct{ err error }

func (e *decodeError) Error() string { return "agent: decode state data: " + e.err.Error() }
func (e *decodeError) Unwrap() error { return e.err }

// checkModel validates a model state's declaration.
func (t *Type[D]) checkModel(s *State[D]) error {
	ms := s.Model
	if ms.Prompt == nil {
		return t.invalid("model state %q has no Prompt", s.Name)
	}
	// What a model state declares reaches the transcript and the record,
	// which keep only valid UTF-8 and valid JSON exactly (ADR 0008, item 8).
	if !utf8.ValidString(ms.Nudge) {
		return t.invalid("model state %q nudge is not valid UTF-8", s.Name)
	}
	seen := map[string]bool{}
	for i := range ms.Tools {
		tool := &ms.Tools[i]
		name := tool.Definition.Name
		switch {
		case name == "":
			return t.invalid("model state %q tool %d has no name", s.Name, i)
		case seen[name]:
			return t.invalid("model state %q declares tool %q twice", s.Name, name)
		case tool.Execute == nil:
			return t.invalid("model state %q tool %q has no Execute", s.Name, name)
		case !utf8.ValidString(name) || !utf8.ValidString(tool.Definition.Description):
			return t.invalid("model state %q tool %d has a name or description that is not valid UTF-8", s.Name, i)
		}
		if schema := tool.Definition.InputSchema; schema != nil {
			if err := store.ValidJSON(schema); err != nil {
				return t.invalid("tool %q input schema: %v", name, err)
			}
		}
		seen[name] = true
		if err := t.checkEffect(tool); err != nil {
			return err
		}
	}
	return nil
}

// checkEffect checks a tool's effect classification and write contract
// (TOOL-2, TOOL-12).
func (t *Type[D]) checkEffect(tool *Tool[D]) error {
	name := tool.Definition.Name
	switch tool.Effect {
	case EffectRead:
		if tool.Contract != "" {
			return t.invalid("read tool %q declares a write contract", name)
		}
	case EffectWrite:
		switch tool.Contract {
		case ContractAtomicLocal, ContractIdempotent, ContractReconcile, ContractUnprotected:
		case "":
			return t.invalid("write tool %q declares no write contract (TOOL-12)", name)
		default:
			return t.invalid("write tool %q has unknown write contract %q", name, tool.Contract)
		}
	default:
		return t.invalid("tool %q has effect %q; want read or write (TOOL-2)", name, tool.Effect)
	}
	return nil
}
