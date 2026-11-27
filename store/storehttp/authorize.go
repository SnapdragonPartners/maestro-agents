package storehttp

import (
	"context"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Operation names the store method (or status route) a call serves.
type Operation string

// The operations, one per route.
const (
	OpCommit           Operation = "Commit"
	OpClaim            Operation = "Claim"
	OpHeartbeat        Operation = "Heartbeat"
	OpRelease          Operation = "Release"
	OpEnqueueTrigger   Operation = "EnqueueTrigger"
	OpRemoveTrigger    Operation = "RemoveTrigger"
	OpCreateRun        Operation = "CreateRun"
	OpRequestCancel    Operation = "RequestCancel"
	OpOwnership        Operation = "Ownership"
	OpClaimable        Operation = "Claimable"
	OpRun              Operation = "Run"
	OpCancelRequest    Operation = "CancelRequest"
	OpActiveRun        Operation = "ActiveRun"
	OpRuns             Operation = "Runs"
	OpTriggers         Operation = "Triggers"
	OpLatestCheckpoint Operation = "LatestCheckpoint"
	OpCheckpoint       Operation = "Checkpoint"
	OpEntries          Operation = "Entries"
	OpArtifact         Operation = "Artifact"
	OpObject           Operation = "Object"
	OpListObjects      Operation = "ListObjects"
	OpAppendEvents     Operation = "AppendEvents"
	OpEvents           Operation = "Events"
	OpStatus           Operation = "Status"
	OpInstanceStatus   Operation = "InstanceStatus"
)

// Call is what an [Authorizer] sees of one call (ADR 0026, item 4): the
// operation and every instance, run and namespace it concerns. A call is
// authorized on exactly one instance, or none:
//
//   - A call that names an instance (in its path, its fence or its body)
//     has Instance set; the handler has already checked that the path and
//     the body agree.
//   - A call that names only a run (Run, Entries, CancelRequest,
//     RequestCancel, Status) has Run set, and Instance set to the run's
//     instance, which the handler read from the run record first. A
//     refused one answers [store.ErrNotFound], exactly as a missing run.
//   - Runs with a cursor is asked twice: once for the path's instance,
//     and once with Run set to the cursor and Instance to the cursor run's
//     instance, the second refusal answering ErrNotFound.
//   - Telemetry (AppendEvents, Events) is asked once per run: on the run's
//     instance when its run record exists, and otherwise as StoreWide,
//     with no instance, since a store accepts events for a run it has no
//     record of. An empty AppendEvents batch, naming no run, is asked
//     once as StoreWide.
//   - Claimable is StoreWide: the executor's store-wide scan, to grant only
//     to an executing service's identity.
type Call struct {
	// Fence is the fence a fenced call carries (Commit, Heartbeat,
	// Release), exactly as the request carries it.
	Fence *store.Fence
	// Op is the operation.
	Op Operation
	// Instance is the instance the call is authorized on, empty for a
	// StoreWide call.
	Instance string
	// Run is the run the call names, if it names one: the run of a
	// run-only call, the requested ID of CreateRun, the cursor of Runs,
	// the run of a telemetry call.
	Run string
	// Owner is the owner a Claim asks for, so a host can bind an owner
	// identity to a credential.
	Owner string
	// Namespaces are the namespaces the call reads or writes: the one of
	// Object or ListObjects, and every namespace a commit's operations
	// touch, each once, in first-use order.
	Namespaces []string
	// StoreWide marks a call that concerns no one instance: Claimable,
	// telemetry for a run with no run record, and an empty AppendEvents
	// batch.
	StoreWide bool
}

// Authorizer decides whether the caller may make a call. The context is
// the request's, carrying whatever the host's authentication put there.
// A nil error allows the call. An error wrapping [ErrUnavailable] means
// the Authorizer could not decide, and the client sees an indeterminate
// error. Any other error refuses it: the client sees [ErrForbidden], or
// [store.ErrNotFound] where a refusal is masked as a missing record. The
// error's text is logged with the request ID, never sent.
type Authorizer interface {
	Authorize(ctx context.Context, c Call) error
}

// AuthorizerFunc adapts a function to an [Authorizer].
type AuthorizerFunc func(ctx context.Context, c Call) error

// Authorize calls f.
func (f AuthorizerFunc) Authorize(ctx context.Context, c Call) error { return f(ctx, c) }

// AllowAll is an Authorizer that allows every call, for a host whose
// authentication alone decides (a single trusted caller). It is never a
// default: a host passes AllowAll{} explicitly.
type AllowAll struct{}

// Authorize allows the call.
func (AllowAll) Authorize(context.Context, Call) error { return nil }
