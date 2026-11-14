# 0016. Telemetry, logging and credentials

- **Status:** Accepted
- **Date:** 2026-11-13

## Context

The standalone host (MODE-2 to MODE-7) is the next step of the MVP. It
needs three host seams the library does not have yet:

- **HOST-1, telemetry events.** "State transitions, model turns, tool
  calls, policy decisions, usage and compactions are emitted as events
  for observation. Events have a stable identity (run, sequence, unbroken
  across activations) so a host can deduplicate redelivered events. A nil
  sink records nothing." Telemetry is never the recovery record (REC-2),
  and [ADR 0004](0004-execution-record-and-recovery.md) item 15 has
  events "emitted after the durable commit they describe". [ADR
  0005](0005-storage-one-fenced-commit.md) item 13 reserves
  `telemetry/<run>/<seq>.json` in the file store, written "idempotently
  on their identity, not through `Commit`", and [ADR
  0006](0006-store-interface.md) item 10 leaves telemetry storage out of
  the store interface until the sink arrives.
- **HOST-2, redaction of telemetry and logs.** The host can redact event
  content before it leaves the run, so telemetry can carry hashes and IDs
  rather than document text or amounts (intake N-5). The library logs
  through a `log/slog` logger the host injects, never logs tool
  arguments, tool results or model content, and gives agent types and
  tools the same logger.
- **TOOL-8 and HOST-5, credentials at call time.** Every tool call carries
  an opaque run context identifying the run and its principal. The tool
  asks the host for a credential when it executes, and the credential
  provider and the policy hook are both keyed by the run context.

The MVP needs no delivery guarantees by kind (DEL-2, deferred), and no
compaction events (LOOP-5, deferred).

It amends, item by item:

- ADR 0003 item 12 (what `ActivationInput` carries): by item 12, it also
  carries the run's principal.
- ADR 0004 item 2 (what `run_started` holds): by item 12, it also records
  the run's principal.
- ADR 0006 item 10 (telemetry storage is not in the store interface): by
  item 7, it arrives as the separate, optional `store.Telemetry`
  interface, not as an addition to `store.Store`.
- ADR 0006 item 11 (what `storetest` runs): by item 9, it also checks the
  `store.Telemetry` contract, for a store that implements it.
- ADR 0006's consequence that telemetry storage arrives "as an addition
  to this interface": by item 7, it arrives as a separate one.
- ADR 0007 item 4 (what the activation input carries): by item 12, the
  run's principal too.
- ADR 0007 item 6 (what ends a run before its start commit lands): by
  item 12, a principal that is not valid UTF-8 ends it too, with
  `config_error`, and the ending records the start.
