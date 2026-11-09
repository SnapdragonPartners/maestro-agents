# 0008. Model states

- **Status:** Accepted
- **Date:** 2026-11-09

## Context

[ADR 0004](0004-execution-record-and-recovery.md) fixed the write-ahead
order of a model turn (items 6 to 9) and its recovery (items 10 to 13),
with entry names marked illustrative. [ADR 0007](0007-agent-types-and-the-run-controller.md)
built the runtime for code states and left model states, and TEST-5 in the
store conformance suite, to the next PR. This ADR records the API and the
decisions building model states raised:

1. how a model state, its tools and the host's policy hook are declared
   (STM-2, STM-3, TOOL-1 to TOOL-3, TOOL-12);
2. how the model client reaches a run (LLM-1, LLM-3);
3. how a model state's request and transcript are recorded and restored
   (STM-6, REC-1);
4. when the results `toolloop` synthesizes itself are committed;
5. what LOOP-3's completion policy does when the model never calls a tool;
6. how a tool's defects, an unknown write and the policy hook's failures
   end a run;
7. where TEST-5 runs.

It amends ADR 0004 item 8 (the commit that carries synthesized results),
ADR 0006 item 11 (TEST-5 joins the suite, item 20) and ADR 0007 items 2 (a
state's third kind, item 1) and 9 (the tail's model-state kinds, item 11).

## Decision

### Declaring a model state

1. **A state's third kind is `Model *ModelState[D]`** beside `Code` and
   `End` (ADR 0007, item 2): exactly one is set, and a model state declares
   `Next` like a code state. `ModelState` holds `Prompt`, `Tools` and an
   optional `Nudge`.
2. **`Prompt(ctx, env, data)` returns an `llms.ChatRequest`**: system
   instructions, the first messages (at least one) and the parameters
   (`MaxTokens`, `Temperature`, `ToolChoice` and the rest), with `Tools`
   empty, since the state offers its `Tools`. It runs when the state's first
   attempt is prepared, and its request is recorded with that attempt. Until
   the state's first turn lands it may run again (after a crash, a failed
   attempt or a shutdown), so, like `Init`, it must have no durable effect
   of its own; its errors are a code state's (ADR 0007, items 12 and 13).
3. **A tool is `Tool[D]`**: an `llms.ToolDefinition`, an `Effect` (`read`
   or `write`, TOOL-2), a write tool's `Contract` (TOOL-12:
   `atomic_local`, `idempotent`, `reconcile`, or `unprotected` when the
   agent type accepts the risk) and `Execute(ctx, *Call[D])`. `Register`
   refuses a write tool with no contract, a read tool with one, duplicate
   or empty tool names and a missing `Execute` (MAN-3). A `Call` carries the
   state's data, decoded afresh for every execution, the call as the model
   emitted it, its action identity and, for a write, its idempotency key.
4. **A tool's `Result` can carry a transition** (STM-3): `To` names a state
   in `Next`, with the next state's `Data`. A result that transitions cannot
   also be an error. **Only an atomic local transaction may return object
   operations** (`Ops`, ADR 0005 item 7), which commit with its result. A
   tool's read view is the run's `Env`; confining it to the namespaces a
   grant names comes with manifests (MAN-1, MAN-2).
5. **The policy hook is `Policy`**, set with `WithPolicy` on the controller
   (TOOL-3): it answers `allow`, `deny` or `pending` with a reason. The
   default allows every call. The recorded permission takes one of STM-6's
   seven values; the MVP records `allowed` and `denied`. A denied call's
   result is `tool call not permitted (denied)` with the reason, as data.

### The model client

6. **The client is `ActivationInput.Client`** (LLM-1): per activation, not
   per controller, so a host can route each run differently and one
   controller still serves every instance. A model state reached with no
   client fails the run with `model_error`. Each attempt and turn records
   the route `Client.Model()` reports (LLM-3); a response carries no served
   model identity in maestro-llms v1, so recording the identity as served
   waits on an upstream field.
7. **Each model state runs `toolloop.Run`** with no iteration limit; the
   activation's step limit (STM-7), its context and, once built, its budgets
   bound it. The decorator refuses a new attempt at a step boundary when the
   step limit is reached or the context has ended, which stops the loop
   there.

### The record and the transcript

