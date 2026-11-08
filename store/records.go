package store

import (
	"encoding/json"
	"time"
)

// Fence identifies the claim an activation holds: the instance, the owner
// that claimed it and the owner epoch the claim produced (ADR 0003, item 8).
// Every commit carries one, and a store rejects the commit unless the
// instance's ownership record still names this owner at this epoch.
type Fence struct {
	Instance string `json:"instance"`
	Owner    string `json:"owner"`
	Epoch    uint64 `json:"epoch"`
}

// Ownership is an instance's ownership record (STORE-1): its current owner,
// the owner epoch and the lease expiry. Owner is empty when nobody owns the
// instance; the epoch is kept, and never decreases.
type Ownership struct {
	// LeaseExpiry is when the owner's lease ends, by the store's clock. It
	// is zero when there is no owner, and also after a file store is
	// reopened, which treats every recorded owner as having no lease
	// (ADR 0005, item 16).
	LeaseExpiry time.Time `json:"lease_expiry"`
	Instance    string    `json:"instance"`
	Owner       string    `json:"owner,omitempty"`
	Epoch       uint64    `json:"epoch"`
}

// Live reports whether the instance has an owner whose lease has not
// expired at now (ADR 0003, item 7).
func (o Ownership) Live(now time.Time) bool {
	return o.Owner != "" && now.Before(o.LeaseExpiry)
}

// Fence returns the fence a commit under this ownership carries.
func (o Ownership) Fence() Fence {
	return Fence{Instance: o.Instance, Owner: o.Owner, Epoch: o.Epoch}
}

// Status is a run's stored status (RUN-3). There is no stored running
// status: whether a runnable run is executing is derived from the
// instance's ownership record (ADR 0003, items 6 and 7).
type Status string

// The five run statuses of RUN-3.
const (
	StatusRunnable  Status = "runnable"
	StatusWaiting   Status = "waiting"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Valid reports whether s is one of the five statuses.
func (s Status) Valid() bool {
	switch s {
	case StatusRunnable, StatusWaiting, StatusSucceeded, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// Terminal reports whether s is final: succeeded, failed or cancelled.
func (s Status) Terminal() bool {
	return s == StatusSucceeded || s == StatusFailed || s == StatusCancelled
}

// Reason qualifies a status (RUN-3). The store keeps it as given; which
// codes are valid for which status is the runtime's contract, not the
// store's. Budget names the budget for a budget_exhausted failure.
type Reason struct {
	Code   string `json:"code,omitempty"`
	Budget string `json:"budget,omitempty"`
}

// Run is a run record (STORE-1).
type Run struct {
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// ID is the host-assigned run ID, unique within the store.
	ID string `json:"id"`
	// Instance is the agent instance the run belongs to. An instance has
	// at most one non-terminal run (ADR 0003, item 2).
	Instance     string `json:"instance"`
	AgentType    string `json:"agent_type"`
	AgentVersion string `json:"agent_version,omitempty"`
	// TriggerID is the trigger's identity. A run is created conditionally
	// on it, per instance, so a retried creation finds the existing run.
	TriggerID string `json:"trigger_id"`
	Status    Status `json:"status"`
	Reason    Reason `json:"reason"`
	// Trigger is the trigger's payload.
	Trigger json.RawMessage `json:"trigger,omitempty"`
	// Consumed holds the run's consumed budgets, in the runtime's format.
	Consumed json.RawMessage `json:"consumed,omitempty"`
	// Version is the run record's version, incremented by every change.
	// A run-record change names the version it expects.
	Version uint64 `json:"version"`
}

// ScopeKind is the kind of a scope (ADR 0003, item 1).
type ScopeKind string

// ScopeRun is the scope of a run; its ID is the run ID. A host chat turn
// (LOOP-4) adds a kind later.
const ScopeRun ScopeKind = "run"

// Scope identifies what a checkpoint, an event or an action belongs to
// (ADR 0003, item 1).
type Scope struct {
	Kind ScopeKind `json:"kind"`
	ID   string    `json:"id"`
}

// Checkpoint is a snapshot of a run's execution record at a step boundary
// (ADR 0004, item 3). The envelope is the library's; State is opaque to
// the host and the store (RUN-5). Checkpoint sequences are per instance
// and start at 1.
type Checkpoint struct {
	Instance string `json:"instance"`
	Scope    Scope  `json:"scope"`
	// State is the agent type's checkpointed state, in the runtime's
	// format.
	State json.RawMessage `json:"state"`
	Seq   uint64          `json:"seq"`
	// RecordSeq is the execution-record sequence number the checkpoint
	// covers.
	RecordSeq uint64 `json:"record_seq"`
	// Format is the checkpoint format version.
	Format int `json:"format"`
}

// Entry is one execution-record entry (ADR 0004, item 1). Entries are
// numbered per run from 1 with no gaps. Kind and Payload are the
// runtime's; the store keeps them as given.
type Entry struct {
	Run     string          `json:"run"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Seq     uint64          `json:"seq"`
	Step    uint64          `json:"step"`
}

// ArtifactMeta describes a stored artifact: a content-addressed,
// write-once blob (STORE-1).
type ArtifactMeta struct {
	// Digest is the lowercase hex SHA-256 of the content.
	Digest    string `json:"digest"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
}

// Object is a value in a namespace's object store (STORE-2). Exactly one of
// JSON and Bytes is set.
type Object struct {
	Namespace string          `json:"namespace"`
	Key       string          `json:"key"`
	JSON      json.RawMessage `json:"json,omitempty"`
	// Bytes is null when unset and "" when an empty byte value.
	Bytes []byte `json:"bytes"`
	// Version is the object's version. Versions never repeat for a key,
	// across deletion and re-creation.
	Version uint64 `json:"version"`
}

// ObjectInfo is a key and its version, as a prefix listing returns them.
type ObjectInfo struct {
	Key     string `json:"key"`
	Version uint64 `json:"version"`
}

// Trigger is a queued trigger (STORE-1, MODE-4): one that arrived while its
// instance had a run in flight.
type Trigger struct {
	EnqueuedAt time.Time `json:"enqueued_at"`
	Instance   string    `json:"instance"`
	// ID is the trigger's identity; enqueueing is idempotent on it.
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload,omitempty"`
	// Seq orders an instance's queued triggers by arrival.
	Seq uint64 `json:"seq"`
}

// Claimable names a ready run: one that is runnable on an instance with no
// live owner (ADR 0003, item 7).
type Claimable struct {
	Instance string `json:"instance"`
	Run      string `json:"run"`
}
