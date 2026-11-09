package agent

import (
	"encoding/json"
	"errors"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Reason codes (RUN-3). Each is valid for one status only.
const (
	// ReasonStepLimit: runnable; the activation stopped at the host's step
	// limit (STM-7).
	ReasonStepLimit = "step_limit"
	// ReasonShutdown: runnable; the activation stopped because its
	// context ended for a reason other than cancellation or a budget
	// (MODE-6).
	ReasonShutdown = "shutdown"

	// ReasonChanged: succeeded; the run made its change.
	ReasonChanged = "changed"
	// ReasonNoChange: succeeded; nothing needed doing.
	ReasonNoChange = "no_change"

	// ReasonBudgetExhausted: failed; a budget ran out (Reason.Budget names
	// it). Retrying as-is cannot help.
	ReasonBudgetExhausted = "budget_exhausted"
	// ReasonBudgetUnenforceable: failed; a budget cannot be enforced or
	// computed. A configuration error: retrying as-is cannot help.
	ReasonBudgetUnenforceable = "budget_unenforceable"
	// ReasonModelError: failed; the model call failed, or a model state
	// was reached with no model client (ADR 0008).
	ReasonModelError = "model_error"
	// ReasonToolError: failed; a tool failed in a way the run cannot
	// continue from: its Execute returned an error or panicked, it returned
	// a result the runtime cannot record, a write's outcome is unknown, or
	// the policy hook failed or asked for approval (ADR 0008).
	ReasonToolError = "tool_error"
	// ReasonInvalidOutput: failed; the agent type judged its output
	// invalid.
	ReasonInvalidOutput = "invalid_output"
	// ReasonWrapUpFailed: failed; a model state's completion policy did not
	// produce a transition (LOOP-3): the model answered without calling a
	// tool again after it was nudged, or a forced wrap-up failed.
	ReasonWrapUpFailed = "wrap_up_failed"
	// ReasonObjectConflict: failed; a code state's object operations still
	// conflicted after the retry bound (ADR 0006, item 6). Retrying can
	// help.
	ReasonObjectConflict = "object_conflict"
	// ReasonStateError: failed; a code state or the agent type's Init or
	// Migrate returned an error, panicked, chose an undeclared transition
	// or returned operations it may not (ADR 0007). A defect in the agent
	// type or its input: retrying as-is cannot help.
	ReasonStateError = "state_error"

	// ReasonSuperseded: cancelled; the host superseded the run.
	ReasonSuperseded = "superseded"
	// ReasonOperator: cancelled; an operator cancelled the run.
	ReasonOperator = "operator"
)

// Cancellation causes. A host cancels an activation with
// context.WithCancelCause and one of these (or an error wrapping one) to
// cancel the run (RUN-2); any other cause, or none, is a shutdown, which
// keeps the run's disposition (ADR 0003, item 17).
var (
	ErrSuperseded = errors.New("agent: run superseded")
	ErrOperator   = errors.New("agent: run cancelled by an operator")
)

// BudgetExhausted is the context cause with which the runtime's time
// budgets end an activation (BUD-2): the run fails with
// ReasonBudgetExhausted naming Budget.
type BudgetExhausted struct {
	Budget string
}

func (e *BudgetExhausted) Error() string { return "agent: budget exhausted: " + e.Budget }

// Errors Activate returns. A non-nil error means the activation has no
// confirmed outcome (ADR 0003, item 14): before acting on it the host
// rereads the run record, because an indeterminate commit may have landed.
var (
	// ErrOwnershipLost: a commit was rejected because the instance's
	// owner epoch moved (HOST-6). The run's status is whatever its new
	// owner commits.
	ErrOwnershipLost = errors.New("agent: ownership lost")
	// ErrStoreFailure: a durable write or read failed, or a write's result
	// is indeterminate. It wraps the cause. It is not a run failure (ADR
	// 0003, item 15).
	ErrStoreFailure = errors.New("agent: store failure")
	// ErrNotActivatable: the activation was refused before it wrote
	// anything: the run does not exist, belongs to another instance, is
	// terminal or waiting, or names an agent type the registry lacks.
	ErrNotActivatable = errors.New("agent: run cannot be activated")
	// ErrInvalidType: an agent type's declaration is malformed (STM-1).
	ErrInvalidType = errors.New("agent: invalid agent type")
)

// ReadError wraps a failure of a store read made through an Env. A code
// state that returns an error wrapping one ends the activation with a store
// failure rather than failing the run (ADR 0003, item 15).
type ReadError struct {
	Err error
}

func (e *ReadError) Error() string { return "agent: store read failed: " + e.Err.Error() }

func (e *ReadError) Unwrap() error { return e.Err }

// Outcome is an activation's confirmed result (RUN-3, ADR 0003 item 13):
// what the activation durably committed, in the commit that released the
// instance.
type Outcome struct {
	Status store.Status
	Reason store.Reason
	// Detail explains a failure, such as a code state's error; it is also
	// in the execution record's outcome entry.
	Detail string
	// Consumed holds the run's consumed budgets. Budgets are not tracked
	// yet; it is nil.
	Consumed json.RawMessage
	// Steps is the number of steps the run has completed.
	Steps uint64
	// CheckpointSeq is the run's latest checkpoint sequence.
	CheckpointSeq uint64
}
