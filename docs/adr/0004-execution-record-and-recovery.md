# 0004. Execution record and recovery

- **Status:** Accepted
- **Date:** 2026-11-07

## Context

The requirements ask for a protected, write-ahead execution record per
run (REC-1, REC-2) from which a run can be recovered, explained and
replayed without calling the model or repeating an external read or write
(RUN-6, REC-4, TEST-1). They also ask for checkpoints at every step
boundary (STM-6), opaque to the host and published by compare-and-set
(RUN-5, HOST-6), and fix what happens to each recorded tool call when an
activation continues mid-turn (STM-6's partition).

The model loop is `maestro-llms/toolloop` (LOOP-1). `toolloop.Run` runs a
whole loop: it sends the transcript, executes the response's tool calls in
order through each `Tool.Execute`, and repeats. It has no hook between
receiving a response and executing its calls, but it does accept any
well-formed transcript, including one that ends in tool results. The
requirements (section 7) record that the write-ahead turn needs no
upstream change, because a decorator around the injected client can
record each response before the loop sees it.

This ADR fixes how the record and checkpoints relate, the order of
durable writes within a step, and the recovery procedure. Where the
records are stored and how a commit is made atomic and fenced is
[ADR 0005](0005-storage-one-fenced-commit.md).

## Decision

### The record and the checkpoint

1. **The execution record is an append-only log per run.** Entries are
   numbered from 1 with no gaps, and each append names the sequence number
   it expects, so a lost or duplicated append is detected at commit. Each
   entry has a kind, the step it belongs to and a JSON payload (P-6).
   Large or repeated content (model requests and responses, documents,
   prompts, skills, tool definitions, the manifest) is stored as an
   artifact and referenced by its SHA-256 digest, in the same commit as
   the entry, so a referenced digest is always retrievable (REC-1).
2. **Entry kinds** (names illustrative):
   - `run_started`: the trigger, the resolved configuration, and the
     digests of the manifest, prompts, skills and tool definitions in
     force (REC-1, MAN-4);
   - `model_attempt`: one provider attempt, recorded before it is sent:
     the request (the messages added to the transcript since the previous
     request, a delta as buggrit stores requests, and the request
     parameters) and the attempt's budget reservation (BUD-3);
   - `turn`: the attempt's response, with the served model identity
     (LLM-3) and the observed usage;
   - `model_failure`: the attempt's observed error (its `maestro-llms`
     error kind, status and message, and any usage reported), so a run
     that ends with `model_error` replays too (REC-1, REC-4);
   - `permission`: a call's permission and the policy decision behind it
     (STM-6, TOOL-3);
   - `attempt`: a call's action identity, idempotency key and arguments
     digest, recorded immediately before it is sent (STM-6, ADR 0003);
   - `result`: a call's outcome: executed, with its result; not executed
     because a transition ended the turn (STM-3); refused because it names
     no tool offered to the state (MAN-2), with the result the model
     received; *unknown*; or known not to have happened (TOOL-12);
   - `transition`: the state left and entered, with the new state data;
   - `outcome`: the status and reason an activation ended with (RUN-3).

   STM-8's pending actions, accepted inputs and invalidations and LOOP-5's
   compactions get entry kinds when that work is built; the log needs no
   other change to take them.
3. **A checkpoint is a snapshot of the record.** It holds the agent type's
   state, its state data, the transcript of a model state in progress, the
   step number, consumed budgets, and the record sequence number it
   covers. The envelope (checkpoint sequence, covered record sequence,
   format version) is the library's; the contents are opaque to the host
   (RUN-5), and an agent type can refuse or migrate an old version.
4. **Every step boundary commits a checkpoint**, in the same commit as the
   entry that completes the step: the last `result` of a model turn, or
   the `transition`, or the `permission` of a call that may not be sent. A
   model turn is complete when every call in it is resolved: it has a
   `result`, or a permission other than *allowed* or *approved*, whose
   denial the model receives as data (a turn with no calls is complete
   once recorded). A step is therefore at most one turn's worth of
   entries past the latest checkpoint. STM-7's choice of how many steps an
   activation covers is how often the host *stops*, not how often state is
   saved.
