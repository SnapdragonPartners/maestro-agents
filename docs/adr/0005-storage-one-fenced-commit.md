# 0005. Storage: one fenced commit, and the reference stores

- **Status:** Accepted
- **Date:** 2026-11-07

## Context

The requirements fix the records a host stores (STORE-1), the object
store (STORE-2, STORE-3) and three reference implementations: memory,
files and Postgres (STORE-4). They require ownership to be checked with
every durable write an activation makes, serialized against ownership
transfer (HOST-6), and tested at two pause points against every durable
store (TEST-6). Tools whose write contract is an atomic local transaction
(TOOL-12) need their effect and its record to commit together, which in
the file store means an atomic commit across several records: a question
the requirements left to this ADR. So is encryption at rest for the
intake agent's data (its N-5, MVP.md).

[ADR 0003](0003-run-model-identities-and-activation-results.md) settles
the claim and the derived *ready* and *executing* views;
[ADR 0004](0004-execution-record-and-recovery.md) settles what is written
at each point of a step. This ADR settles how those writes reach a store.

## Decision

### One commit for every durable write

1. **Every durable write an activation makes goes through one operation**
   (names illustrative):

   ```go
   Commit(ctx context.Context, fence Fence, ops []Op) error
   ```

   `Fence` is (instance, owner epoch) from the activation's claim. A
   commit applies all of its operations or none. There is no other write
   path for an activation to the library's stores, so HOST-6's rule that
   no write skips the check holds by construction for every write to
   them. A tool's effect outside those stores (an external write) is not
   covered by `Commit`: HOST-6 still requires the destination or the host
   to fence it (accepting a fencing token, say), or the write tool to
   declare that it cannot be fenced, with the outcome reporting the
   uncertainty. The MVP has no external writes (MVP.md); a tool
   contract that carries the fence to an external destination is decided
   with the first such tool.
