package store

import (
	"context"
	"time"
)

// Store is everything a host provides for the library's durable records
// (HOST-3, STORE-1). Implementations are safe for concurrent use.
type Store interface {
	Committer
	ControlPlane
	Reader

	// Close releases the store's resources. A closed store returns
	// [ErrUnusable] from every method.
	Close() error
}

// Committer is the single write path for an activation (ADR 0005, item 1).
type Committer interface {
	// Commit applies ops atomically, fenced on f: the store reads the
	// instance's ownership record, compares it with the fence and applies
	// the operations with no claim able to commit in between. A mismatch
	// returns [ErrOwnership]. Preconditions are checked in operation order,
	// each against the store as updated by the commit's earlier
	// operations; a failure returns a [*ConflictError] naming the
	// operation. An operation on another instance's records returns
	// [ErrInvalid]. In each of those cases nothing was applied. Any other
	// error is indeterminate ([IsDefinite]).
	//
	// A commit whose operations change nothing (re-storing an artifact
	// already stored and already linked to the fenced instance) is fenced
	// and checked all the same. Re-storing one for an instance not yet
	// linked to it is a write: it links it (ADR 0024, item 9). The reference
	// stores then return without writing, so such a commit cannot fail
	// after those checks; that is their optimization, not part of this
	// contract: like any commit, a host store's may return an
	// indeterminate error.
	Commit(ctx context.Context, f Fence, ops []Op) error
}

// ControlPlane holds the writes made outside an activation, each a single
// compare-and-set with no fence (ADR 0005, item 4).
type ControlPlane interface {
	// Claim takes ownership of an instance for owner, with a lease of the
	// given length by the store's clock. It succeeds only if the instance
	// has no live owner, and increments the epoch; otherwise it returns
	// [ErrNotClaimable]. A claim is serialized with commits.
	Claim(ctx context.Context, instance, owner string, lease time.Duration) (Ownership, error)

	// Heartbeat extends the lease of the fenced owner, which may already
	// have expired as long as nobody has claimed since. A moved epoch or
	// owner returns [ErrOwnership]. It also returns the cancel request of
	// the instance's non-terminal run, or nil if it has none, so the
	// owner cancels its activation with the request's cause (ADR 0024,
	// item 6).
	Heartbeat(ctx context.Context, f Fence, lease time.Duration) (Ownership, *CancelRequest, error)

	// Release clears the fenced owner, keeping the epoch. A host uses it
	// to give up a claim after an activation's store failure (ADR 0003,
	// item 14); an activation releases in its final commit instead
	// ([ReleaseOp]). A moved epoch or owner returns [ErrOwnership].
	Release(ctx context.Context, f Fence) error

	// EnqueueTrigger queues a trigger for its instance, idempotently on
	// (instance, ID): a trigger already queued is returned as stored. The
	// store assigns Seq and EnqueuedAt.
	EnqueueTrigger(ctx context.Context, t Trigger) (Trigger, error)

	// RemoveTrigger removes a queued trigger once its run exists. Removing
	// one that is not queued is not an error.
	RemoveTrigger(ctx context.Context, instance, id string) error

	// CreateRun creates a run record, conditionally on (instance,
	// TriggerID). If a run already exists for that trigger it is returned
	// with created false, whatever r's other fields say. Otherwise, if the
	// instance has a non-terminal run, it returns [ErrInstanceBusy]; a run
	// ID already in use returns [ErrInvalid]. A new run is created
	// runnable at version 1, with r's Status, Reason, Version and
	// timestamps ignored.
	CreateRun(ctx context.Context, r Run) (run Run, created bool, err error)

	// RequestCancel records a cancel request for a run (ADR 0024, item
	// 6), with no fence: anyone may ask, whoever executes the run. It is
	// idempotent and the first cause stands: if the run has a request it
	// is returned unchanged, whatever cause is given. A run already
	// terminal is left alone: nothing is recorded, and recorded is false
	// (unless it has an earlier request, which is returned). The request
	// is serialized with commits, so a run's final commit either lands
	// first, and the run keeps its outcome, or lands after the request,
	// which its owner then finds at the next heartbeat or activation. It
	// changes neither the run record nor its version. A cause that is not
	// one of the three returns [ErrInvalid]; an unknown run returns
	// [ErrNotFound].
	RequestCancel(ctx context.Context, run string, cause CancelCause) (req CancelRequest, recorded bool, err error)
}

