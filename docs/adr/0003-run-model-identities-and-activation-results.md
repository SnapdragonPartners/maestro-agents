# 0003. Run model: identities, stored status and activation results

- **Status:** Amended by ADR-0007, ADR-0012, ADR-0016
- **Date:** 2026-11-07

## Context

[The requirements](../requirements.md) define an agent *instance*, a
*run* and an *activation* (section 1), the outcome an activation reports
(RUN-3), and the identities that must survive every handover between
activations (RUN-1, TOOL-5). They left three questions to the design:

1. What a run's stored status is while a worker is executing it, given
   that RUN-3's statuses are all ones an activation *ends* with.
2. How an orchestrator tells work that is ready to be picked up from work
   that is being executed.
3. How an activation reports that it could not record anything, for
   example because the execution-record store failed (REC-2), when every
   RUN-3 status is itself a durable write that would fail the same way.

Identities also need to accept a host chat turn later (LOOP-4), which has
no run, state machine or checkpoint, without changing the contracts that
key on them.

## Decision

### Identities

1. **Scope.** Everything that RUN-1, TOOL-5, TOOL-8, HOST-1 and REC-1 key
   on "the run" keys on a *scope*: a kind and an ID. The MVP has one kind,
   `run`, whose ID is the run ID. LOOP-4 adds `chat_turn`, whose ID the host
   supplies (conversation and turn); no contract changes when it does.
2. **Instance and run IDs** are host-assigned opaque strings. A run record
   is created conditionally on its trigger's identity (STORE-1), so a
   retried creation finds the existing run. An instance has at most one
   non-terminal run. A trigger that arrives meanwhile is either queued
   for that instance or started on a new instance, as the host chooses
   (MODE-4, STM-9); the standalone host queues by default with a durable
   store (MODE-4).
3. **Step.** A step is the work between two step boundaries (STM-6): one
   model turn with its tool calls, or one state's processing that ends in
   a transition. Steps are numbered from 1 within the run, and the number
   is part of the run's checkpointed state. A wait for input (STM-8) does
   not start a new step.
4. **Action identity** is (scope, step, call index), where the call index
   is the call's position in the recorded model response (STM-6). Every
   call has an action identity; a write call also has an idempotency key
   (TOOL-5), and reads carry none (STM-6). The key is the hex SHA-256 of a canonical,
   domain-separated encoding of the action identity. The attempt marker
   (ADR 0004) also records a digest of the call's arguments; reissuing an
   action identity whose recorded digest differs is refused as a defect.
5. **Code states have no durable effect of their own.** A code state's
   only durable output is its transition, together with any object-store
   operations committed atomically with it (ADR 0005). Re-running a code
   state after a crash is therefore safe: its earlier run left nothing
   behind. External effects go through tools, which carry action
   identities.

### Stored status and ownership

6. **There is no stored `running` status.** A run's stored status is one
   of RUN-3's: `runnable`, `waiting`, `succeeded`, `failed` or `cancelled`,
   each with its reason. A new run is created `runnable`, and a run being
   executed stays `runnable` until its activation commits something else.
7. **Whether a run is executing comes from the instance's ownership
   record** (STORE-1): owner, epoch and lease expiry. The two facts a host
   needs are derived from the run record and the ownership record
   together:
   - *executing*: status `runnable` and the instance has a live owner (an
     owner whose lease has not expired);
   - *ready*: status `runnable` and the instance has no live owner (no
     owner, or an expired lease). A `waiting` run is not ready: nothing
     can advance it until its input arrives. When STM-8 is built
     (post-MVP), the host records a delivered input in the store, with a
     control-plane write added to ADR 0005 item 4, and *ready* is
     extended to a `waiting` run with no live owner and a recorded input
     it has not yet accepted; the activation then accepts it (STM-8). Until
     then nothing enters `waiting` (MVP.md).
8. **Claiming is compare-and-set on the ownership record.** A claim
   succeeds only if the instance has no live owner, and it increments the
   epoch, which never decreases. Two hosts racing for one run cannot both
   win. Heartbeats extend the lease and are fenced on the epoch. Lease
   expiry uses the store's clock.
9. **Fencing is by epoch, not by lease.** Every durable write is checked
   against the owner epoch (HOST-6, ADR 0005). An owner whose lease has
   expired but whose epoch is still current may still commit: nobody else
   owns the instance. Once another host claims, the epoch has moved and the
   old owner's writes are rejected.
10. **An activation releases ownership in its final commit.** The commit
    that records the activation's ending status (`runnable` at a step
    limit or shutdown, `waiting`, or terminal) also clears the owner and
    lease, so a run stopped at the host's step limit is ready again at
    once. A crashed owner releases nothing; its lease expires and the run
    becomes ready.
11. **The store answers both questions.** The store interface offers a
    query for claimable instances and the atomic claim (ADR 0005), so no
    host reimplements the readiness condition.