2. **The ownership check and the writes are serialized against transfer.**
   A store reads the instance's ownership record, compares its epoch with
   the fence and applies the operations so that no claim can commit in
   between. A mismatch rejects the whole commit with an ownership error
   (ADR 0003's *ownership lost*).
3. **Operations are JSON values** (P-6), each with its own precondition:
   - put or delete an object in a namespace, conditional on its version
     (STORE-2);
   - append an execution-record entry at the sequence it expects
     (ADR 0004);
   - publish a checkpoint at the instance's next checkpoint sequence
     (HOST-6's compare-and-set on instance, owner and sequence);
   - change the run record (status, reason, consumed budgets, accepted
     inputs);
   - store an artifact, content-addressed and write-once (storing the
     same digest twice is a no-op);
   - release ownership (ADR 0003, item 10).

   A failed precondition rejects the whole commit with a conflict error
   naming the operation. New kinds of record add operations; they do not
   add write paths.

   **A commit's result is one of three, and only the first two are
   definite.** A nil error means the commit landed. An ownership or
   conflict error means it did not, and nothing was applied. Any other
   error is *indeterminate*: the commit may have landed (a database
   commit whose acknowledgement was lost, a file-store commit that passed
   its commit point and then failed, item 14). Every caller treats an
   indeterminate commit as possibly landed: it stops, and whoever acts
   next learns what happened by reading durable state, never by assuming
   either answer. The write-ahead order is safe under both answers: an
   expected-sequence append cannot land twice, and recovery decides each
   recorded call from what the record actually holds (ADR 0004, item 10).
4. **Writes outside an activation are the control plane's own**, each a
   single compare-and-set with no fence: claiming and heartbeats
   (ADR 0003), enqueueing a trigger (idempotent on its identity), creating
   a run from a queued trigger (conditional on the trigger's identity,
   STORE-1) and removing the trigger once its run exists. A host that
   overrides a run's outcome (RUN-4) claims the instance first and uses
   `Commit`.
5. **Reads are unfenced** and may be stale; correctness comes from the
   preconditions at commit. The store also answers the claimable-instances
   query (ADR 0003).
6. **The test-only pause hook sits inside `Commit`** at TEST-6's two
   points: before the commit starts, and after the ownership record is
   read but before any write. Each durable store adapter accepts it as a
   construction option documented as test-only. A conformance suite (a
   `storetest` package) runs TEST-5, TEST-6 and the operations' semantics
   against any store, so a host store that claims HOST-6 runs the same
   tests as the reference stores.

### Atomic local transactions

7. **A tool whose write contract is an atomic local transaction does not
   write.** Its `Execute` receives a read-only view of the namespaces its
   grant names, with each object's version, and returns its result
   together with object-store operations conditional on the versions it
   read. The runtime commits those operations in the same commit as the
   call's `result` entry (ADR 0004, item 8), and, if the call causes a
   transition, with the transition and its checkpoint.
8. **Hence the outcome is never unknown.** An `attempt` with no `result`
   proves the operations did not commit, so recovery issues the call again
   with the same action identity (ADR 0004, item 10).
9. **Such a tool has no other effect.** It receives the read view and
   nothing else: no credential provider (TOOL-8) and no writable handle.
   The declaration is the tool author's promise, enforced by what the tool
   is given.
10. **A conflict re-runs the tool.** If a precondition fails because
    another instance sharing the namespace committed first, the runtime
    runs the tool's `Execute` again against fresh reads, up to three times
    by default. If it still conflicts, the call's result is a tool error
    returned to the model as data (nothing was written), and that result
    is committed like any other.

### The memory store

11. **The memory store** holds every record in maps behind one mutex, so
    commits serialize trivially. It meets every semantic above for a
    process's lifetime and is not evidence of fencing (TEST-6).

### The file store

12. **One process per store directory.** Opening the store takes an
    exclusive, non-blocking `flock` on a lock file in the directory; a
    second process fails at once with an error naming the directory. The
    lock is released when the process exits, however it exits. Network
    file systems are not supported.
13. **One JSON file per record**, with keys and IDs encoded so each is a
    single safe path component (names illustrative):
    - `instances/<instance>/ownership.json`: owner and epoch (item 16);
    - `instances/<instance>/checkpoints/<seq>.json`: one file per
      checkpoint, retained (ADR 0004, item 3);
    - `runs/<run>/run.json`: the run record;
    - `runs/<run>/record/<seq>.json`: one file per execution-record entry.
      An append at sequence *n* has the precondition that entry *n* does
      not exist and entry *n*-1 does (or *n* is 1), checked under the
      mutex like every precondition; publishing checkpoint *n* is checked
      the same way;
    - `objects/<namespace>/<key>.json`: value and version;
    - `artifacts/<sha256>`: content, written once;
    - `triggers/<instance>/<seq>.json`: queued triggers in arrival order;
    - `telemetry/<run>/<seq>.json`: telemetry events, written by the
      telemetry sink (HOST-1) idempotently on their identity, not through
      `Commit`: telemetry is an observation, never state, so recovery and
      fencing never depend on it (HOST-1, REC-2);
    - `journal/`: commits in progress (item 14).

    Prefix listing reads a namespace's directory and filters the decoded
    keys, which is linear in the namespace's size and adequate at the file store's scale. A file
    is written by writing a temporary file in the same directory, syncing
    it, renaming it over the target and syncing the directory. (Go's
    `File.Sync` issues `F_FULLFSYNC` on macOS.)
14. **A multi-record commit goes through a journal.** The store has one
    in-process mutex, which serializes commits and claims. A commit:
    1. reaches the first pause point (before taking the mutex);
    2. takes the mutex, reads the ownership record and reaches the second
       pause point;
    3. checks the fence and every precondition against the current files;
    4. writes one journal file holding every operation's final contents,
       by the atomic write above (**the commit point**);
    5. applies each operation by the atomic write above;
    6. deletes the journal and syncs the journal's directory, so its
       removal is durable;
    7. releases the mutex.

    A commit of one file skips the journal; its rename is the commit point.

    **No later write may land while a journal is live.** A journal holds
    final contents, so replaying it is harmless only if nothing has
    written its files since. Two rules guarantee that. First, the mutex is
    released only after step 6, so no other commit or claim runs while a
    journal exists. Second, any failure from the journal's rename onwards
    (its directory sync, step 5 or step 6) *poisons* the store: the commit
    returns an indeterminate error (item 3), and the store refuses every
    later operation, reads included, with an error saying it must be
    reopened. Reopening (item 15) finishes the journal before serving
    anything. A failure before the journal's rename is definite: nothing
    was applied, and the store stays usable. A one-file commit follows the
    same rule: a failure after its rename (the directory sync) poisons the
    store, since the commit may or may not survive a power loss.
15. **Opening the store finishes interrupted commits.** After taking the
    lock, and before serving any operation, the store applies every journal
    it finds, in order, deletes each and syncs the directory. A journal is
    complete or absent, since it is written by rename. If this fails, the
    open fails.
16. **Leases live in memory; owners and epochs on disk.** Only the one
    process reads its leases, so the store keeps lease expiry in memory and
    writes no file per heartbeat. The ownership file holds the owner and
    the epoch, so the epoch never decreases across restarts. On open, the
    lock proves that the previous holder is gone, so every recorded owner
    is treated as having no lease: the instances are claimable at once,
    and the next claim increments the epoch.

### The Postgres store (deferred)

17. **The Postgres store's `Commit` is one transaction** that locks the
    instance's ownership row (`SELECT ... FOR UPDATE`) before comparing
    the epoch and writing; the second pause point is after that select. It
    follows STORE-5 and is built after the MVP.

### Encryption at rest

18. **The library and its reference stores do not encrypt.** Encryption at
    rest is the host's: a host that wants it encrypts values before they
    reach the store (for instance by wrapping the store), or relies on
    disk encryption. The standalone intake agent relies on the operating
    system's full-disk encryption, which its documentation makes an
    operator requirement (intake N-5). REC-3 already gives protection of
    the execution record to the host.

## Consequences

- HOST-6 is enforced at one choke point. A new record kind is a new
  operation, which `Commit` fences automatically; there is no second write
  path to forget.
- Atomic local transactions are declarative: a tool returns operations and
  the runtime commits them. The tool cannot hold a transaction open, and
  its outcome is never unknown.
- The file store is crash-consistent without a database: every file is
  replaced atomically, and every multi-record commit either reached its
  journal or never happened. Its cost is one process per directory and a
  linear prefix listing.
- After a restart, the standalone host can claim every unfinished run at
  once, without waiting out leases from the previous process.
- A host store implements `Commit` and the control-plane writes, and
  proves them with the conformance suite.
- Data at rest is protected by the host or the operating system, never by
  the library.

## References

- [docs/requirements.md](../requirements.md): STORE-1 to STORE-5, HOST-6,
  TEST-5, TEST-6, TOOL-8, TOOL-12, RUN-4, REC-3, P-6
- [MVP.md](../MVP.md): the MVP's setting; the open N-5 question this ADR
  closes
- [ADR 0003](0003-run-model-identities-and-activation-results.md): claims,
  leases and activation errors
- [ADR 0004](0004-execution-record-and-recovery.md): what each step writes
- buggrit `internal/github/fence.go`: the fence checked before the effect,
  which this design moves into the commit