5. **Recovery reads the latest checkpoint and the entries after the
   sequence it covers** (the tail). The tail is never longer than one step.

### Write-ahead order within a step

6. **Each provider attempt is recorded before it is sent.** A decorator
   around the injected client commits a `model_attempt` entry, with the
   request and the attempt's reservation, before sending it (BUD-3).
   After the attempt it commits exactly one of `turn` (a response) or
   `model_failure` (an error), each with the observed usage replacing the
   reservation; a failure whose consumption is unknown is charged its full
   reservation (BUD-3, BUD-4). A `model_attempt` with neither after it is
   an attempt interrupted by a crash or a store failure: its consumption is
   unknown and it is charged its full reservation, so a crash can never
   return budget to the run, and its request and the absence of an
   observation are both on the record. Until `maestro-llms#149` provides a hook at
   each attempt, the MVP's client makes one attempt per call (MVP.md,
   BUD-1), so the decorator sees every attempt.
7. **The model turn is recorded before any of its calls executes.** The
   same decorator commits the `turn` entry when the response arrives and
   only then returns it to `toolloop` (STM-6, REC-2). If the commit
   fails, the decorator returns an error and the loop executes nothing.
8. **Each tool call is wrapped.** Every `toolloop.Tool` the runtime gives
   the loop is a wrapper whose `Execute`, in this order:
   1. obtains the call's permission from the policy hook and commits a
      `permission` entry;
   2. if the permission is anything but *allowed* or *approved*, returns
      the denial to the model as data, with no attempt (TOOL-3);
   3. commits an `attempt` entry;
   4. runs the tool through its middleware (TOOL-7);
   5. commits a `result` entry.

   For a tool whose write contract is an atomic local transaction, step 4
   produces object-store operations instead of performing them, and step
   5 commits them together with the `result` (ADR 0005). A failed commit
   at any step stops the call there and returns an error from `Execute`,
   which stops the loop; the activation then returns a store failure
   (ADR 0003).

   **Results the loop synthesizes itself never reach a wrapper**, and are
   recorded by the decorator. `toolloop` makes two kinds: the not-executed
   results of siblings after a `Stop` (item 9), and the error result for a
   call naming no registered tool (`dispatch`). For the second, the next
   durable write after the turn's calls is the decorator's: before the
   next provider attempt, or when the loop returns, it takes the
   synthesized results from the transcript the loop now holds and commits
   their `result` entries, and the turn's checkpoint if they complete it,
   in the same commit as the next `model_attempt` (or on the loop's
   return, before anything else is committed). So a turn that ends with
   such a call reaches its step boundary before any later work, and the
   recorded content is exactly what the model was sent.
9. **A transition commits atomically.** The `result` of a call that causes
   a transition (STM-3), the not-executed `result` of every later sibling,
   the `transition` entry and the new state's checkpoint go in one commit.
   No recovery ever sees a transition without its siblings marked, or the
   reverse. The tool returns `ToolResult.Stop`, so `toolloop` ends the turn
   at that call and synthesizes matching not-executed results in the
   transcript (`maestro-llms` ADR 0017); the loop then returns, and the
   ended state's transcript, which is the only place that text appears, is
   recorded from the loop's returned messages before the next state runs.

### Recovery

