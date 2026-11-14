package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
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

	// Approval is what follows a pending verdict for the state's calls
	// (TOOL-4). Approval is not built yet: a state declaring a mode is
	// refused at registration (ADR 0011, item 9).
	Approval Approval

	// Tools are the tools offered to the model in this state (TOOL-1). A
	// call naming any other tool is refused, and the model receives the
	// refusal as a tool error (MAN-2). No tool may be named
	// ReadSkillTool.
	Tools []Tool[D]

	// Skills names the skills granted in this state (SKL-1, SKL-2, ADR
	// 0013), each held by the type's Skills, in the order the index lists
	// them. A state granting any is also offered the ReadSkillTool tool,
	// after its own: its description lists each skill's name and
	// description, and a call returns a skill's SKILL.md or one of its
	// files. It is a read; it does not transition, so it is never a
	// wrap-up tool, and a wrap-up turn does not offer it.
	Skills []string

	// WrapUp is the state's forced wrap-up near a budget's end (LOOP-3,
	// BUD-2, ADR 0010). It must name the tools that leave the state.
	WrapUp WrapUp
}

// DefaultNudge is the message a model state sends after a turn that calls
// no tool, when the state declares none.
const DefaultNudge = "You have not called a tool. This step is complete only when you call one of the tools that finish it; call the appropriate tool now."

// WrapUp declares a model state's forced wrap-up (LOOP-3, ADR 0010). When a
// run's budget nears its end while the state runs (the active time or the
// time to the deadline left is within Reserve, or the turns left within
// Turns), the state's next turns are wrap-up turns: only Tools are offered,
// with tool choice auto, and the wrap-up message tells the model to call
// one of them. Tool choice is not forced, since some providers reject a
// forced tool (with reasoning on, or on every request). A tool executing a
// wrap-up turn's call sees Call.WrapUp, so it can relax what it accepts
// from a model out of budget. A state that has not transitioned after
// Turns wrap-up turns fails the run with ReasonWrapUpFailed. Once the
// state wraps up it does not go back, and a state entered with a budget
// already within its reserve wraps up from its first turn.
type WrapUp struct {
	// Message is the user message that starts the wrap-up, sent again
	// after a wrap-up turn that calls no tool; empty uses
	// DefaultWrapUpMessage.
	Message string
	// Tools names the tools that leave the state: at least one, each
	// declared in the state's Tools. A wrap-up turn offers only these.
	Tools []string
	// Reserve is the time held back for wrap-up from each time budget the
	// run sets. Zero is a tenth of each: of the active-time budget, and of
	// the time from the run's creation to its deadline.
	Reserve time.Duration
	// Turns bounds the completed wrap-up turns (the first call, any
	// resubmission after a tool error, and a turn that calls no tool),
	// and is the reserve held back from the turn budget. Zero uses
	// DefaultWrapUpTurns.
	Turns int
}

// DefaultWrapUpMessage starts a wrap-up when the state declares no message.
const DefaultWrapUpMessage = "Your budget for this step is nearly spent. Finish now: call one of the tools that complete this step with what you have."

// DefaultWrapUpTurns bounds a wrap-up when the state declares no bound.
const DefaultWrapUpTurns = 3

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

	// Version is the version of the code that implements the tool, part
	// of its content identity (MAN-1, ADR 0011 item 3); empty takes the
	// agent type's Version. A tool name names one tool in an agent type:
	// states declaring tools of one name must declare the same
	// definition, effect, contract and version.
	Version string

	// Approval is what follows a pending verdict for the tool's calls
	// (TOOL-4). Approval is not built yet: a tool declaring a mode is
	// refused at registration (ADR 0011, item 9).
	Approval Approval

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
	// Env exposes the run and reads of its namespace. An atomic local
	// transaction reads through it and writes only through Result.Ops.
	Env *Env
	// IdempotencyKey is a write call's idempotency key (TOOL-5), stable
	// across every issue of the call; a read's is empty.
	IdempotencyKey string
	// ToolCall is the call as the model emitted it.
	ToolCall llms.ToolCall
	// Action is the call's action identity (ADR 0003, item 4).
	Action ActionID
	// WrapUp reports that the call is in a wrap-up turn (ADR 0010): the
	// model is out of budget, and the tool may accept what it has.
	WrapUp bool
}

// RunContext returns the call's run context (TOOL-8, ADR 0016 item 12).
func (c *Call[D]) RunContext() RunContext { return c.Env.rc }

// Credential asks the host's credential provider for the named credential,
// for the call's run context, when the tool executes (TOOL-8, ADR 0016
// item 13). Without a provider it fails with ErrNoCredential, and so it
// always does for an atomic-local tool, which receives no credential
// provider (ADR 0005, item 9). The runtime never records or logs the
// secret; a tool that puts its value in a result
// discloses it to the execution record.
func (c *Call[D]) Credential(ctx context.Context, name string) (Secret, error) {
	creds := c.Env.creds
	if creds == nil {
		creds = noCredentials
	}
	rc := c.Env.rc
	s, err := creds(ctx, &rc, name)
	if err != nil {
		return Secret{}, fmt.Errorf("agent: credential %q: %w", name, err)
	}
	return s, nil
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
	// ContractAtomicLocal only, in the run's namespace (Env.PutJSON,
	// Env.PutBytes, Env.Delete; ADR 0012) and conditional on the versions
	// it read. An operation outside the run's namespace fails the run with
	// ReasonToolError. They commit with the call's result, and with its
	// transition if it has one.
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
	// Identity is the tool's content identity (MAN-1, ADR 0011 item 3),
	// which the decision is bound to and the permission records.
	Identity string
	Effect   Effect
	Call     llms.ToolCall
	// Run is the call's run context, the key the policy shares with the
	// credential provider (HOST-5, ADR 0016 item 12).
	Run    RunContext
	Action ActionID
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
	// yet (MVP.md), so no state or tool declares a mode that could follow
	// it: a pending verdict fails the run with ReasonConfigError (ADR
	// 0011, item 10).
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
	// exits are the wrap-up's tools, by name (ADR 0010).
	exits   map[string]bool
	nudge   string
	wrapMsg string
	tools   []*erasedTool
	reserve time.Duration
	// wrapTurns bounds the wrap-up turns.
	wrapTurns int
}

