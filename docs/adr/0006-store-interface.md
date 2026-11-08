# 0006. The store interface

- **Status:** Accepted
- **Date:** 2026-11-08

## Context

[ADR 0005](0005-storage-one-fenced-commit.md) fixed how durable writes
reach a store, with names marked illustrative. This ADR records the
interface as built, and four questions the review of ADR 0005 left to the
PR that defines it:

1. whether artifact reads are fresh for an owner, like its own records;
2. how an object-conflict re-run (ADR 0005, item 10) is sure to see the
   write it lost to;
3. the precondition of the run-record change, which ADR 0005 item 3 lists
   without one;
4. how a run ends when a code state's object operations keep conflicting:
   ADR 0005 item 3 says `tool_error`, which misnames it.

## Decision

### Packages

1. **`store`** holds the records, the operations, the errors and the
   interface, and imports only the standard library (P-7). The interface
   is three parts, so a consumer can ask for the least it needs:
   `Committer` (the one fenced write path), `ControlPlane` (claims,
   heartbeats, release, triggers, run creation) and `Reader`.
   **`store/memstore`** and **`store/filestore`** are the reference
   stores; **`store/storetest`** is the conformance suite. Both reference
   stores run on one engine, `internal/storecore`, over a backend that
   only reads, lists and atomically applies files, so the two cannot
   differ in meaning; the file store adds the lock, the journal and
   recovery.
2. **A fence is (instance, owner, epoch).** A commit lands only if the
   ownership record names that owner at that epoch, as HOST-6's
   compare-and-set on instance and owner reads.

### The four deferred questions

3. **Artifact reads are fresh.** ADR 0005 item 5's fresh-read set gains
   artifacts: an owner's read after its claim sees every artifact
   committed before the claim and since. An entry and the artifact it
   references commit together (ADR 0004, item 1), so a fresh entry read
   with a stale artifact read could otherwise find a digest missing.
4. **A conflict carries the object's current version.** An object
   conflict reports the version the object stood at when the commit was
   checked (for an absent key, the version a creation must expect), and
   any read the same store serves after the conflict observes at least that
   version. A re-run therefore never reads a state older than the one it
   lost to. The reported version is always a committed one: when the
   conflict is with an earlier operation of the same commit, the rejected
   commit's own writes are not reported, since none of them land. **Object versions never repeat:** a deletion is itself a
   version, kept as a tombstone, so a writer holding a version from before
   a deletion and re-creation cannot succeed.
5. **A run-record change is conditional on the run record's version**,
   which every change increments. Only the owner writes its runs, so the
   precondition cannot fail legitimately; a failure is a store failure
   (ADR 0005, item 3). A change from a terminal status to a non-terminal
   one is refused as invalid: terminal is final until extending a
   budget-exhausted run is built (RUN-3, deferred).
6. **A code state whose object operations still conflict after the bound
   fails with `object_conflict`**, a new RUN-3 failure reason, in place of
   ADR 0005 item 3's `tool_error`: no tool was involved. Retrying can help,
   since the competing commits have landed and a new activation reads
   them. A tool's persistent conflict is unchanged: the model receives it
   as a tool error, as data (ADR 0005, item 10).

### Errors and the control plane

