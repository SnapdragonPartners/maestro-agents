// Package store defines the durable records an agent host keeps and the
// interface through which the runtime and the host read and write them.
//
// The design is ADR 0005 (docs/adr/0005-storage-one-fenced-commit.md), as
// amended by ADR 0006 (docs/adr/0006-store-interface.md):
//
//   - Every durable write an activation makes goes through [Committer.Commit],
//     fenced on the instance's owner epoch and applied atomically: all of a
//     commit's operations land or none do.
//   - Writes outside an activation (claims, heartbeats, triggers, run
//     creation, cancel requests) are the control plane's own, each a
//     single compare-and-set with no fence ([ControlPlane]). A cancel
//     request is a record of its own, never part of the run record (ADR
//     0024, item 6).
//   - Reads are unfenced ([Reader]). Reads of shared objects may be stale;
//     an owner's reads of its own instance's records, and of artifacts, are
//     fresh.
//
// A commit's error is one of three kinds. A nil error means the commit
// landed. [ErrOwnership], a [*ConflictError] or [ErrInvalid] means it did
// not, and nothing was applied ([IsDefinite] reports these, with the
// control plane's own definite errors). Any other
// error is indeterminate: the commit may have landed, and the caller stops
// and learns what happened by reading durable state.
//
// The reference implementations are [github.com/SnapdragonPartners/maestro-agents/store/memstore]
// and [github.com/SnapdragonPartners/maestro-agents/store/filestore]. A host
// store proves its conformance with
// [github.com/SnapdragonPartners/maestro-agents/store/storetest].
package store
