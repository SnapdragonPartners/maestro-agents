package agent

import (
	"encoding/json"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Execution-record entry kinds this package writes (ADR 0004, item 2;
// ADR 0008).
const (
	EntryActivationStarted = "activation_started"
	EntryRunStarted        = "run_started"
	EntryModelAttempt      = "model_attempt"
	EntryTurn              = "turn"
	EntryModelFailure      = "model_failure"
	EntryPermission        = "permission"
	EntryAttempt           = "attempt"
	EntryResult            = "result"
	EntryTransition        = "transition"
	EntryOutcome           = "outcome"
)

// Clock is the run's active time when an entry was committed (BUD-2, ADR
// 0009 item 10). Every entry payload embeds it, and the runtime stamps it on
// each entry as it adds it, so recovery restores active time as of the
// latest entry and a crash loses at most the time since then.
type Clock struct {
	Active time.Duration `json:"active,omitempty"`
}

func (c *Clock) stamp(d time.Duration) { c.Active = d }

// ActivationStarted is the payload of an activation_started entry: the
// claim and the starting point, committed before anything else the
// activation does. CheckpointSeq and RecordSeq are zero for a run's first
// activation.
type ActivationStarted struct {
	Owner         string `json:"owner"`
	Epoch         uint64 `json:"epoch"`
	CheckpointSeq uint64 `json:"checkpoint_seq"`
	RecordSeq     uint64 `json:"record_seq"`
	Clock
}

// RunStarted is the payload of a run_started entry (REC-1). Config is the
// digest of the resolved configuration's artifact, empty if the run has
// none; Budgets are the run's budgets, nil if it has none (ADR 0009), with
// the agent type's defaults filled in (ADR 0011, item 7). Manifest is the
// digest of the agent type's manifest's artifact (MAN-4, ADR 0011 item 11).
type RunStarted struct {
	Budgets      *Budgets `json:"budgets,omitempty"`
	AgentType    string   `json:"agent_type"`
	AgentVersion string   `json:"agent_version"`
	Manifest     string   `json:"manifest,omitempty"`
	TriggerID    string   `json:"trigger_id"`
	TriggerKind  string   `json:"trigger_kind,omitempty"`
	Config       string   `json:"config,omitempty"`
	// Namespace is the run's object-store namespace (STORE-2, ADR 0012).
	Namespace string          `json:"namespace"`
	Trigger   json.RawMessage `json:"trigger,omitempty"`
	Clock
}

// TransitionEntry is the payload of a transition entry: the state left and
// entered, the new state data and the object operations committed with it.
type TransitionEntry struct {
	From string          `json:"from"`
	To   string          `json:"to"`
	Data json.RawMessage `json:"data"`
	Ops  []store.Op      `json:"ops,omitempty"`
	Clock
}

// OutcomeEntry is the payload of an outcome entry: the status and reason
// the activation ended with (RUN-3), a failure's detail, and the run's
// consumption as the activation ended (ADR 0009).
type OutcomeEntry struct {
	Consumed *Consumption `json:"consumed,omitempty"`
	Status   store.Status `json:"status"`
	Reason   store.Reason `json:"reason"`
	Detail   string       `json:"detail,omitempty"`
	Clock
}

// ModelRef is the model route a client reported (LLM-3).
type ModelRef struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

// ModelAttemptEntry is the payload of a model_attempt entry, committed
// before the attempt is sent (ADR 0004, item 6). The request is the
// parameters' artifact (system instructions, tools, tool choice and the
// rest, without messages) and the transcript: the Base messages the state's
// latest checkpoint holds, followed by the Messages artifacts added since.
// Reservation is what the attempt was admitted on (BUD-3); an attempt
// recorded before budgets were built has none. Params are the parameters
// sent; when they differ from the state's own (a token budget lowered the
// output cap, or a wrap-up turn offered only the wrap-up's tools, ADR
// 0010), StateParams are the state's own, from which later attempts are
// prepared.
type ModelAttemptEntry struct {
	Reservation *Reservation `json:"reservation,omitempty"`
	Model       ModelRef     `json:"model"`
	State       string       `json:"state"`
	Params      string       `json:"params"`
	StateParams string       `json:"state_params,omitempty"`
	Messages    []string     `json:"messages,omitempty"`
	Base        int          `json:"base"`
	// WrapUp marks a wrap-up turn's attempt (ADR 0010): its calls execute
	// with Call.WrapUp, and only the wrap-up's tools were offered.
	WrapUp bool `json:"wrap_up,omitempty"`
	Clock
}

// TurnEntry is the payload of a turn entry: the attempt's response,
// committed before any of its calls executes (ADR 0004, item 7). Response
// is the response's artifact (its message, tool calls, stop reason, usage
// and applied reasoning); Message is the assistant message's artifact, as
// the transcript holds it.
type TurnEntry struct {
	Model      ModelRef `json:"model"`
	Response   string   `json:"response"`
	Message    string   `json:"message"`
	StopReason string   `json:"stop_reason,omitempty"`
	Usage      Usage    `json:"usage"`
	Calls      int      `json:"calls"`
	Clock
}

// Usage is a response's observed token usage (maestro-llms' llms.Usage,
// whose fields it mirrors).
type Usage struct {
	ProviderRequestID    string `json:"provider_request_id,omitempty"`
	InputTokens          int    `json:"input_tokens,omitempty"`
	OutputTokens         int    `json:"output_tokens,omitempty"`
	TotalTokens          int    `json:"total_tokens,omitempty"`
	ReasoningTokens      int    `json:"reasoning_tokens,omitempty"`
	BillableOutputTokens int    `json:"billable_output_tokens,omitempty"`
	CacheReadTokens      int    `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens     int    `json:"cache_write_tokens,omitempty"`
}

func usageOf(u *llms.Usage) Usage {
	return Usage{
		ProviderRequestID: u.ProviderRequestID, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		TotalTokens: u.TotalTokens, ReasoningTokens: u.ReasoningTokens, BillableOutputTokens: u.BillableOutputTokens,
		CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens,
	}
}

// ModelFailureEntry is the payload of a model_failure entry: the attempt's
// observed error (ADR 0004, item 2). Kind is the maestro-llms error kind of
// a provider error, empty for any other error. Usage is the usage reported,
// nil when none was: a provider error carries none, a response the runtime
// cannot record does (ADR 0009).
type ModelFailureEntry struct {
	Usage   *Usage `json:"usage,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Message string `json:"message"`
	Status  int    `json:"status,omitempty"`
	Clock
}

// PermissionEntry is the payload of a permission entry: a call's
// permission and the policy decision behind it (STM-6, TOOL-3).
type PermissionEntry struct {
	Tool string `json:"tool"`
	// Identity is the content identity of the tool the policy decided on
	// (ADR 0011, item 3): recovery refuses the call if the tool now has
	// another. Empty in a permission recorded before manifests, which is
	// held to its effect alone.
	Identity string `json:"identity,omitempty"`
	// Effect is the tool's effect the policy decided on (TOOL-2): recovery
	// refuses a call whose tool a later version declares with another.
	Effect     Effect     `json:"effect"`
	Permission Permission `json:"permission"`
	Reason     string     `json:"reason,omitempty"`
	Call       int        `json:"call"`
	Clock
}

// AttemptEntry is the payload of an attempt entry, committed immediately
// before the call is sent (STM-6): its action identity, the SHA-256 of its
// arguments, the tool's effect and write contract as they were when it was
// sent and, for a write, its idempotency key (TOOL-5). Recovery partitions
// an attempt with no result by its recorded effect and contract, not by
// the tool a later version declares (ADR 0008, item 17).
type AttemptEntry struct {
	Tool     string        `json:"tool"`
	Args     string        `json:"args"`
	Effect   Effect        `json:"effect"`
	Contract WriteContract `json:"contract,omitempty"`
	Key      string        `json:"key,omitempty"`
	Action   ActionID      `json:"action"`
	Call     int           `json:"call"`
	Clock
}

// reissuable reports whether the attempt, with no recorded result, is
// issued again (STM-6).
func (e *AttemptEntry) reissuable() bool { return reissuable(e.Effect, e.Contract) }

// ResultStatus is how a call was resolved (ADR 0004, item 2).
type ResultStatus string

// The result statuses.
const (
	// ResultExecuted: the call ran; Result holds what it returned.
	ResultExecuted ResultStatus = "executed"
	// ResultNotExecuted: a transition ended the turn before the call
	// (STM-3).
	ResultNotExecuted ResultStatus = "not_executed"
	// ResultRefused: the call names no tool offered to the state (MAN-2).
	ResultRefused ResultStatus = "refused"
	// ResultUnknown: a write was attempted and its outcome is unknown
	// (TOOL-12). The run fails with ReasonToolError (MVP.md).
	ResultUnknown ResultStatus = "unknown"
)

// ResultEntry is the payload of a result entry. Result is the artifact of
// what the model receives; Ops are an atomic local transaction's object
// operations, committed with the entry.
type ResultEntry struct {
	Tool    string       `json:"tool"`
	Status  ResultStatus `json:"status"`
	Result  string       `json:"result,omitempty"`
	Ops     []store.Op   `json:"ops,omitempty"`
	Call    int          `json:"call"`
	IsError bool         `json:"is_error,omitempty"`
	Clock
}

// checkpointFormat is the version of checkpointState.
const checkpointFormat = 1

// checkpointState is the library's checkpoint contents (ADR 0004, item 3),
// opaque to the host (RUN-5). Step is the number of the next step to run:
// 1 at the run's start. Budgets are the run's budgets and Consumed what it
// has consumed of them as of the entries the checkpoint covers (ADR 0009);
// older checkpoints, which lack them, read as no budgets and nothing
// consumed.
type checkpointState struct {
	// Model is a model state's progress, nil before its first turn lands.
	Model        *modelProgress `json:"model,omitempty"`
	Budgets      *Budgets       `json:"budgets,omitempty"`
	Consumed     *Consumption   `json:"consumed,omitempty"`
	AgentType    string         `json:"agent_type"`
	AgentVersion string         `json:"agent_version"`
	State        string         `json:"state"`
	Config       string         `json:"config,omitempty"`
	// Namespace is the run's object-store namespace (ADR 0012).
	Namespace string          `json:"namespace"`
	Data      json.RawMessage `json:"data"`
	Step      uint64          `json:"step"`
}

// modelProgress is a model state's transcript at a step boundary (STM-6):
// the request parameters' artifact and each message's artifact, in order.
// Answered means the latest turn called no tool, so the next request
// carries a nudge (LOOP-3); Nudged that the nudge has been sent since the
// latest turn that called a tool. WrapUp counts the state's completed
// wrap-up turns (ADR 0010); a state with any is wrapping up.
type modelProgress struct {
	Params     string   `json:"params"`
	Transcript []string `json:"transcript"`
	WrapUp     int      `json:"wrap_up,omitempty"`
	Answered   bool     `json:"answered,omitempty"`
	Nudged     bool     `json:"nudged,omitempty"`
}
