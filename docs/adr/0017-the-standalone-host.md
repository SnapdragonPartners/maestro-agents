# 0017. The standalone host

- **Status:** Accepted
- **Date:** 2026-11-14

## Context

The MVP is one standalone executable, `cmd/addepar` ([MVP.md](../MVP.md)).
Everything an agent type needs from a host now exists in the library: the
run controller ([ADR 0007](0007-agent-types-and-the-run-controller.md)),
the model client ([ADR 0014](0014-model-client-and-attempt-admission.md)),
and the telemetry, logging and credential seams ([ADR
0016](0016-telemetry-logging-and-credentials.md)). What is missing is the
host itself: the small program, shipped with the library and modeled on
buggrit, that reads a declarative configuration, opens a store, turns
inputs into runs, claims and activates them, and stops cleanly. The
requirements it builds:

- **MODE-2, config file.** "One JSON file declares the agent type, its
  instance config, triggers, budgets, the model route, MCP servers and
  which stores to use. It is decoded strictly (unknown fields are errors)
  and validated before anything runs, with a `config validate` command."
- **MODE-3, pluggable stores**, chosen in the configuration: the memory and
  file stores (STORE-4; Postgres is deferred).
- **MODE-4, triggers.** One-shot and long-running; MVP.md builds one-shot
  and folder watch, with queued triggers, and defers schedules. A trigger
  arriving while its instance has a run in flight is never dropped: the
  host queues it, or starts it on a new instance (STM-9).
- **MODE-5, credentials stay outside agent code**: environment variables or
  a secrets file, read only by the host, reaching tools through TOOL-8.
- **MODE-6, graceful shutdown.** SIGINT and SIGTERM stop activations through
  their contexts at the next step boundary, each reporting its actual
  disposition, with exit codes distinguishing complete, incomplete and
  usage errors (buggrit's 0/1/2).
- **MODE-7, optional infrastructure degrades.** A failing optional sink is a
  warning, a required one fails before the run starts.
- **DEL-1, at-least-once runs**, and **HOST-4** as a single-process host
  provides it (lease, heartbeat, cancellation).

Decided with the human operator before this ADR (2026-11-13): the host is
the package `standalone` at the module root, entered through
`standalone.Main`; it runs **one instance per trigger**, its identity
derived from the input file's SHA-256, so statements run concurrently
(intake N-6) and a file dropped twice is processed once; a watched
folder's files are left in place, deduplicated by that hash, rather than
moved; and longer-lived instances belong to a future control plane. MCP
servers, schedule triggers and Postgres are out.

It amends no earlier ADR.

## Decision

### The package and its entry point

1. **`standalone` is a library package; the executable is the agent's.**
   An agent's `main` builds a `standalone.Program` (its name, a
   `*agent.Registry` holding its agent types, its model providers, and
   optionally a policy hook and a redactor) and calls
   `standalone.Main(p)`, which reads `os.Args`, installs the signal
   handling of item 21 and exits with item 22's code. `standalone.Run`
   is the same with the context, arguments and output streams passed in,
   for tests and for an executable that adds its own commands. The
   standalone host is itself a compiled-mode host (MODE-1): it only calls
   the controller and the store.
2. **Model providers come from the executable.** `Program.Providers` maps
   a `maestro-llms` provider ID to a function that builds that provider's
   client from the configured route and its credential. The configuration
   names a provider; one the program does not offer is a configuration
   error. So `standalone` imports no provider SDK, and an executable
   carries only the SDKs of the providers it offers: every importer of a
   library package pays for its imports (CLAUDE.md, *Dependency
   Discipline*). The host wraps the provider's client with
   `agent.NewModelClient` (ADR 0014), once per process, so every
   activation shares one chain and one rate limiter.
3. **Two commands.** `run -config FILE [-input FILE]` runs the
   configuration's trigger; `config validate -config FILE` decodes and
   validates the configuration (item 7) and exits, opening nothing.

### The configuration

4. **One JSON file**, every field named in snake case:
   `agent_type`, `config` (the instance configuration, any JSON),
   `namespace`, `principal`, `budgets`, `model`, `store`, `trigger`,
   `concurrency`, `max_input_bytes`, `credentials`, `telemetry` and `log`.
   Durations are strings Go's `time.ParseDuration` reads (`"10m"`). A
   relative path is resolved against the configuration file's directory,
   so a configuration means the same from any working directory.
