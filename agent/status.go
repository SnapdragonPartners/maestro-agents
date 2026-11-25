package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Phase is a run's derived phase (ADR 0024, item 7): a closed set, part of
// the external contract, so a new phase is a contract change.
type Phase string

// The phases of a run.
const (
	// PhaseQueued is a runnable run on an instance with no live lease.
	PhaseQueued Phase = "queued"
	// PhaseRunning is a runnable run on an instance with a live lease.
	PhaseRunning Phase = "running"
	// PhaseWaiting is a run waiting for input (STM-8, not built: no run
	// is stored waiting yet).
	PhaseWaiting Phase = "waiting"
	// PhaseStopping is a run with a cancel request that is not yet
	// terminal.
	PhaseStopping Phase = "stopping"
	// PhaseSucceeded, PhaseFailed and PhaseCancelled are the terminal
	// statuses, each with RUN-3's reason.
	PhaseSucceeded Phase = "succeeded"
	PhaseFailed    Phase = "failed"
	PhaseCancelled Phase = "cancelled"
)

// StateKind is the kind of an agent type's state.
type StateKind string

// The kinds of state (ADR 0007, ADR 0008).
const (
	StateCode  StateKind = "code"
	StateModel StateKind = "model"
)

// RunStatus is a run's status, derived from its run record, its cancel
// request, its instance's ownership record, its checkpoint and its
// execution record, and never stored (ADR 0024, item 7; docs/contract.md
// section 2). It is a snapshot assembled from several reads, not one
// transaction: a run that ends while it is read may show its new phase with
// its progress from just before.
type RunStatus struct {
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// LastActivity is the commit time of the run's latest execution-record
	// entry, or the run record's latest change if that is later.
	LastActivity time.Time `json:"last_activity"`
	// CancelRequested is the run's cancel request, nil if it has none.
	CancelRequested *store.CancelRequest `json:"cancel_requested"`
	// State is the current state of a run that is not terminal, nil
	// before its start is committed and once it is terminal.
	State *StateStatus `json:"state"`
	// Budgets are the run's budgets and what it has consumed of them, nil
	// before its start is committed.
	Budgets *BudgetUse `json:"budgets"`
	Run     string     `json:"run"`
	// Instance, AgentType and AgentVersion are the run record's.
	Instance     string `json:"instance"`
	AgentType    string `json:"agent_type"`
	AgentVersion string `json:"agent_version"`
	Phase        Phase  `json:"phase"`
	// Reason is a terminal run's RUN-3 reason, empty otherwise.
	Reason store.Reason `json:"reason"`
	// Requests are the run's open requests (ADR 0024, item 11); always
	// empty until requests are built (#31).
	Requests []RequestStatus `json:"requests"`
	// Artifacts are the run's published artifacts (ADR 0024, item 10);
	// always empty until publishing is built (#30).
	Artifacts []ArtifactStatus `json:"artifacts"`
	// Step is the step of the run's latest execution-record entry.
	Step uint64 `json:"step"`
}

// StateStatus names a run's current state and its kind. Kind is empty for
// a run checkpointed before kinds were recorded.
type StateStatus struct {
	Name string    `json:"name"`
	Kind StateKind `json:"kind,omitempty"`
}

// BudgetUse is a run's budgets (ADR 0009) and what it has consumed of
// them. A zero limit sets none, and is left out; Deadline is nil when the
// run has none. Cost budgets are not built (MVP.md, BUD-1) and not shown.
type BudgetUse struct {
	Deadline   *time.Time `json:"deadline"`
	Tokens     CountUse   `json:"tokens"`
	Turns      CountUse   `json:"turns"`
	ActiveTime TimeUse    `json:"active_time"`
}

// CountUse is a counted budget's consumption and limit.
type CountUse struct {
	Used  int64 `json:"used"`
	Limit int64 `json:"limit,omitempty"`
}

// TimeUse is a time budget's consumption and limit. In JSON each is a Go
// duration string ("2m10s"), and a zero limit is left out.
type TimeUse struct {
	Used  time.Duration
	Limit time.Duration
}

type timeUseJSON struct {
	Used  string `json:"used"`
	Limit string `json:"limit,omitempty"`
}

// MarshalJSON writes the durations as strings.
func (u TimeUse) MarshalJSON() ([]byte, error) {
	j := timeUseJSON{Used: u.Used.String()}
	if u.Limit != 0 {
		j.Limit = u.Limit.String()
	}
	return json.Marshal(j) //nolint:wrapcheck // strings cannot fail to encode
}

// UnmarshalJSON reads the durations MarshalJSON writes.
func (u *TimeUse) UnmarshalJSON(data []byte) error {
	var j timeUseJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err //nolint:wrapcheck // the decoder adds its context
	}
	var out TimeUse
	var err error
	if out.Used, err = time.ParseDuration(j.Used); err != nil {
		return fmt.Errorf("agent: active time used: %w", err)
	}
	if j.Limit != "" {
		if out.Limit, err = time.ParseDuration(j.Limit); err != nil {
			return fmt.Errorf("agent: active time limit: %w", err)
		}
	}
	*u = out
	return nil
}