- ADR 0007 item 8 (the checkpoint's contents): by item 12, the run's
  principal too.
- ADR 0007 item 13 (what `agent.Env` exposes): by items 11 and 12, also
  the host's logger (`Env.Logger`) and the run context
  (`Env.RunContext`).
- ADR 0007's consequence listing telemetry events (HOST-1) as not yet
  built: items 1 to 6 build them.
- ADR 0008 item 3 (what a `Call` carries): by items 12 and 13, also the
  run context (`Call.RunContext`) and a way to ask for a credential
  (`Call.Credential`).
- ADR 0008 item 5 (the policy hook's request): by item 12, it also
  carries the run context (`PolicyRequest.Run`).
- ADR 0008's consequence listing credentials and the run context (TOOL-8)
  and telemetry events (HOST-1) as not yet built: items 1 to 6 and 12 to
  14 build them.
- ADR 0009's consequence listing usage telemetry (HOST-1) as not yet
  built: items 1 and 2 build it, as `turn` and `model_failure` events
  carrying usage, and `outcome` events carrying consumption.
- ADR 0011 item 8 (what fails a run with `config_error`): by item 12, a
  principal that is not valid UTF-8 too.

## Decision

### Telemetry events

1. **Each event projects one execution-record entry, after its commit.**
   The runtime emits telemetry in one place: after a commit lands, it
   emits one event for each entry the commit appended, in order. An
   event's identity is (run, sequence), and its sequence is the entry's
   sequence. Identities are therefore unbroken across activations, and a
   redelivered event has the same identity. No event describes a write
   that did not land. The entry kinds cover HOST-1's list:
   - `transition`: state transitions;
   - `model_attempt`, `turn` (with usage) and `model_failure`: model
     turns and usage;
   - `attempt` and `result`: tool calls;
   - `permission`: policy decisions;
   - `activation_started`, `run_started` and `outcome`: the run's
     lifecycle.

   Compaction events come with LOOP-5.
2. **An event holds metadata, never content.** `agent.Event` carries the
   run, instance, agent type, sequence, step, kind and emission time.
   It also carries only the fields of its entry that name or count
   things:
   - state names;
   - the model route, the attempt number and the wrap-up flag;
   - usage;
   - the tool's name, call index, effect, permission and result status;
   - the outcome's status, reason and consumption;
   - the claim's owner and epoch;
   - the trigger's ID and kind, and the namespace.

   Some entry fields are never projected: state data, object operations
   and their values, the trigger's payload, the configuration, the
   principal, artifact digests of prompts and transcripts, tool
   arguments and results, and model content. A host that needs them
   reads the execution record, which is protected (REC-3) rather than
   redacted.
3. **Free text is withheld unless the host's redactor keeps it.** Three
   entry fields are free text that may quote content:
   - an outcome's detail (a code state's error, for instance);
   - a model failure's message;
   - a permission's reason.

   They go in `Event.Detail`. A controller built without a redactor
   clears `Detail` before an event leaves. `WithRedactor` installs the
   host's `Redactor`, which receives each full event and edits it in
   place; what it leaves is emitted (HOST-2). So the default fails
   closed: telemetry carries only IDs, names, counts and codes. One
   controller serves many instances concurrently, so the redactor may be
   called concurrently and must be safe for concurrent use.
4. **The sink is the host's.** `agent.Sink` has one method,
   `Emit(ctx, []Event) error`, which receives one commit's events per
   call, in sequence order. `WithTelemetry` sets it, and with no sink
   nothing is emitted. Emission is synchronous, so within one activation
   a sink sees the run's events in order. Across activations it may not:
   an activation's final commit releases ownership (ADR 0003, item 10)
   before its events are emitted, so the next owner, in this process or
   another, can emit later events while that last batch is still in the
   sink. No fence orders two owners' emissions, so a sink that needs a
   run's events in order sorts them by sequence; identities make that
   possible, as they make deduplication possible. `Emit` is called
   concurrently, for different runs and across a handoff for the same
   run, so a sink must be safe for concurrent use. Its context ends
   after `TelemetryTimeout` (5 s) and is not cancelled with the
   activation, so a shutdown still emits the final commit's events. A
   sink that needs more time buffers on its own.
5. **A sink's failure never fails the run.** An error from `Emit`, or a
   panic in it or in the redactor, is logged at warn level with the run,
   the sequence range and the failure's class (`sink_error`,
   `sink_timeout`, `sink_panic`, `redactor_panic`), never the error's text
   or the panic's value, which may quote what the redactor was meant to
   remove; a panicking redactor's batch is dropped. The activation
   continues (MODE-7: a failing optional sink is a warning). Whether a sink is required, and failing before the run
   starts if it is, is the host's decision (MODE-7, in the standalone
   host).
6. **Delivery is best effort.** Two gaps are accepted:
   - A commit whose result is indeterminate (ADR 0003, item 14) emits
     nothing, even if it landed. The activation returns an error, and its
     events are not re-emitted later.
   - A crash between a commit and its emission loses that commit's
     events.

   The execution record holds every entry either way, so per-run token
   use (REC-1) is never lost, only its event. At-least-once delivery for
   usage events is DEL-2, deferred. It can be built by re-emitting the
   execution record's tail when an activation starts, without changing
   identities.

### Telemetry storage

7. **`store.Telemetry` is a separate, optional interface.** Most hosts
   send telemetry elsewhere, so it is not part of `store.Store`, which
   host stores implement. It has two methods:
   - `AppendEvents(ctx, []store.Event) error` writes each event
     idempotently on (run, sequence): an event already stored is kept as
     it was, and a repeat is not an error;
   - `Events(ctx, run, after)` returns the run's events with sequence
     numbers greater than `after`, in order.

   `store.Event` is STORE-1's telemetry record: run, sequence, kind,
   time and a JSON payload.
8. **Both reference stores implement it**, through the shared engine, at
   ADR 0005's path `telemetry/<run>/<seq>.json`. The writes do not go
   through `Commit`, so they are not fenced (telemetry is not state). They
   share the engine's single writer and the file store's atomic writes.
   One known consequence: a failure after the commit point of a telemetry
   write (a failed directory sync) poisons the file store like any other
   write (ADR 0005, item 14). That failure means the disk is failing, and
   the run's next commit would meet it anyway.
9. **`agent.StoreSink` adapts a `store.Telemetry` to a `Sink`.** It
   encodes each event as its record's payload. The standalone host uses it
   with the store it opened, and `storetest` checks the interface's
   contract for every store that implements it.

### Logging

10. **The host injects one `*slog.Logger`.** `WithLogger` sets it, and by
    default the library logs nothing. The runtime logs these, at most at
    info level for ordinary events:
    - an activation's start, with run, instance and epoch;
    - its end, with status and reason code;
    - an activation that ends with an error (a refusal, a store failure,
      lost ownership), at error level, with the error's class only
      (`ownership_lost`, `store_failure`, `not_activatable`, `other`);
    - a sink's failure, at warn level, with its class (item 5).

    The runtime's log lines carry IDs, names, codes and counts, never an
    outcome's detail, an error's or a panic's text (either may quote
    content: a store failure can name an object key), tool arguments or
    results, configuration or model content. The host has every error
    itself, from `Activate`'s return or from its own sink, and logs it
    under its own policy.
