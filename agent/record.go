package agent

import (
	"encoding/json"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Execution-record entry kinds this package writes (ADR 0004, item 2). The
// model-state kinds (model_attempt, turn, model_failure, permission,
// attempt, result) arrive with model states.
const (
	EntryActivationStarted = "activation_started"
	EntryRunStarted        = "run_started"
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

// checkpointFormat is the version of checkpointState.
const checkpointFormat = 1

// checkpointState is the library's checkpoint contents (ADR 0004, item 3),
// opaque to the host (RUN-5). Step is the number of the next step to run:
// 1 at the run's start.
type checkpointState struct {
	AgentType    string          `json:"agent_type"`
	AgentVersion string          `json:"agent_version"`
	State        string          `json:"state"`
	Config       string          `json:"config,omitempty"`
	Data         json.RawMessage `json:"data"`
	Step         uint64          `json:"step"`
}