// Reader holds the unfenced reads (ADR 0005, item 5). An owner's reads,
// after its claim succeeded, of its own instance's records (ownership,
// runs, checkpoints, execution records) and of its artifacts are fresh: they
// reflect every commit made before the claim and the owner's own commits
// since. Reads of objects may be stale, except as [ConflictError] states.
type Reader interface {
	// Ownership returns the instance's ownership record, its AsOf the
	// store's clock when it was read, by which a reader judges the lease
	// live (ADR 0024, item 7). An instance never claimed has no owner and
	// epoch 0.
	Ownership(ctx context.Context, instance string) (Ownership, error)

	// Claimable returns the ready runs: runnable runs on instances with no
	// live owner (ADR 0003, items 7 and 11), ordered by instance.
	Claimable(ctx context.Context) ([]Claimable, error)

	// Run returns a run record, or [ErrNotFound].
	Run(ctx context.Context, id string) (Run, error)

	// CancelRequest returns the run's cancel request, if it has one
	// (ADR 0024, item 6). An unknown run returns [ErrNotFound].
	CancelRequest(ctx context.Context, run string) (req CancelRequest, ok bool, err error)

	// ActiveRun returns the instance's non-terminal run, if it has one.
	ActiveRun(ctx context.Context, instance string) (run Run, ok bool, err error)

	// Runs returns a page of the instance's runs as q selects them, and
	// whether more follow it (ADR 0024, item 7). An instance with no runs
	// has none.
	Runs(ctx context.Context, instance string, q RunQuery) (runs []Run, more bool, err error)

	// Triggers returns the instance's queued triggers in arrival order.
	Triggers(ctx context.Context, instance string) ([]Trigger, error)

	// LatestCheckpoint returns the instance's checkpoint with the highest
	// sequence number, if it has any.
	LatestCheckpoint(ctx context.Context, instance string) (c Checkpoint, ok bool, err error)

	// Checkpoint returns one checkpoint, or [ErrNotFound].
	Checkpoint(ctx context.Context, instance string, seq uint64) (Checkpoint, error)

	// Entries returns the run's execution-record entries with sequence
	// numbers greater than after, in order, each with its commit time
	// ([Entry.At]).
	Entries(ctx context.Context, run string, after uint64) ([]Entry, error)

	// Artifact returns an artifact's metadata and content, if its digest
	// is linked to the instance (ADR 0024, item 9): a [PutArtifact] in a
	// commit fenced on that instance links it. A digest not linked to the
	// instance returns [ErrNotFound], exactly as one never stored, so
	// knowing a digest never reaches another instance's content. An
	// instance or digest the store cannot hold (an empty instance, a
	// digest not 64 lowercase hex characters) returns [ErrInvalid].
	Artifact(ctx context.Context, instance, digest string) (ArtifactMeta, []byte, error)

	// Object returns an object. For an absent key ok is false and the
	// returned Version is the one a creation must expect.
	Object(ctx context.Context, namespace, key string) (obj Object, ok bool, err error)

	// ListObjects returns the keys in a namespace that start with prefix,
	// with their versions, ordered by key.
	ListObjects(ctx context.Context, namespace, prefix string) ([]ObjectInfo, error)
}

// Telemetry is the optional telemetry storage a store may offer (STORE-1,
// ADR 0016). It is not part of [Store]: telemetry is written by the host's
// telemetry sink, not through Commit, and is never fenced, because it is an
// observation, not state (HOST-1, REC-2). Both reference stores implement
// it; a host finds it by type assertion.
type Telemetry interface {
	// AppendEvents stores events idempotently on (Run, Seq): an event
	// already stored is kept as it was, and repeating it is not an
	// error. Events are validated before any is written: a run that is
	// not a valid ID, a zero Seq, a kind that is not valid UTF-8 or a
	// payload that is not valid JSON returns [ErrInvalid] and writes
	// nothing.
	AppendEvents(ctx context.Context, events []Event) error

	// Events returns the run's events with sequence numbers greater than
	// after, in order.
	Events(ctx context.Context, run string, after uint64) ([]Event, error)
}

// PausePoint names one of TEST-6's two pause points inside a commit
// (ADR 0005, item 6).
type PausePoint int

// The two pause points.
const (
	// PauseBeforeCommit is reached before the commit starts: before it
	// takes any lock or reads the ownership record.
	PauseBeforeCommit PausePoint = iota + 1
	// PauseAfterOwnershipRead is reached after the commit has read the
	// ownership record and before it checks the fence or writes, while it
	// holds whatever serializes it against a claim.
	PauseAfterOwnershipRead
)

func (p PausePoint) String() string {
	switch p {
	case PauseBeforeCommit:
		return "before-commit"
	case PauseAfterOwnershipRead:
		return "after-ownership-read"
	}
	return "unknown"
}

// PauseHook is the test-only hook a durable store adapter accepts as a
// construction option, called at each pause point of every commit. A hook
// that blocks pauses the commit. Production code never sets one.
type PauseHook func(ctx context.Context, p PausePoint, f Fence)