11. **Agent code logs through `Env.Logger()`.** It is the host's logger
    with the run, instance and state as attributes, so one host policy
    (level, handler, redaction) governs every log line. What an agent
    type's own lines carry is its own responsibility. The intake agent's
    rule (hashes and IDs only, N-5) is enforced in its code and tests, not
    by the library.

### Credentials and the run context

12. **The run context is built by the runtime.** `agent.RunContext` holds
    the run's scope, instance, agent type and principal.
    `ActivationInput.Principal` is the host's opaque name for whose
    authority the run acts under (an operator, a service account). Like
    the namespace, it is recorded in `run_started` and the checkpoint and
    fixed for the run, so a later activation ignores the field. A
    principal that is not valid UTF-8 fails the start with `config_error`,
    and, like any terminal ending before the start lands, that ending
    records the start (ADR 0007, item 6).
    An empty principal is allowed: such a run acts under the host's own
    authority. The policy hook receives the run context in
    `PolicyRequest.Run` (HOST-5).
13. **A tool asks for a credential when it executes.**
    `Call.Credential(ctx, name)` calls the host's `CredentialProvider`
    (set with `WithCredentials`) with the call's run context and the
    credential's name, and returns a `Secret`. The provider travels on
    the `Call` the runtime makes for a tool it executes, never on `Env`,
    so a code state, or any `Call` built outside the runtime, has none.
    An `atomic_local` tool
    receives no provider (ADR 0005, item 9, which this ADR keeps): its
    every request fails with `ErrNoCredential` without reaching the host's
    provider, so it still has no effect beyond its operations. Tools of
    many runs call it concurrently, so a provider must be safe for
    concurrent use. Without a provider every request fails with
    `ErrNoCredential`, which the tool handles like any
    failure (as a tool error the model receives, or an error that fails
    the run). The runtime never records or logs a credential.
14. **`Secret` resists accidental disclosure.** Its value is reached only
    through `Reveal`, and is held behind a pointer, so where `fmt` bypasses
    the methods below (a `Secret` in an unexported struct field, which it
    prints by reflection) it prints an address, never the value. Its
    `String`, `GoString`, `Format`, `LogValue`, `MarshalJSON` and
    `MarshalText` all render `[redacted]`, so a secret passed by mistake
    to a log, a format verb, a text encoding or a JSON result (which the
    execution record stores) does not leak its value. A tool that puts `Reveal()` into a
    result has disclosed it, and the record keeps it: that is the tool's
    defect.

## Consequences

- One choke point emits telemetry, the activation's commit. A new entry
  kind is emitted automatically, and its projection names the fields
  safe to emit. Until it has one, only the common fields go out.
- Hosts get deduplication for free from entry sequences. The cost is that
  an event's kind follows the record's vocabulary (`attempt`, `result`)
  rather than a separate telemetry vocabulary.
- A sink sits on the activation's path. It is bounded by
  `TelemetryTimeout`, but a slow sink still slows the run, so production
  sinks buffer.
- With no redactor, telemetry never carries free text, so a host loses
  failure details from telemetry until it installs one. The details are
  always in the execution record.
- Tools that need credentials, the policy hook and the credential
  provider share one run context. LOOP-4 adds a chat turn's scope to it
  without changing its shape.
- `run_started` and the checkpoint gain an optional `principal` field.
  Records and checkpoints written before it read as an empty principal.

## Not decided here

- Telemetry delivery guarantees by kind, and reporting incomplete
  delivery (DEL-2).
- Result redaction before the model sees a result (TOOL-9) and output
  caps (TOOL-10). Both are separate library work.
- Which sinks the standalone host offers, how its configuration marks one
  required, and where its credentials come from (MODE-5, MODE-7): ADR
  0017, with the host.

## References

- [docs/requirements.md](../requirements.md): HOST-1, HOST-2, HOST-5,
  TOOL-8, REC-1 to REC-3, STORE-1, DEL-2, MODE-7, LOOP-4, LOOP-5
- [ADR 0003](0003-run-model-identities-and-activation-results.md), items
  1, 10, 12 and 14
- [ADR 0004](0004-execution-record-and-recovery.md), items 2 and 15
- [ADR 0005](0005-storage-one-fenced-commit.md), items 9 (an
  `atomic_local` tool receives no credential provider), 13 and 14
- [ADR 0006](0006-store-interface.md), items 10 and 11
- [ADR 0007](0007-agent-types-and-the-run-controller.md), items 4, 6, 8
  and 13
- [ADR 0008](0008-model-states.md), items 3 and 5, consequences
- [ADR 0009](0009-budgets.md), consequences
- [ADR 0011](0011-manifests.md), item 8
- [ADR 0012](0012-namespace-confinement.md): the namespace, which the
  principal follows