10. **A continuing activation rebuilds the step from the tail.** Starting
    from the checkpoint, it applies the tail's entries. If the tail holds
    a `turn` whose calls are not all resolved, the activation does not ask
    the model again. It resolves each recorded call in order by STM-6's
    partition, from that call's `permission`, `attempt` and `result`
    entries:
    - not attempted, not marked not executed, allowed or approved:
      issued through the same wrapper (a call with no permission gets one
      first);
    - a read attempted with no result: issued again;
    - a write attempted with no result: *unknown*, except that a write
      whose contract is an atomic local transaction is *known not to have
      happened*, because its effect commits only with its result
      (ADR 0005), and is issued again;
    - executed: its recorded result stands;
    - not executed, or with any permission but allowed or approved: not
      issued;
    - naming no tool offered to the state, with no result: refused, with
      the runtime's own error result, recorded before the turn continues
      (MAN-2).

    A write issued again keeps its action identity and idempotency key
    (TOOL-5). When every call is resolved, the step's checkpoint is
    committed and, if the run is still in a model state, the transcript
    (ending in the turn's tool results) is passed to `toolloop.Run`, which
    continues from it.
11. **An unknown outcome fails the run in the MVP.** The *unknown* outcome
    exists in the types and is recorded if it arises; the run then fails
    with `tool_error`. Resolving it by reconciliation probe or by a person
    is deferred (MVP.md). The MVP's writes are all atomic local
    transactions, so it does not arise there.
12. **A code state is re-run from the checkpoint.** It has no durable
    effect of its own (ADR 0003), so a crash during it leaves nothing to
    resolve.
13. **Interrupted attempts are charged on recovery**: a `model_attempt`
    with no `turn` or `model_failure` after it is charged its full
    reservation (item 6) before any new attempt is admitted.

### Replay

14. **Replay uses the same seams.** The replay harness (TEST-1) runs the
    agent type with a client that returns the recorded responses and
    provider errors, a policy hook that returns the recorded permissions,
    and tool wrappers that return the recorded results without executing
    anything (REC-4). Each activation is replayed from the checkpoint and
    tail it started from, so a recovery's own decisions (item 10) are
    replayed as recorded. A
    request that differs from the recorded one, or a call the record does
    not hold, is a replay divergence and stops the replay with an error
    that names the entry.

### What the record is not

15. **Telemetry is separate** (HOST-1). Events are emitted after the
    durable commit they describe, carry their own sequence and may be
    redacted (HOST-2) or dropped; the record is neither (REC-2, REC-3).

## Consequences

- One linear log per run and one snapshot per step (ADR 0002) make
  recovery a bounded, single-order procedure: apply at most one step of
  entries, then resolve calls by a fixed table.
- Each tool call costs three commits (permission, attempt, result) and
  each provider attempt two (`model_attempt`, then `turn` or
  `model_failure`). Model turns take seconds to
  minutes, so commit latency is not material; a host whose store makes it
  so may batch, provided the order above is kept.
- `toolloop` needs no change: the decorator and the wrappers carry the
  write-ahead, and recovery re-enters `Run` with a transcript ending in
  tool results.
- The execution record holds full content, by reference, so its storage
  grows with every run. Retention and protection are the host's (REC-3).
- Hosts and agent types never read checkpoints' contents or entries'
  payloads to make decisions; the run record carries what a host needs
  (STM-8, RUN-3).

## References

- [docs/requirements.md](../requirements.md): STM-3, STM-6, STM-7, RUN-5,
  RUN-6, TOOL-3, TOOL-5, TOOL-7, TOOL-12, BUD-3, BUD-4, LLM-3, MAN-4,
  REC-1 to REC-4, HOST-1, HOST-2, TEST-1, section 7
- [MVP.md](../MVP.md): BUD-1's single-attempt client; deferral of the
  unknown outcome's resolution
- [ADR 0002](0002-serial-execution-within-an-agent-instance.md): one
  linear sequence per instance
- [ADR 0003](0003-run-model-identities-and-activation-results.md): action
  identities, activation errors
- [ADR 0005](0005-storage-one-fenced-commit.md): the fenced, atomic commit
- `maestro-llms`: `llms/toolloop` (`Run`, `ToolResult.Stop`), ADR 0017,
  ADR 0031