// RequestStatus is an open request as a status lists it (ADR 0024, item
// 11). Requests are not built yet (#31).
type RequestStatus struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

// ArtifactStatus is a published artifact as a status lists it (ADR 0024,
// item 10). Publishing is not built yet (#30).
type ArtifactStatus struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// Status derives a run's status (ADR 0024, item 7). Its work is bounded by
// the run, never by its instance's history: for a run that is not
// terminal, the instance's latest checkpoint, if it is the run's, and the
// entries from the one it covers; for a terminal run, or one with no
// checkpoint yet, the run's execution record. A run the reader does not
// have returns store.ErrNotFound.
func Status(ctx context.Context, r store.Reader, runID string) (RunStatus, error) {
	run, err := r.Run(ctx, runID)
	if err != nil {
		return RunStatus{}, fmt.Errorf("agent: status of run %q: %w", runID, err)
	}
	return runStatus(ctx, r, &run)
}

func runStatus(ctx context.Context, r store.Reader, run *store.Run) (RunStatus, error) {
	st := RunStatus{
		Run: run.ID, Instance: run.Instance, AgentType: run.AgentType, AgentVersion: run.AgentVersion,
		CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt, LastActivity: run.UpdatedAt,
		Requests: []RequestStatus{}, Artifacts: []ArtifactStatus{},
	}
	req, ok, err := r.CancelRequest(ctx, run.ID)
	if err != nil {
		return RunStatus{}, fmt.Errorf("agent: status of run %q: %w", run.ID, err)
	}
	if ok {
		st.CancelRequested = &req
	}
	switch {
	case run.Status.Terminal():
		st.Phase, st.Reason = Phase(run.Status), run.Reason
	case ok:
		st.Phase = PhaseStopping
	case run.Status == store.StatusWaiting:
		st.Phase = PhaseWaiting
	default:
		own, err := r.Ownership(ctx, run.Instance)
		if err != nil {
			return RunStatus{}, fmt.Errorf("agent: status of run %q: %w", run.ID, err)
		}
		st.Phase = PhaseQueued
		if own.Live(own.AsOf) {
			st.Phase = PhaseRunning
		}
	}
	if err := progress(ctx, r, run, &st); err != nil {
		return RunStatus{}, fmt.Errorf("agent: status of run %q: %w", run.ID, err)
	}
	return st, nil
}

// progress fills a status's state, step, budgets and last activity.
func progress(ctx context.Context, r store.Reader, run *store.Run, st *RunStatus) error {
	var cs *checkpointState
	var covered uint64
	if !run.Status.Terminal() {
		var err error
		if cs, covered, err = ownCheckpoint(ctx, r, run); err != nil {
			return err
		}
	}
	// Read from the entry the checkpoint covers, for its time and step.
	after := uint64(0)
	if covered > 0 {
		after = covered - 1
	}
	es, err := r.Entries(ctx, run.ID, after)
	if err != nil {
		return err //nolint:wrapcheck // the caller adds its context
	}
	started, err := foldEntries(es, st)
	if err != nil {
		return err
	}
	var used Consumption
	limits := started
	switch {
	case cs != nil:
		st.State = &StateStatus{Name: cs.State, Kind: cs.StateKind}
		st.Step = max(st.Step, cs.Step)
		limits = &budgetsAt{budgets: cs.Budgets}
		used = tailConsumption(cs, es, covered)
	case run.Status.Terminal() && len(run.Consumed) > 0:
		if err := json.Unmarshal(run.Consumed, &used); err != nil {
			return fmt.Errorf("decode consumption: %w", err)
		}
	}
	if limits != nil {
		st.Budgets = budgetStatus(limits.budgets, &used)
	}
	return nil
}

// budgetsAt holds the budgets a run's start recorded, nil if it set none.
type budgetsAt struct {
	budgets *Budgets
}

// ownCheckpoint returns the instance's latest checkpoint and the entry it
// covers, if it is the run's in the format this version reads.
func ownCheckpoint(ctx context.Context, r store.Reader, run *store.Run) (*checkpointState, uint64, error) {
	cp, ok, err := r.LatestCheckpoint(ctx, run.Instance)
	if err != nil {
		return nil, 0, err //nolint:wrapcheck // the caller adds its context
	}
	if !ok || cp.Scope != (store.Scope{Kind: store.ScopeRun, ID: run.ID}) || cp.Format != checkpointFormat {
		return nil, 0, nil
	}
	var s checkpointState
	if err := json.Unmarshal(cp.State, &s); err != nil {
		return nil, 0, fmt.Errorf("decode checkpoint %d: %w", cp.Seq, err)
	}
	return &s, cp.RecordSeq, nil
}