A stored `running` status was rejected: a crashed worker leaves it behind,
so every reader would still have to check the lease, and the store would
hold two sources of the same fact that can disagree.

### Activations and their results

12. **The host claims; the controller runs.** The host (its control plane,
    HOST-4) claims the instance and passes the claim (instance, owner,
    epoch) to the run controller, whose entry point has this shape (names
    illustrative):

    ```go
    func (c *Controller) Activate(ctx context.Context, in ActivationInput) (Outcome, error)
    ```

    `ActivationInput` carries the claim, the run's scope, the trigger, the
    resolved configuration and, for a `waiting` run, the delivered input
    (RUN-1). The controller reads the run record, the latest checkpoint
    and the execution record's tail (ADR 0004) through the store, rather
    than taking them by value, so the input never duplicates or races
    durable state.
13. **An `Outcome` is returned only when this activation's commit of it is
    confirmed.** The outcome is RUN-3's status and reason, with the step
    reached, the checkpoint sequence, consumed budgets and artifact
    references. When `Activate` returns a nil error, the outcome's status
    is what the activation durably committed, in the same commit that
    released ownership.
14. **A non-nil error means the activation has no confirmed outcome**, and
    the `Outcome` is zero. The error is one of two kinds:
    - *ownership lost*: a commit was rejected because the epoch moved
      (HOST-6). The run's status is whatever its new owner commits
      (RUN-3, RUN-4).
    - *store failure*: a durable write failed, or its result is
      indeterminate (ADR 0005, item 3). The error wraps the cause.

    An error does not say the activation committed nothing: an
    indeterminate commit may have landed, including the activation's final
    commit with a terminal status. So **before acting on an error, the host
    rereads the run record**. If the run is terminal, it is finished and
    the reread status is its outcome. If it is `runnable` or `waiting`, the
    run stands at its last landed commit, whose checkpoint and record are
    intact, and the host retries it later: delivery is at least once
    (DEL-1), and recovery (ADR 0004) continues from what landed. A store
    that cannot be read yet is reread later; nothing is assumed meanwhile.
    After a store failure the claim may still be held; the host releases
    it through the store when it can, or lets the lease lapse.
15. **A store failure is not a run failure.** The run is not marked
    `failed`: the fault is in infrastructure, not in the run, and the
    store that would record `failed` is the one that is failing. A host
    that wants to give up on a run after repeated store failures records
    its own outcome (RUN-4) by claiming the instance and committing a
    status change through the same fenced commit.
16. **On a store failure the activation stops at once.** It admits no new
    tool call or model attempt (no write-ahead entry can be recorded, so
    none may proceed, REC-2), lets admitted tool calls finish or reach
    their deadline (HOST-7), and returns the error. A tool call whose
    attempt was recorded but whose result was not is resolved by recovery
    (STM-6, ADR 0004).
17. **Cancellation, budget expiry and shutdown are outcomes, not
    errors**, told apart by the context's cause, never by the bare fact of
    cancellation. A host's cancellation (superseded or operator) commits
    `cancelled` with that cause (RUN-2). The runtime's own time budgets
    also end the context (BUD-2), with a cause naming the budget, and
    commit `failed` with `budget_exhausted` naming it (RUN-3). A shutdown
    commits the run's actual disposition (`runnable` or `waiting`,
    MODE-6). Only a failure to commit those turns them into an error.

## Consequences

- The run record has five statuses, all of which RUN-3 already defines.
  "Executing" and "ready" are derived views, computed by the store from
  two records the store already has.
- Readiness after a crash depends on lease expiry: a run whose worker
  crashed is picked up one lease period later. Hosts choose the lease
  length; the standalone host avoids the wait at startup (ADR 0005).
- Hosts handle two return shapes: an outcome (the run's confirmed status)
  or an error (no confirmed outcome: reread the run record, then retry if
  it is not terminal). A host must not record a run as failed because
  `Activate` returned an error.
- Code states are restricted to transitions and atomically committed
  object-store operations. An agent type that wants an external effect
  from a code state expresses it as a tool call instead.
- Chat turns (LOOP-4) fit by adding a scope kind, with no change to action
  identities, idempotency keys, events or the execution record.

## References

- [docs/requirements.md](../requirements.md): section 1; STM-6, STM-8,
  RUN-1 to RUN-4, TOOL-5, TOOL-8, HOST-4, HOST-6, HOST-7, REC-2, STORE-1,
  MODE-4, MODE-6, LOOP-4, DEL-1
- [ADR 0002](0002-serial-execution-within-an-agent-instance.md): one
  execution owner per instance
- [ADR 0004](0004-execution-record-and-recovery.md): the execution record,
  write-ahead order and recovery
- [ADR 0005](0005-storage-one-fenced-commit.md): the fenced commit, the
  claim and the reference stores
- Maestro [ADR 0032](https://github.com/SnapdragonPartners/maestro/blob/main/docs/adr/0032-agent-execution-contract.md):
  fenced execution authority (binding item 9)