5. **Strict decoding goes further than `encoding/json`'s.** Its
   `DisallowUnknownFields` still matches keys case-insensitively and lets
   a repeated key overwrite the first. The host walks the document's
   tokens against the configuration's types first and refuses a key that
   is not exactly a field's name, a key repeated in any object (the
   instance configuration and the secrets file included, so the schema
   validator and the host see the same value), data after the document
   and invalid UTF-8; only then does it decode. A silently ignored or
   overwritten field in a configuration that sets budgets and credentials
   is a misconfiguration nobody sees.
6. **Budgets** are `tokens`, `tokens_estimated`, `turns`, `active_time` and
   `deadline_after`. The last is a duration: a standalone run's deadline is
   its first activation's start plus it, recorded with the run's start and
   fixed for the run (ADR 0009). There is no cost budget: cost budgets are
   not built (BUD-1), and a field that can only fail a run is refused at
   decoding instead. A budget left zero takes the agent type's default
   (ADR 0011, item 7).
7. **Validation is complete before anything runs**, and reports every
   problem at once. It checks: the agent type is registered; the trigger
   kind is one the host builds (`one_shot` or `folder`) and the type's
   manifest declares (ADR 0011, item 5); the instance configuration
   validates against the manifest's schema, with the validator the
   registry uses (`internal/jsonschema`); the namespace is set; budgets
   are non-negative and an estimated token budget sets a limit; a type
   with a model state has a model route, whose provider the program
   offers and whose credential the configuration declares; the store,
   trigger, telemetry and log sections are complete and carry no field
   their kind does not use. So `config validate` catches what would
   otherwise fail each run with `config_error`, and it depends on nothing
   outside the file: it reads no credential and opens no store, so it
   runs in CI without secrets. `run` additionally reads the credentials,
   opens the store and the telemetry sink, and checks the watched folder.
8. **MCP servers are not in the configuration.** MODE-2 lists them; MCP
   tools are deferred (TOOL-6), and a field the host would ignore is
   refused by item 5 rather than accepted and dropped.

### Stores

9. **`store.kind` is `memory` or `file`** (MODE-3); `file` takes `store.dir`.
   The file store's exclusive lock keeps every other process out of the
   directory (ADR 0005, item 12), and opening it finishes interrupted
   commits and leaves every recorded owner without a lease, so the host
   claims every unfinished run at once after a restart (ADR 0005, item
   16). A store serves one configuration: runs of another agent type in it
   are left alone (item 14), an input whose run is another agent type's is
   refused (logged once in a watch, not retried until the file changes),
   and a run created by an earlier configuration of the same type and not
   yet started starts under the current one.
10. **A store the file store has poisoned stops the host** (ADR 0005, item
    14): an operation returning `store.ErrUnusable` ends the process with
    exit 1, after the other activations have stopped, leaving the restart
    to the supervisor. Any other store failure is retried (item 17).

### Triggers, instances and runs

11. **One instance per trigger.** The host reads the input file, at most
    `max_input_bytes` (64 MiB by default), and hashes it. The trigger ID is
    `sha256:<hex>`, the instance ID and the run ID are `sha256-<hex>`. The
    trigger's payload is a `standalone.FileTrigger`: the file's absolute
    path, base name, size and SHA-256. An agent type reads the input with
    `FileTrigger.Read`, which refuses a file whose size or hash no longer
    matches: the file is left in place, so it can change after the host
    hashed it, and a run must process the bytes its identity names.
12. **Every trigger becomes a run at once**: `EnqueueTrigger`, then
    `CreateRun`, then `RemoveTrigger`. Each is idempotent on the trigger's
    identity (ADR 0006, item 8), so a crash between any two is repaired by
    doing them again, and a file seen twice, by two scans or two
    processes' lifetimes, finds its existing run (`created` false) rather
    than making a second: the dedupe the human operator chose. The queued
    trigger record is written as MODE-4 asks, but with one trigger per
    instance an instance never has a run in flight when its trigger
    arrives, so the queue that matters is the set of runnable runs: the
    durable runs are themselves the queue, `Claimable` reads it, and it
    survives a restart with a durable store. With the memory store it does
    not, which choosing that store accepts (MODE-4).
