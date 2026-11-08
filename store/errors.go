package store

import (
	"errors"
	"fmt"
)

// Errors a store returns. Each is matched with [errors.Is].
var (
	// ErrOwnership rejects a fenced write because the instance's ownership
	// record no longer names the fence's owner and epoch (HOST-6; ADR
	// 0003's "ownership lost"). Nothing was applied.
	ErrOwnership = errors.New("store: ownership lost")

	// ErrInvalid rejects a malformed request: a malformed operation, an
	// empty or oversized identifier, an operation outside the fenced
	// instance, or a transition the store forbids. It is a defect in the
	// caller. Nothing was applied.
	ErrInvalid = errors.New("store: invalid request")

	// ErrNotFound reports that a record does not exist.
	ErrNotFound = errors.New("store: not found")

	// ErrNotClaimable rejects a claim because the instance has a live
	// owner (ADR 0003, item 8).
	ErrNotClaimable = errors.New("store: instance has a live owner")

	// ErrInstanceBusy rejects the creation of a run on an instance that
	// already has a non-terminal run for a different trigger (ADR 0003,
	// item 2).
	ErrInstanceBusy = errors.New("store: instance has a non-terminal run")

	// ErrUnusable is returned by a store that must be closed and reopened:
	// one closed already, or one that a failure after a commit point has
	// left unable to serve anything until it is reopened (ADR 0005,
	// item 14).
	ErrUnusable = errors.New("store: unusable until reopened")
)

// ConflictError rejects a commit because an operation's precondition
// failed. Nothing was applied.
//
// Only an object-version conflict is expected: objects are shared between
// instances, and the caller re-runs the work against fresh reads (ADR
// 0005, items 3 and 10). For one, Current is the object's version when the
// commit was checked, which is what a write must expect (for an absent key,
// the version it would be created over). A read the same store serves after
// the conflict observes at least that version. Any other conflict concerns
// a record only the owner writes, and means the runtime or the store has
// broken its contract.
type ConflictError struct {
	Kind   OpKind
	Detail string
	// Op is the failed operation's index in the commit.
	Op      int
	Current uint64
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("store: conflict in operation %d (%s): %s", e.Op, e.Kind, e.Detail)
}

// IsObjectConflict reports whether the error is a conflict on an object
// operation: the one kind of conflict that arises in normal operation.
func (e *ConflictError) IsObjectConflict() bool {
	return e.Kind == OpPutObject || e.Kind == OpDeleteObject
}

// IsDefinite reports whether a write (a commit or a control-plane write)
// that returned err definitely landed (nil) or definitely did not:
// [ErrOwnership], [ErrInvalid], [ErrNotClaimable], [ErrInstanceBusy] and a
// [*ConflictError]. Any other error is indeterminate: the write may have
// landed, and the caller learns what happened by reading durable state
// (ADR 0005, item 3).
func IsDefinite(err error) bool {
	var conflict *ConflictError
	return err == nil || errors.Is(err, ErrOwnership) || errors.Is(err, ErrInvalid) ||
		errors.Is(err, ErrNotClaimable) || errors.Is(err, ErrInstanceBusy) ||
		errors.As(err, &conflict)
}
