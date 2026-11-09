package agent

import (
	"encoding/json"

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

// ActivationStarted is the payload of an activation_started entry: the
// claim and the starting point, committed before anything else the
// activation does. CheckpointSeq and RecordSeq are zero for a run's first
// activation.
type ActivationStarted struct {
	Owner         string `json:"owner"`
	Epoch         uint64 `json:"epoch"`
	CheckpointSeq uint64 `json:"checkpoint_seq"`
	RecordSeq     uint64 `json:"record_seq"`
}

// RunStarted is the payload of a run_started entry (REC-1). Config is the
// digest of the resolved configuration's artifact, empty if the run has
// none.
type RunStarted struct {
	AgentType    string          `json:"agent_type"`
	AgentVersion string          `json:"agent_version"`
	TriggerID    string          `json:"trigger_id"`
	Config       string          `json:"config,omitempty"`
	Trigger      json.RawMessage `json:"trigger,omitempty"`
}

// TransitionEntry is the payload of a transition entry: the state left and
// entered, the new state data and the object operations committed with it.
type TransitionEntry struct {
	From string          `json:"from"`
	To   string          `json:"to"`
	Data json.RawMessage `json:"data"`
	Ops  []store.Op      `json:"ops,omitempty"`
}

// OutcomeEntry is the payload of an outcome entry: the status and reason
// the activation ended with (RUN-3), and a failure's detail.
type OutcomeEntry struct {
	Status store.Status `json:"status"`
	Reason store.Reason `json:"reason"`
	Detail string       `json:"detail,omitempty"`
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
type ModelAttemptEntry struct {
	Model    ModelRef `json:"model"`
	State    string   `json:"state"`
	Params   string   `json:"params"`
	Messages []string `json:"messages,omitempty"`
	Base     int      `json:"base"`
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
// a provider error, empty for any other error.
type ModelFailureEntry struct {
	Kind    string `json:"kind,omitempty"`
	Message string `json:"message"`
	Status  int    `json:"status,omitempty"`
}

// PermissionEntry is the payload of a permission entry: a call's
// permission and the policy decision behind it (STM-6, TOOL-3).
type PermissionEntry struct {
	Tool       string     `json:"tool"`
	Permission Permission `json:"permission"`
	Reason     string     `json:"reason,omitempty"`
	Call       int        `json:"call"`
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
}

// checkpointFormat is the version of checkpointState.
const checkpointFormat = 1

// checkpointState is the library's checkpoint contents (ADR 0004, item 3),
// opaque to the host (RUN-5). Step is the number of the next step to run:
// 1 at the run's start.
type checkpointState struct {
	// Model is a model state's progress, nil before its first turn lands.
	Model        *modelProgress  `json:"model,omitempty"`
	AgentType    string          `json:"agent_type"`
	AgentVersion string          `json:"agent_version"`
	State        string          `json:"state"`
	Config       string          `json:"config,omitempty"`
	Data         json.RawMessage `json:"data"`
	Step         uint64          `json:"step"`
}

// modelProgress is a model state's transcript at a step boundary (STM-6):
// the request parameters' artifact and each message's artifact, in order.
// Answered means the latest turn called no tool, so the next request
// carries a nudge (LOOP-3); Nudged that the nudge has been sent since the
// latest turn that called a tool.
type modelProgress struct {
	Params     string   `json:"params"`
	Transcript []string `json:"transcript"`
	Answered   bool     `json:"answered,omitempty"`
	Nudged     bool     `json:"nudged,omitempty"`
}