13. **A run is processed once.** The same bytes name the same run, so
    running a file again, after its run succeeded or failed, reports that
    run's outcome and does not run it again. Re-running an input
    deliberately (after a transient `model_error`, say) needs a new trigger
    identity, which this host does not offer: that is a control plane's
    job, deferred with longer-lived instances. A runnable run, left by a
    shutdown or a crash, is continued: running its file again in one-shot
    mode activates it from its last step boundary.
14. **The scheduler activates claimable runs up to `concurrency`** (default
    1). It lists `Claimable`, skips runs of another agent type, runs
    already executing in this process and runs waiting out a retry delay,
    and claims each with a one-minute lease that a heartbeat renews every
    twenty seconds (HOST-4). The host tracks the runs it is executing by
    instance, so the lease (which, in the file store, lives in this
    process's memory) is never what keeps it from claiming its own
    instance twice. Claimable runs come in instance order, which
    is hash order, not arrival order; the MVP needs no ordering. An
    activation runs with no step limit.

### One-shot and folder watch

15. **One-shot** (`trigger.kind` `one_shot`) takes its input from
    `-input`: it makes the input's run (item 12), activates it if it is
    runnable, prints the run's outcome as JSON to standard output and
    exits (item 22). The outcome includes its detail and the input's path:
    standard output is the command's result for the operator who named
    the input, not a log line, so item 20's rule for logs does not cover
    it. It activates only that run; other unfinished runs in
    the store wait for their own input or for a folder watch.
16. **Folder watch** (`trigger.kind` `folder`, with `dir`, `pattern`,
    default `*`, and `poll_interval`, default five seconds, at least 100
    milliseconds) polls the folder rather than subscribing to file-system
    events: polling is portable, needs no dependency, and a few seconds'
    latency is nothing to a statement's processing. It considers regular
    files directly in the folder whose names match the pattern; it skips
    subdirectories, symbolic links (a link could point anywhere outside the
    folder the operator chose) and names starting with `.` (editors' and
    copiers' temporary files). A file triggers only once its size and
    modification time are unchanged across two consecutive scans, so a
    file still being copied in is not hashed half-written; a file whose
    size or modification time changes later is hashed again, and new
    bytes are a new run. At start-up it activates every claimable run of
    its agent type before the first scan finds anything new, so unfinished
    work resumes at once.

### Activations and their failures

17. **The host handles `Activate`'s errors as ADR 0003 item 14 says.**
    After any error it releases the claim if it still holds one and
    rereads the run: terminal means the run is finished and the reread
    status is its outcome; `runnable` means it is retried, after a delay
    that doubles from one second to five minutes per instance while the
    process lives (DEL-1: recovery continues from what landed).
    `ErrNotActivatable` (the run's agent type is missing, say) is not
    retried while the process lives. A store failure is not a run failure
    (ADR 0003, item 15): the host never marks a run failed for one.
18. **A heartbeat that finds its ownership gone cancels the activation**,
    whose next commit then fails with `ErrOwnershipLost`; other heartbeat
    failures are logged and retried at the next beat. A heartbeat that
    races the activation's final commit, which releases the claim (ADR
    0003, item 10), finds the instance ownerless at its own epoch and stops
    quietly. In one process no one else claims, so cancelling is a guard,
    not a path the MVP takes.

### Credentials, telemetry and logs

19. **Credentials** (MODE-5): `credentials.env` maps a credential's name to
    the environment variable holding it, and `credentials.file` names a
    secrets file, a JSON object from name to value decoded as strictly as
    the configuration. A name in both is a configuration error. The host
    reads both once, when `run` starts, and refuses a secrets file that
    its group or others can read or write, as `ssh` refuses a private key.
    The host's credential provider (`agent.WithCredentials`) serves only
    those names, and only to runs whose principal (fixed at the run's
    start, ADR 0016 item 12) is the configured one: a run started under
    another principal does not get this configuration's credentials. The
    model route's credential comes from the same set and goes only to the
    provider function.
20. **Telemetry and logs** (HOST-1, HOST-2, MODE-7). `telemetry.sink` is
    `none` (the default), `store` (the store's `store.Telemetry`, through
    `agent.StoreSink`) or `jsonl` (one JSON event per line, appended to
    `telemetry.path`, created owner-only). `telemetry.required` decides
    what a sink that cannot be set up does: a required one fails `run`
    before any run starts (exit 2); an optional one is a warning, and the
    host runs without telemetry. A sink that fails while running is
    always a warning (ADR 0016, item 4). Events leave through the
    program's redactor, or with `Detail` cleared without one (ADR 0016,
    item 3). The host logs through one `log/slog` logger to standard
    error, `log.level` and `log.format` (`text` or `json`) choosing its
    handler, and gives the same logger to the controller (ADR 0016, item
    10). Its own lines carry IDs, statuses, codes and counts, and never an
    input's path or name, which can name a client (intake N-5); the
    instance ID, the input's hash, is how an operator matches a log line
    to a file.

### Shutdown and exit codes

21. **Signals.** `Main` turns the first SIGINT or SIGTERM into the
    cancellation of the root context. Its cause is neither
    `agent.ErrSuperseded` nor `agent.ErrOperator`, so each activation
    reads it as a shutdown and commits its actual disposition at the next
    step boundary (ADR 0003, item 17): `runnable` while working. The host
    stops scanning, waits for every activation to return, and closes the
    store. A second signal restores the default handling and so ends the
    process at once; the stores are crash-consistent (ADR 0005), and
    recovery continues what an interrupted activation left (ADR 0004).
22. **Exit codes**, after buggrit's: **0**, complete: the one-shot run
    succeeded, or a watch stopped with no activation interrupted; **1**,
    incomplete: the one-shot run did not succeed (it failed, was
    cancelled, or was left runnable by a shutdown or a store failure), or
    a watch's shutdown interrupted an activation or a poisoned store ended
    it; **2**, usage: bad arguments, an invalid configuration, missing
    credentials, a store, required sink or watched folder that cannot be
    opened. In a watch, a run that fails is an outcome, logged, not a
    reason to exit non-zero: the daemon did its job.

## Consequences

- An agent's executable is a few lines: register its types, list its
  providers, call `standalone.Main`. The intake agent's `cmd/addepar`
  adopts it in the vertical slice; until then the package is exercised by
  its tests, with test agent types.
- The configuration is the whole deployment: what to run, where state
  lives, how to reach the model, which credentials exist and what to
  observe. `config validate` checks it without secrets or a store.
- One file is one run, for the store's lifetime. Re-running an input on
  purpose waits for a control plane; deleting the store forgets every run
  and so allows it, at the cost of everything else the store held.
- A crash or restart loses nothing a durable store committed: runs are
  created before they are claimed, and the scheduler's queue is the
  store's set of runnable runs.
- `standalone` depends on `maestro-llms`' core packages only; the provider
  SDKs an executable offers are its own imports.

## Not decided here

- **The intake agent's own configuration and trigger handling**, and the
  operator's acceptance of a copy-restricted PDF at intake (ADR 0015,
  point 4): the vertical slice.
- **Schedule triggers, MCP servers and the Postgres store** (MODE-4, TOOL-6,
  STORE-4): deferred by MVP.md.
- **Re-running an input**, longer-lived instances and any other control
  plane: a later orchestrator's.
- **A shared limiter's degradation** (MODE-7): with the Postgres bucket.

## References

- [Requirements](../requirements.md): MODE-1 to MODE-7, HOST-4, DEL-1,
  STM-9, TOOL-8
- [MVP.md](../MVP.md): the standalone host's rows
- [ADR 0003](0003-run-model-identities-and-activation-results.md): items
  12 to 17, activations and their errors
- [ADR 0005](0005-storage-one-fenced-commit.md): items 12 to 16, the file
  store's lock, poisoning and reopen
- [ADR 0006](0006-store-interface.md): item 8, idempotent triggers and runs
- [ADR 0011](0011-manifests.md): trigger kinds, configuration schemas and
  default budgets
- [ADR 0014](0014-model-client-and-attempt-admission.md): the model client
- [ADR 0016](0016-telemetry-logging-and-credentials.md): telemetry,
  redaction, logging and credentials