8. **Entry payloads** are `ModelAttemptEntry`, `TurnEntry`,
   `ModelFailureEntry`, `PermissionEntry`, `AttemptEntry` and `ResultEntry`
   (ADR 0004, item 2). Large content is an artifact referenced by digest
   (ADR 0004, item 1), in the same commit:
   - the request's **parameters**: the `ChatRequest` without messages, with
     the state's tool definitions, which also gives MAN-4 a digest of what
     the model was told and offered;
   - each **message** of the transcript, one artifact each, so a document
     in one message is stored once however many checkpoints hold it;
   - each **response** (the `ChatResponse` without its provider-specific
     `Raw`) and its assistant message;
   - each call's **result** as the model receives it (`Content` or `Parts`,
     and `IsError`), so non-text results (TOOL-11) are recorded whole.

   **What is recorded reads back byte for byte.** Messages, tool calls and
   results go through one recorded form (`agent/wire.go`) that keeps
   tool-call arguments as bytes, since `encoding/json` would compact a
   `json.RawMessage` and refuse a malformed one; recovery hashes and sends
   exactly what was received, and malformed arguments reach the tool, which
   answers as it chooses. Every string is checked as valid UTF-8 where it
   enters, since `encoding/json` would replace invalid bytes: what a model
   state declares (its nudge, its tools' names, descriptions and schemas,
   which must also be valid JSON) is refused at registration; a prompt's
   invalid messages or system instructions fail the run with
   `state_error`, an invalid response with `model_error`, and an invalid
   tool result or policy reason with `tool_error`. Observations that never
   reach the model (an error's text in a `model_failure` entry or an
   outcome's detail) are recorded as `encoding/json` encodes them. The
   parameters are stored in `encoding/json`'s default form, read back with
   numbers kept exact (`json.Number`), and **the request sent is the one
   decoded from that record**, live as on recovery, so whatever
   `encoding/json` normalizes (invalid UTF-8 in metadata or provider
   parameters, typed values) is normalized alike for both; a value it
   cannot encode (a cycle) fails the run with `state_error`. A tool's
   schema is copied and compacted at registration, as the parameters hold
   it. **Restored parameters keep the tools the loop offers**: when the
   recovering version declares the state's tools differently from the
   recorded parameters (a changed schema, a tool added or removed), its
   next attempt records new parameters, with the recorded system
   instructions and settings and the current tools, so every attempt's
   parameters are what it sent; `Prompt` does not run again.
9. **A model attempt records its request as a delta** (ADR 0004, item 2):
   the parameters' digest, `Base` (the number of messages the state's
   latest checkpoint holds) and the digests of the messages added since,
   such as a nudge.
10. **The checkpoint gains a model state's progress** (ADR 0007, item 8): the
    parameters' digest, the transcript as message digests, and the nudge
    state (item 13). Older checkpoints, which lack the field, read the same,
    so the format stays 1. Each turn's completion checkpoints the transcript
    extended by the turn's request messages, its assistant message (unless
    the response is empty, which `toolloop` does not append either) and its
    results message. **Each turn is a step**, and the action identity's step
    (ADR 0003, item 4) is that turn's.
