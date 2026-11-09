# 0007. Agent types and the run controller

- **Status:** Accepted; amended by [ADR 0008](0008-model-states.md)
- **Date:** 2026-11-08

## Context

ADRs 0003 and 0004 fixed the run model, the execution record and recovery,
with names marked illustrative; [ADR 0005](0005-storage-one-fenced-commit.md)
and [ADR 0006](0006-store-interface.md) fixed the store. This ADR records
the public API of the runtime that drives a run, as built for code states,
and the questions building it raised:

1. how an agent type, its states and its typed state data are declared
   (STM-1 to STM-5, MAN-3);
2. how a terminal state says what the run ends with (STM-4, RUN-3);
3. when a run's initial state data is computed, and how a run's
   configuration survives handovers (RUN-1);
4. what a code state may return, and how a run ends when a code state
   fails, which no RUN-3 reason names;
5. how an activation that cannot run at all is refused.

It amends ADR 0003 items 12 (the trigger is read from the run record) and
14 (a third error, for an activation refused before it writes).

Model states (the write-ahead turn, tool permissions, attempts and
results, ADR 0004 items 6 to 10) build on this API; [ADR 0008](0008-model-states.md)
records them and amends items 2 and 9.

## Decision

### Declaring an agent type

1. **Package `agent`** holds the runtime: agent types, the registry, the
   controller and the execution record's entry formats. (`runtime` was
   rejected: it shadows the standard library's package.)
2. **An agent type is `agent.Type[D]`**, where `D` is the state data's Go
   type, serialized as JSON at every step boundary (STM-5). It declares a
   name, a version, an initial state and its states. A state sets exactly
   one of `Code` (a code state, STM-2) and `End` (a terminal state, STM-4);
   a code state declares `Next`, the states it may transition to (STM-1).
   (Amended by ADR 0008, item 1: a state sets exactly one of `Code`,
   `Model` and `End`.) **`Registry.Register`** checks the
   declaration (unique names, declared transitions naming declared states,
   a non-terminal initial state) and refuses a malformed one or a
   duplicate name with `ErrInvalidType` (MAN-3). The registry erases `D`,
   so one controller drives every agent type.
3. **A terminal state declares its ending** as one of the three an agent
   type may choose: `EndChanged` and `EndNoChange` (succeeded) and
   `EndInvalidOutput` (failed). The status follows from the reason, so an
   invalid combination cannot be declared (RUN-3). An agent type with two
   success dispositions declares two terminal states. Every other ending
   is the runtime's or the host's.

### Starting and continuing a run

4. **`Controller.Activate(ctx, ActivationInput) (Outcome, error)`** is
   ADR 0003 item 12's entry point. The input carries the claim's fence,
   the run's scope, the resolved configuration and the host's step limit
   (STM-7). **The trigger is read from the run record**, where `CreateRun`
   stored it, rather than passed again: ADR 0003 item 12's rule that the
   input never duplicates durable state applies to it too.
5. **An activation commits `activation_started` alone, first**, so its
   fence is verified and its starting point recorded before it runs any
   agent-type code (ADR 0004, item 2).
6. **A run's start commits `run_started` with a first checkpoint.** The
   agent type's `Init` computes the initial state data from the trigger
   and configuration; its result is checkpointed (state: the initial
   state, step 1) in the same commit as `run_started`, so once that
   commit lands `Init` never runs again for the run, even if it is not
   deterministic. Until it lands, `Init` may run again (after a crash or
   an indeterminate commit), so like a code state it must have no durable
   effect of its own (ADR 0003, item 5); only its committed result is
   kept. This is a
   checkpoint at a boundary that is not a step boundary, which ADR 0004
   item 4 does not forbid; it means every started run has a checkpoint of
   its own, and recovery always starts from one. **An activation that
   ends the run before that commit lands** (`Init` fails or panics, the
   configuration is not valid JSON, or the context's cause cancels or
   fails the run) commits the configuration artifact and `run_started`
   with its terminal ending, so the run's inputs are on the record
   (REC-1). A non-terminal ending (a shutdown) does not: the next
   activation starts the run, and records its start once.
7. **The configuration is fixed when the run starts** (RUN-1). It is
   stored as an artifact in the start commit, and its digest is recorded
   in `run_started` and in every checkpoint. Later activations read it
   from there and ignore the input's `Config`.
8. **The checkpoint's contents** (format 1, opaque to the host, RUN-5) are
   the agent type's name and version, the current state, the state data,
   the configuration's digest and the next step's number. A checkpoint
   written by another version of the agent type is passed to the type's
   `Migrate`; a type with no `Migrate` refuses it, and the run fails with
   `state_error` (RUN-5). The migrated data is checkpointed by the next
   step boundary, so until that commit lands `Migrate` may run again for
   the same checkpoint and, like `Init`, must have no durable effect of
   its own.