// offered reports whether a turn offered the named tool: every declared
// tool, or in a wrap-up turn the wrap-up's tools only.
func (m *erasedModel) offered(name string, wrapUp bool) bool {
	return m.byName[name] != nil && (!wrapUp || m.exits[name])
}

// wrapUpTools returns the tools a wrap-up turn offers, in declaration
// order, and its tool choice: auto, whatever the state's own, which could
// name a tool not offered or forbid tools; a forced choice is not used
// because some providers reject one (ADR 0010, item 5).
func (m *erasedModel) wrapUpTools() ([]*erasedTool, llms.ToolChoice) {
	var out []*erasedTool
	for _, t := range m.tools {
		if m.exits[t.def.Name] {
			out = append(out, t)
		}
	}
	return out, llms.ToolChoice{Type: llms.ToolChoiceAuto}
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
	exec     func(ctx context.Context, env *Env, data json.RawMessage, call llms.ToolCall, action ActionID, key string, wrapUp bool) (erasedResult, error)
	effect   Effect
	contract WriteContract
	identity string
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

func eraseModel[D any](ms *ModelState[D], typeVersion string, skills []*skill) *erasedModel {
	prompt := ms.Prompt
	em := &erasedModel{
		nudge: ms.Nudge, wrapMsg: ms.WrapUp.Message, reserve: ms.WrapUp.Reserve, wrapTurns: ms.WrapUp.Turns,
		byName: make(map[string]*erasedTool, len(ms.Tools)), exits: make(map[string]bool, len(ms.WrapUp.Tools)),
	}
	if em.nudge == "" {
		em.nudge = DefaultNudge
	}
	if em.wrapMsg == "" {
		em.wrapMsg = DefaultWrapUpMessage
	}
	if em.wrapTurns == 0 {
		em.wrapTurns = DefaultWrapUpTurns
	}
	for _, name := range ms.WrapUp.Tools {
		em.exits[name] = true
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
		version := t.Version
		if version == "" {
			version = typeVersion
		}
		et.identity = toolIdentity(&et.def, t.Effect, t.Contract, version)
		atomicLocal := t.Contract == ContractAtomicLocal
		et.exec = func(ctx context.Context, env *Env, raw json.RawMessage, call llms.ToolCall, action ActionID, key string, wrapUp bool) (erasedResult, error) {
			d, err := decodeData[D](raw)
			if err != nil {
				return erasedResult{}, err
			}
			if atomicLocal {
				// An atomic-local tool receives no credential provider (ADR
				// 0005, item 9): it has no effect beyond its operations.
				e := *env
				e.creds = atomicLocalNoCredentials
				env = &e
			}
			res, err := exec(ctx, &Call[D]{Env: env, Data: d, ToolCall: call, Action: action, IdempotencyKey: key, WrapUp: wrapUp})
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
	if len(skills) > 0 {
		et := readSkill(skills)
		em.tools = append(em.tools, et)
		em.byName[et.def.Name] = et
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
	if ms.Approval != "" {
		return t.invalid("model state %q declares approval %q; approval is not built yet (TOOL-4, ADR 0011 item 9)", s.Name, ms.Approval)
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
		case !utf8.ValidString(tool.Version):
			return t.invalid("model state %q tool %q version is not valid UTF-8", s.Name, name)
		case tool.Approval != "":
			return t.invalid("tool %q declares approval %q; approval is not built yet (TOOL-4, ADR 0011 item 9)", name, tool.Approval)
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
	// The wrap-up names declared tools, so it is checked after them: a
	// defect in a tool's declaration is reported as itself.
	return t.checkWrapUp(s)
}

// checkWrapUp checks a model state's wrap-up (ADR 0010): it names at least
// one of the state's tools, each once.
func (t *Type[D]) checkWrapUp(s *State[D]) error {
	w := &s.Model.WrapUp
	switch {
	case len(w.Tools) == 0:
		return t.invalid("model state %q wrap-up names no tool (LOOP-3)", s.Name)
	case !utf8.ValidString(w.Message):
		return t.invalid("model state %q wrap-up message is not valid UTF-8", s.Name)
	case w.Reserve < 0:
		return t.invalid("model state %q wrap-up reserve %s is negative", s.Name, w.Reserve)
	case w.Turns < 0:
		return t.invalid("model state %q wrap-up turns %d is negative", s.Name, w.Turns)
	}
	declared := map[string]bool{}
	for i := range s.Model.Tools {
		declared[s.Model.Tools[i].Definition.Name] = true
	}
	seen := map[string]bool{}
	for _, name := range w.Tools {
		switch {
		case !declared[name]:
			return t.invalid("model state %q wrap-up names tool %q, which the state does not declare", s.Name, name)
		case seen[name]:
			return t.invalid("model state %q wrap-up names tool %q twice", s.Name, name)
		}
		seen[name] = true
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