7. **Definite errors** are `ErrOwnership`, `ErrInvalid` (a malformed
   operation, an empty or oversized identifier, a write to another
   instance's records, a forbidden transition), `ErrNotClaimable`,
   `ErrInstanceBusy` and `*ConflictError`; `IsDefinite` reports them. Every
   other error from a write is indeterminate (ADR 0005, item 3), including
   a context ended before the write started: callers treat it like any
   other indeterminate result.
8. **The control plane adds `Release`**, fenced on owner and epoch, for a
   host giving up a claim after a store failure (ADR 0003, item 14); an
   activation releases in its final commit. **A run's trigger identity is
   scoped to its instance.** `CreateRun` refuses a new run on an instance
   with a non-terminal one (ADR 0003, item 2) and a run ID already in use.
9. **Every stored string is valid UTF-8**, and a write holding one that is
   not is refused as invalid: `encoding/json` replaces invalid bytes with
   U+FFFD, so such a string would not read back as written, and an
   identity built from it would name another record. The checks sit at the
   choke points: `Op.Validate` for every commit operation, and the claim,
   the trigger queue and run creation for the control plane. **A raw JSON
   value is held to the same rule:** `encoding/json` stores invalid UTF-8
   inside a raw value byte for byte, but such a value is not JSON
   (RFC 8259, section 8.1), a reader decoding it gets U+FFFD, and a store
   on a JSON column type refuses it. Every raw value (object values,
   payloads, checkpoint state, consumed budgets) must be valid JSON in
   valid UTF-8; `store.ValidJSON` is the check, for host stores too. **An empty byte value is a
   value:** an object's `bytes` field is `null` when unset and `""` when
   empty, so the two survive every JSON round trip.
10. **Telemetry storage is not in this interface yet.** Telemetry is
   written by the telemetry sink, not through `Commit` (ADR 0005, item
   13), and joins with that sink.

### Tests

11. **Both reference stores accept the pause hook**; the suite runs TEST-6
    against both, though only the file store's pass is evidence of
    fencing (ADR 0005, item 11). The suite's harness requires an
    injectable clock, which it advances to expire leases. The suite tests
    itself against stores broken on purpose: one that commits under
    whoever owns the instance at write time fails the before-commit test,
    and one that checks the fence and then writes without holding anything
    against a claim fails the inside-commit test and passes the
    before-commit one, which is why both exist. The inside-commit test
    treats a claim that returns within 300 ms while A is paused as having
    committed first; a store that serializes the claim behind the commit
    blocks it indefinitely, so the window cannot fail a correct store.
    **The suite does not run TEST-5 yet**, which ADR 0005 item 6 assigns
    to it: it runs the operations' semantics, TEST-6's two interleavings
    and a reopen check. TEST-5 stops an activation at a durable boundary
    and continues it in a new one, so it needs the execution record's
    runtime, and joins the suite with that runtime.

### The file store

12. **Names.** A key or ID is one path component: lowercase letters,
    digits, `-` and `_` stand for themselves and every other byte is
    `%xx` in lowercase hex, so distinct keys stay distinct on
    case-insensitive file systems and no name starts with `.`, which
    temporary files (`.tmp-`) use. An encoded component is limited to 240
    bytes; a longer one is invalid. The lock file is `LOCK`; a run's
    trigger index is `instances/<instance>/runs/<trigger>.json`.
13. **Confinement and permissions.** Every file access goes through
    `os.Root` on the store directory. Directories are created `0700` and
    files `0600`. Opening creates any missing ancestors of the store
    directory and syncs each new directory, and the store directory itself,
    with its parent before accepting a write. On a platform without
    `flock` the store refuses to open rather than run unlocked.

## Consequences

- Hosts implement one interface of three parts and prove it with
  `storetest`; the reference stores share one engine, so a semantic fixed
  once is fixed in both.
- RUN-3 gains `object_conflict`; the requirements' table records it.
- Tombstones keep a deleted key's version, so a namespace's directory
  grows with every key ever written; at the file store's scale this is
  acceptable, and retention tooling comes after the MVP.
- Telemetry storage, and with it the telemetry record of STORE-1, arrives
  with the telemetry sink, as an addition to this interface.

## References

- [ADR 0003](0003-run-model-identities-and-activation-results.md): claims,
  release, activation errors
- [ADR 0004](0004-execution-record-and-recovery.md): entries and the
  artifacts they reference
- [ADR 0005](0005-storage-one-fenced-commit.md): the fenced commit and the
  reference stores, which this ADR amends (items 3, 5, 6 and 10)
- [docs/requirements.md](../requirements.md): RUN-3, HOST-6, STORE-1 to
  STORE-4, TEST-6, P-7