11. **Recovery reads the tail as one step** (ADR 0007 item 9 gains model
    states' kinds): activation starts and non-terminal endings, model
    attempts that failed or were interrupted, and at most one turn with its
    calls' permissions, attempts and results. A record that breaks that
    shape (a call entry with no turn, an attempt after an unresolved turn, a
    resolved turn with no checkpoint, model entries past a code state's
    checkpoint) is refused with `ErrNotActivatable` before anything is
    written. A recovered turn is rebuilt from its recorded attempt and
    response, so `Prompt` never runs again for it, and its unresolved calls
    are resolved in order by ADR 0004 item 10's table through the same path
    a live call takes.
12. **A call issued again commits a new attempt entry** before it is sent,
    with the same action identity, arguments digest and idempotency key: an
    attempt entry records each send.

### Synthesized results

13. **(Amends ADR 0004, item 8.)** The results `toolloop` synthesizes for a
    call naming no offered tool are committed **with the runtime's next
    commit**, whichever it is: the next wrapper's permission, attempt or
    result, or the decorator's next attempt, or on the loop's return. ADR
    0004 named only the decorator's commits, which suffices when such a call
    is the turn's last, but a later offered call's wrapper commits first.
    **An ending is a commit too**: one made while a model state runs (a
    policy failure, a tool's defect, a shutdown) carries the pending results
    and, if they complete the turn, its checkpoint. Pending results stay
    pending until a commit carrying them lands, so a rejected or abandoned
    commit never drops them. Either way the content recorded is the loop's
    own, captured from `OnToolCall`, and a commit that completes the turn
    carries its checkpoint. Recovery refuses such a call with the same text the loop
    uses (`tool "x" is not available`), so the transcript reads alike.

### Completion and failures

14. **LOOP-3's model-state policy.** A turn that calls no tool (a text
    answer, or an empty one) is complete once recorded, with its checkpoint;
    the next request adds the state's nudge as a user message. A second
    answer in a row with no tool call fails the run with **`wrap_up_failed`**,
    whose meaning widens to "a model state's completion policy did not
    produce a transition"; forced wrap-up near a budget's end comes with
    budgets.
15. **A tool's defect fails the run with `tool_error`**: `Execute` returned
    an error or panicked, or its result is one the runtime cannot record (a
    transition to an undeclared state or with `IsError`, operations from a
    tool that is not an atomic local transaction, operations of any kind but
    object puts and deletes, `Parts` that break maestro-llms' rules, a
    string that is not valid UTF-8, or operations the store refuses as
    invalid). The result is checked against all of `toolloop`'s rules
    (maestro-llms' `CheckToolResult`: no empty text part, a media type that
    parses and matches its part) before anything is committed, so the loop
    never refuses a result already recorded; should it still refuse one
    after a transition committed, the transition stands. `tool_error` stays with tools
    as ADR 0007 item 12 left it, distinct from `state_error`. A model-visible
    failure is a result with `IsError`. As for a code state (ADR 0007,
    item 13), an error wrapping a `*ReadError` from the tool's `Env` ends the
    activation with a store failure instead, not a run failure, unless the
    context has ended (item 18).
16. **The policy hook's failure fails the run with `tool_error`**: an error,
    a panic, or an unknown verdict. A `pending` verdict does too, until
    approval is built (MVP.md, TOOL-4).
17. **An unknown write fails the run** (ADR 0004, item 11): a write that is
    not an atomic local transaction, attempted with no result, gets a
    `result` entry with status `unknown` in the commit that fails the run
    with `tool_error`. **An attempt records its tool's effect and write
    contract**, and recovery partitions an attempt with no result by what
    it recorded, not by what the recovering version declares: a write
    attempted by a tool since removed or redeclared still has an unknown
    outcome.
18. **The context's end** (ADR 0003, item 17; ADR 0007, item 14): a read's
    or an atomic local transaction's result returned after the context ended
    is discarded, since its effect commits only with its result, and
    recovery issues the call again. Any other write's error after the
    context ended (a store read the end interrupted included) ends the
    activation by the cause, leaving its attempt for recovery to resolve
    as unknown. Any other write's result is recorded, as
    its effect may have happened; if it transitions to a terminal state the
    transition and its checkpoint commit but the run's ending yields to the
    cause, and a later activation that finds a terminal checkpointed state
    ends the run with that state's ending (ADR 0007).
19. **A model failure** commits its `model_failure` entry, then the run fails
    with `model_error`, or, when the context ended, the cause decides. A
    `toolloop` configuration error (a tool choice naming no offered tool)
    fails the run with `state_error`.

### TEST-5

20. **TEST-5 joins `storetest`** (ADR 0006, item 11): `CrashRecovery` runs a
    model-state agent against the store under test, stops the activation at
    each commit in turn, before it lands and after it lands but before its
    result returns, continues the run in a new activation (after a reopen,
    for a durable store, and the lease's expiry), and checks that each write
    landed exactly once, no denied or not-executed write landed, every turn
    was recorded once (none was asked of the model again once recorded) and
    every call was resolved. A self-test shows it fails a store whose reads
    of an instance's checkpoints are stale. `storetest` therefore imports
    `agent` and maestro-llms' `testllm`.

## Consequences

- An agent type writes a model state as a prompt function and tools over
  its own data type; the record, checkpoints and recovery are the runtime's.
- A tool call costs three commits and an attempt two, as ADR 0004 expected;
  a refused call's result rides on the next commit.
- maestro-llms is a dependency of `agent`, and of `storetest` with
  `testllm`. Until maestro-llms v1.2.0 is tagged the module requires a
  pseudo-version of its main branch, which has image and document content
  (maestro-llms #147, #148).
- Not yet built, and decided with their own work: budgets (each attempt's
  reservation in `model_attempt`, interrupted attempts charged on recovery,
  the turn budget, forced wrap-up), manifests and tool grants (MAN-1,
  MAN-2), tool middleware (TOOL-7: redaction TOOL-9, output caps TOOL-10,
  the visible clock), credentials and the run context (TOOL-8), skills
  (SKL), compaction (LOOP-5), telemetry events (HOST-1), the replay harness
  (TEST-1), approval (TOOL-4, STM-8) and resolving an unknown outcome by
  reconciliation probe (TOOL-12).

## References

- [docs/requirements.md](../requirements.md): STM-2, STM-3, STM-6, STM-7,
  LLM-1, LLM-3, TOOL-1 to TOOL-5, TOOL-11, TOOL-12, LOOP-1, LOOP-3, MAN-4,
  REC-1, REC-2, TEST-5
- [MVP.md](../MVP.md): BUD-1's single-attempt client; TOOL-4's contract
- [ADR 0003](0003-run-model-identities-and-activation-results.md): action
  identities (item 4), endings (item 17)
- [ADR 0004](0004-execution-record-and-recovery.md): the write-ahead turn
  and recovery (items 2, 4 to 13), which this ADR amends (item 8)
- [ADR 0005](0005-storage-one-fenced-commit.md): atomic local transactions
  (items 7 to 10)
- [ADR 0006](0006-store-interface.md): TEST-5 in the suite (item 11)
- [ADR 0007](0007-agent-types-and-the-run-controller.md): the API this
  extends (items 2, 8, 9, 12 to 14)
- `maestro-llms`: `llms/toolloop` (`Run`, `ToolResult.Stop`,
  `OnToolCall`), ADR 0017, ADR 0041