// foldEntries takes the step and last activity from the entries, and
// returns the budgets of the run_started entry among them, nil if there is
// none.
func foldEntries(es []store.Entry, st *RunStatus) (*budgetsAt, error) {
	var started *budgetsAt
	for i := range es {
		e := &es[i]
		st.Step = max(st.Step, e.Step)
		if e.At.After(st.LastActivity) {
			st.LastActivity = e.At
		}
		if e.Kind == EntryRunStarted {
			var rs RunStarted
			if err := decode(e, &rs); err != nil {
				return nil, fmt.Errorf("entry %d: %w", e.Seq, err)
			}
			started = &budgetsAt{budgets: rs.Budgets}
		}
	}
	return started, nil
}

// tailConsumption is the run's consumption as of its latest entry: the
// checkpoint's, charged the tail's model attempts as recovery charges them
// (ADR 0009). A tail the fold refuses (the run ended between the reads, or
// a record this version cannot read) leaves the checkpoint's.
func tailConsumption(cs *checkpointState, es []store.Entry, covered uint64) Consumption {
	var base Consumption
	if cs.Consumed != nil {
		base = cs.Consumed.clone()
	}
	used := base.clone()
	f := &tailFold{used: &used}
	for i := range es {
		if es[i].Seq <= covered {
			continue
		}
		if err := f.add(&es[i]); err != nil {
			return base
		}
	}
	f.interrupt()
	return used
}

func budgetStatus(b *Budgets, used *Consumption) *BudgetUse {
	var limits Budgets
	if b != nil {
		limits = *b
	}
	out := &BudgetUse{
		Tokens:     CountUse{Used: used.Tokens, Limit: limits.Tokens.Limit},
		Turns:      CountUse{Used: int64(used.Turns), Limit: int64(limits.Turns)},
		ActiveTime: TimeUse{Used: used.ActiveTime, Limit: limits.ActiveTime},
	}
	if !limits.Deadline.IsZero() {
		d := limits.Deadline
		out.Deadline = &d
	}
	return out
}

// InstanceReport is an instance's status, as InstanceStatus derives it, for agents that run continuously
// (ADR 0024, item 7; docs/contract.md section 2).
type InstanceReport struct {
	// Active is the status of the instance's run that is not terminal,
	// nil if it has none.
	Active *RunStatus `json:"active"`
	// LastSucceeded and LastFailed are the instance's newest succeeded and
	// failed runs, nil if it has none. A cancelled run is neither.
	LastSucceeded *RunSummary `json:"last_succeeded"`
	LastFailed    *RunSummary `json:"last_failed"`
	// Queued are the instance's queued triggers.
	Queued   QueuedTriggers `json:"queued"`
	Instance string         `json:"instance"`
	// Requests are the instance's open requests; always empty until
	// requests are built (#31).
	Requests []RequestStatus `json:"requests"`
}

// RunSummary is a finished run as an instance's status names it: its ID,
// when it ended (the run record's last change) and its reason.
type RunSummary struct {
	At     time.Time    `json:"at"`
	Run    string       `json:"run"`
	Reason store.Reason `json:"reason"`
}

// QueuedTriggers counts an instance's queued triggers and gives the oldest
// one's enqueue time, nil when none is queued.
type QueuedTriggers struct {
	Oldest *time.Time `json:"oldest"`
	Count  int        `json:"count"`
}

// InstanceStatus derives an instance's status (ADR 0024, item 7): its
// active run's status, its last succeeded and last failed runs (each one
// newest-first, filtered query of Reader.Runs) and its queued triggers.
// An instance the reader has no records of has an empty status.
func InstanceStatus(ctx context.Context, r store.Reader, instance string) (InstanceReport, error) {
	out := InstanceReport{Instance: instance, Requests: []RequestStatus{}}
	wrap := func(err error) error { return fmt.Errorf("agent: status of instance %q: %w", instance, err) }
	run, ok, err := r.ActiveRun(ctx, instance)
	if err != nil {
		return InstanceReport{}, wrap(err)
	}
	if ok {
		st, serr := runStatus(ctx, r, &run)
		if serr != nil {
			return InstanceReport{}, wrap(serr)
		}
		out.Active = &st
	}
	for _, last := range []struct {
		dst    **RunSummary
		status store.Status
	}{{&out.LastSucceeded, store.StatusSucceeded}, {&out.LastFailed, store.StatusFailed}} {
		runs, _, rerr := r.Runs(ctx, instance, store.RunQuery{Status: []store.Status{last.status}, Newest: true, Limit: 1})
		if rerr != nil {
			return InstanceReport{}, wrap(rerr)
		}
		if len(runs) > 0 {
			*last.dst = &RunSummary{Run: runs[0].ID, At: runs[0].UpdatedAt, Reason: runs[0].Reason}
		}
	}
	tr, err := r.Triggers(ctx, instance)
	if err != nil {
		return InstanceReport{}, wrap(err)
	}
	out.Queued.Count = len(tr)
	if len(tr) > 0 {
		t := tr[0].EnqueuedAt
		out.Queued.Oldest = &t
	}
	return out, nil
}