9. **The tail holds what an activation commits without a checkpoint**:
   `activation_started` and a non-terminal `outcome`. Every step boundary
   of a code state commits a checkpoint (ADR 0004, item 4), so recovery
   of a code-state run is restoring the checkpoint (ADR 0004, item 12). An
   entry of any other kind past the checkpoint is refused as one this
   version cannot recover. (Amended by ADR 0008, item 11: the tail also
   holds a model state's attempts and at most one turn with its calls.)

### Code states

10. **A code state returns a `Transition`**: the next state, its data, and
    object operations (put and delete) conditional on the versions it
    read. The controller commits the operations, the `transition` entry
    and the new state's checkpoint in one commit, and entering a terminal
    state, the `outcome` entry, the run-record change and the release in
    that same commit (ADR 0003 item 10, ADR 0004 item 4). Any other kind
    of operation fails the run. The `transition` entry records the
    operations and the new data, so the record holds the state's whole
    output: a replay (TEST-1) can apply it without running the state.
11. **A code state may run more than once for one step**: after a crash
    (ADR 0003, item 5), and when its operations conflict with another
    instance's commit. Each run decodes the step's starting data afresh,
    so a run never sees a value an earlier run mutated. **A conflict
    re-runs the state** against fresh reads up to
    `DefaultConflictRetries` (3) times (ADR 0005, item 10; ADR 0006,
    item 4), after which the run fails with `object_conflict` (ADR 0006,
    item 6). Hosts can change the bound with `WithConflictRetries`.
12. **RUN-3 gains `state_error`**, a failure reason: a code state, `Init`
    or `Migrate` returned an error or panicked, a code state chose an
    undeclared transition (STM-1) or returned operations it may not, the
    store refused its operations as invalid (a key it cannot hold), or a
    checkpoint's version has no migration. It is a defect in the agent
    type or its input, so retrying as-is cannot help. The error's text is
    kept as the outcome's `Detail`, in the `outcome` entry. A panic in
    agent-type code is recovered and treated as an error. `invalid_output`
    stays the agent type's own verdict on its output (item 3);
    `tool_error` stays with tools.
13. **A code state reads through `agent.Env`**, which exposes the run's
    identity, trigger, configuration, state and step, and object reads. A
    read that fails in the store returns a `*ReadError`; a state, `Init`
    or `Migrate` that returns an error wrapping one ends the activation
    with a store failure, not a run failure (ADR 0003, item 15), and one
    that returns an error after the context ended yields to its cause
    (item 14). A panic counts as a returned error (item 12). A malformed read (an
    invalid key) is the state's defect and is returned as is.

### Endings and errors

14. **Ending by the context's cause** (ADR 0003, item 17): a cause that
    is or wraps `ErrSuperseded` or `ErrOperator` cancels the run with that
    reason; a `*BudgetExhausted` cause fails it with `budget_exhausted`
    naming the budget; any other cause, or none, is a shutdown, and the
    run stays `runnable` with reason `shutdown`. Stopping at the host's
    step limit leaves it `runnable` with reason `step_limit`. **The cause
    takes precedence over every ending the runtime would otherwise
    choose** (RUN-2): a code state that returns a transition, terminal or
    not, after the context ended has the result discarded, which is safe
    because it has no durable effect of its own (ADR 0003, item 5); and a
    failure the runtime picks itself (`state_error`, `object_conflict`)
    yields to the cause once the context has ended. **The
    controller's own reads and writes use a context that is not cancelled
    with the activation's**, so a cancelled activation still commits its
    outcome; agent-type code receives the activation's context.
15. **Activate's errors** are ADR 0003 item 14's two, `ErrOwnershipLost`
    and `ErrStoreFailure` (each wrapping the store's error), and one
    more: **`ErrNotActivatable`**, a refusal before anything is written,
    because the run does not exist, belongs to another instance, is
    terminal or `waiting`, names an unregistered agent type, or has a
    record this version cannot recover. The host's procedure is
    unchanged: reread the run record before acting.
16. **An `Outcome`** carries the status, reason and detail, the steps the
    run has completed and its latest checkpoint sequence. Consumed
    budgets are carried as a field and stay empty until budgets are built.

## Consequences

- An agent-type author writes Go functions over their own data type and
  never touches the execution record, checkpoints or the store's write
  path; every write goes through the transition.
- RUN-3 has a reason for an agent type's defects, distinct from a model's
  or a tool's; the requirements' table records it.
- A run's first activation makes at least two commits before its first
  step (its start and the run's start); a code step makes one.
- Replaying a code state needs no record of its object reads, since its
  output is recorded whole; the replay harness (TEST-1) is decided with
  its own work.
- Not yet built, and decided with their own work (model states, the
  write-ahead turn and TEST-5 in `storetest` followed in ADR 0008): budgets and their consumption (`Outcome.Consumed`);
  manifests, tool grants and the namespaces a state may touch (MAN-1,
  MAN-2); telemetry events (HOST-1).
  Heartbeats remain the host's (HOST-4): a long activation's host extends
  the lease while it runs.

## References

- [docs/requirements.md](../requirements.md): STM-1 to STM-7, RUN-1 to
  RUN-5, MAN-3, HOST-4, TEST-1, TEST-5
- [ADR 0003](0003-run-model-identities-and-activation-results.md): the
  run model, activation results and endings (items 5, 10, 12 to 17),
  which this ADR amends (items 12 and 14)
- [ADR 0004](0004-execution-record-and-recovery.md): the record, the
  checkpoint and recovery (items 2 to 5, 12)
- [ADR 0005](0005-storage-one-fenced-commit.md): the fenced commit and the
  conflict re-run (items 3, 10)
- [ADR 0006](0006-store-interface.md): conflict versions and
  `object_conflict` (items 4, 6)
