# The external contract

This is the reference for anyone who runs agents built on
`maestro-agents`: Morris's worker service, the macOS app
(`maestro-agents-macos`), and an agent's own CLI. It also covers what an
agent module exports. The decisions behind it are
[ADR 0024](adr/0024-the-external-contract.md). Where this document and
an ADR disagree, the ADR wins, and this document is fixed.

There is **one contract for every consumer**. Every value crossing a
seam is a Go type with one JSON form. In process it travels as a Go
value, and across a process boundary as that JSON.

Each part is marked:

- **exists**: in the library today, with the ADR that defines it.
- **planned**: decided in ADR 0024 but not built. Wire against the shape
  given here. The tracking issue is named beside it.

Contents:

1. [Running agents: what a host provides and calls](#1-running-agents-what-a-host-provides-and-calls)
2. [Run status](#2-run-status)
3. [Stores: Postgres and the wire form](#3-stores-postgres-and-the-wire-form)
4. [Requests and responses](#4-requests-and-responses)
5. [What an agent module exports](#5-what-an-agent-module-exports)
6. [The app process](#6-the-app-process)
7. [MCP tools](#7-mcp-tools)

## 1. Running agents: what a host provides and calls

### What the host provides

| Seam | Go type | Status | What it does |
|---|---|---|---|
| Store | `store.Store` | exists ([ADR 0006](adr/0006-store-interface.md)) | Every durable record. One fenced, atomic `Commit`, the control plane and the reader. Prove a host store with `store/storetest`. |
| Model client | `*agent.ModelClient`, from `agent.NewModelClient` | exists ([ADR 0014](adr/0014-model-client-and-attempt-admission.md)) | Wraps the host's `maestro-llms` client and its rate limiter. Admits and records every provider attempt. No other client compiles. |
| Policy | `agent.Policy` | exists ([ADR 0008](adr/0008-model-states.md), item 5) | Allows or denies every tool call. Holding a call for approval (TOOL-4) is **not built**: a `pending` verdict fails the run with `config_error` ([ADR 0011](adr/0011-manifests.md)). A run asks a person through a request instead (section 4). |
| Credentials | `agent.CredentialProvider` | exists ([ADR 0016](adr/0016-telemetry-logging-and-credentials.md)) | Serves a tool a credential at call time, keyed by the run context (run, principal). Never reaches agent code. |
| Telemetry | `agent.Sink`, `agent.Redactor` | exists (ADR 0016) | One event per committed record entry: IDs, names, counts and codes, never content. Free text leaves only through the redactor. |
| Logging | `*slog.Logger` | exists (ADR 0016) | One logger for the runtime, agent types and tools. |
| Tool sources | `agent.ToolSource`, in `ActivationInput.ToolSources` | exists ([ADR 0025](adr/0025-mcp-served-tools.md)), with the MCP adapter `mcptools` | Serves an agent type's served tools (an MCP server, say). Built for the run's principal; the host owns its transport, token and renewal. See section 7. |

### What the host calls

| Call | Status | Use |
|---|---|---|
| `agent.NewRegistry`, `Registry.Register` | exists | Register each agent type explicitly (MAN-3). |
| `Registry.Manifest(name)` | exists ([ADR 0011](adr/0011-manifests.md)) | The type's `Manifest` value (and whether the type exists), whose JSON form is the catalogue entry: name, version, triggers, configuration schema, default budgets, states, tools, skills, templates. Morris builds its catalogue and its "propose a run" tool from it. |
| `Registry.CheckStart(agentType, triggerKind, config, budgets)` | exists ([ADR 0024](adr/0024-the-external-contract.md), item 5) | Checks a start without a run: the trigger kind, the configuration against the type's schema, and the budgets (the type's defaults filling zero ones), returning every problem as `[]agent.StartProblem` (`check`, `reason`, and `budget` or `pointer` where they apply, `detail`). `reason` is what the start would fail with: `config_error`, or `budget_unenforceable` for a cost budget; an unregistered type has none (`Activate` refuses the run). The namespace and principal are not checked: the host assigns them and checks them itself. Use it before a confirmation box or before saving a setup. The start itself checks again, with the same code. |
| `Registry.CheckToolSources(ctx, agentType, sources)` | exists ([ADR 0025](adr/0025-mcp-served-tools.md), item 4) | The start's check of the tool sources, without a run: each source the type declares present and listing its tools, and offering every served tool the type grants. Problems are `[]agent.StartProblem` with check `tool_sources` and reason `config_error`; an unregistered type is one problem with check `agent_type` and no reason, as from `CheckStart`. Unlike `CheckStart` it makes network calls. |
| `ControlPlane.EnqueueTrigger`, `CreateRun`, `RemoveTrigger` | exists ([ADR 0005](adr/0005-storage-one-fenced-commit.md)) | Create a run, idempotently on (instance, trigger ID). |
| `ControlPlane.Claim`, `Heartbeat`, `Release` | exists ([ADR 0003](adr/0003-run-model-identities-and-activation-results.md)) | Lease the instance before activating, and keep the lease alive. `Heartbeat` also returns the active run's cancel request, if any (below). |
| `agent.NewController(store, registry, opts...)` | exists | One controller serves every instance, concurrently. |
| `Controller.Activate(ctx, ActivationInput)` | exists ([ADR 0007](adr/0007-agent-types-and-the-run-controller.md)) | Run a claimed run up to `MaxSteps` steps, or to its end. |
| `ControlPlane.RequestCancel(ctx, run, cause)`, `Reader.CancelRequest(run)` | exists ([ADR 0024](adr/0024-the-external-contract.md), item 6) | Stop a run, whoever is executing it (below). |
| `agent.Status(ctx, reader, run)`, `agent.InstanceStatus(ctx, reader, instance)`, `Reader.Runs(ctx, instance, RunQuery)` | exists ([ADR 0024](adr/0024-the-external-contract.md), item 7) | A run's or an instance's derived status, and an instance's runs paged: section 2. |

`ActivationInput` (exists) carries:

- the model client;
- the scope (the run) and the fence from the claim;
- the configuration, the namespace, the principal and the budgets,
  which are recorded when the run starts and fixed for the run;
- `MaxSteps`;
- the tool sources, by the source names the type's served tools declare
  (section 7).

The **principal** is the host's opaque name for whose authority the run
acts under. Morris passes the user who clicked Confirm.

### The run lifecycle a host drives

1. **Create the run.** `EnqueueTrigger`, then `CreateRun` (conditional
   on instance and trigger ID), then `RemoveTrigger`. A run is created
   `runnable`. The trigger is whatever starts a run: a file, a click, a
   submitted response. Schedule triggers, for continuously running
   agents (MODE-4), are planned (#33).
2. **Claim it.** Find ready runs with `Reader.Claimable`, then call
   `Claim(instance, owner, lease)`. The ownership returned gives the
   fence.
3. **Activate it.** While it runs, call `Heartbeat` at a fraction of the
   lease:
   - a heartbeat that finds ownership gone cancels the activation's
     context;
   - a heartbeat that returns a cancel request cancels it with
     `context.WithCancelCause` and the matching cause,
     `agent.CancelCause(req.Cause)`: `agent.ErrOperator`,
     `agent.ErrSuperseded` or `agent.ErrUnauthorized`.
4. **Read the result.**
   - An `Outcome` means its commit is confirmed. The activation released
     the claim in that commit.
   - An error means there is no confirmed outcome. Release the claim if
     you still hold it, reread the run record, and retry from step 2
     (ADR 0003, item 14). A store failure is never a run failure.
5. **Shut down.** On shutdown, cancel the context with no cause, or any
   cause other than the cancellation causes above. The run keeps its disposition
   (`runnable`), and the next claimant continues it.

Delivery is at least once (DEL-1). Recovery replays at most one step
(ADR 0004).

### Run statuses and reasons (exists, RUN-3)

The stored status is one of `runnable`, `waiting` (never produced
before STM-8), `succeeded`, `failed` or `cancelled`. Each carries a
`reason` (`{"code": "...", "budget": "..."}`), and the codes allowed
depend on the status:

| Status | Reason codes |
|---|---|
| `runnable` | none (an empty reason: created, not yet activated), `step_limit`, `shutdown` |
| `waiting` | the awaited input (STM-8, planned, #34); no run enters `waiting` before then |
| `succeeded` | `changed`, `no_change` |
| `failed` | `budget_exhausted`, `budget_unenforceable`, `model_error`, `tool_error`, `invalid_output`, `wrap_up_failed`, `object_conflict`, `state_error`, `config_error` |
| `cancelled` | `operator`, `superseded`, `unauthorized` |

There is no stored `running` status. Section 2 derives one.

### Stopping a run (exists, ADR 0024 item 6)

`RequestCancel(ctx, run, cause)` records a cancel request, with the cause
`operator`, `superseded` or `unauthorized`. Use `unauthorized` when the
run's principal no longer holds the authority the run started under (a
host that re-checks it at each lease); the run ends `cancelled` with
that reason, never as stopped by an operator. A host that finds the
loss while it holds the lease may also cancel the activation directly
with `agent.ErrUnauthorized`. It is unfenced and idempotent: the first
cause stands. The request is a record of its own, not part of the run
record, so it never makes the executing activation's commit conflict.
`Reader.CancelRequest(run)` reads it, and it does nothing to a terminal
run.

```go
req, recorded, err := st.RequestCancel(ctx, runID, store.CancelOperator)
// req: {"requested_at": "...", "run": "...", "cause": "operator"}
// recorded false: the run was already terminal, and nothing was recorded.
// A run that already has a request returns it, its first cause standing.
// An unknown run is store.ErrNotFound; another cause, store.ErrInvalid.

own, req, err := st.Heartbeat(ctx, fence, lease) // req is nil, or the request
if req != nil {
	cancel(agent.CancelCause(req.Cause)) // the activation's CancelCauseFunc
}
```

- **If the run is executing,** the executing host learns of the request
  from its next heartbeat and cancels the activation with the matching
  cause. The run commits `cancelled` at its next safe point.
- **If the run is ready,** the next activation commits `cancelled`
  before taking any step, so the host claims it at once (the standalone
  host does, past any retry delay). The request decides even over a
  deadline already passed.

The status shows phase `stopping` until the run is terminal.

Stopping is a request, and the run's own commit decides. If the run's
final commit lands before the request, the run keeps that outcome, and
its status never shows `stopping`.

### Artifacts a person downloads (planned, #30)

An artifact is content-addressed and write-once (`store.Artifact`, which
exists). When an agent publishes one, it appends an `artifact_published`
entry to the execution record, in the same commit:

```json
{"name": "statement-2026-q3.csv", "media_type": "text/csv",
 "digest": "<sha256 hex>", "size": 18234}
```

A run's artifacts are those entries, and `RunStatus.artifacts` lists
them. Fetch the content with `Reader.Artifact(instance, digest)`
(exists): every store answers only for a digest linked to that
instance, because each put links the digest to the instance that
committed it, even when the content was already stored. A digest not
linked to the instance answers `ErrNotFound`, exactly as one never
stored, so knowing a digest is never enough to read another instance's
content. The host decides
where downloads appear: Morris on its runs page and in chat, the app
host as links, and an optional copy in a local folder.

### Artifact links and existing file stores (exists, ADR 0024 item 9)

A file store written before artifact links existed holds artifacts no
instance is linked to. `filestore.Open` refuses it with
`filestore.ErrMigrationRequired` unless it is opened with
`filestore.WithLegacyArtifactLinks(agent.ArtifactReferences)`, which
links each stored artifact, once and atomically, to every instance
whose own records name it: the runtime's typed digest fields in its
entries and checkpoints, never an agent's own data. An artifact no
instance names fails the open with a `*filestore.UnclaimedArtifactsError`
listing the digests, with nothing written, until
`filestore.WithArtifactOwners` states its owners; stated owners only
add to the references, so the open needs
`filestore.WithLegacyArtifactLinks` with or without them. The standalone host
passes `agent.ArtifactReferences`, so a store the runtime wrote
migrates on first open. The memory store has nothing to migrate.

## 2. Run status

`agent.Status(ctx, reader, runID)` returns an `agent.RunStatus`. It is
derived from the run record, the cancel request, the ownership record,
the checkpoint and the execution record, and never stored. It is a
snapshot from several reads, not one transaction: a run that ends while
it is read may show its new phase with its progress from just before.
An unknown run is `store.ErrNotFound`.

```json
{
  "created_at": "2026-11-19T18:01:59Z",
  "updated_at": "2026-11-19T18:04:11Z",
  "last_activity": "2026-11-19T18:04:11Z",
  "cancel_requested": null,
  "state": {"name": "extract", "kind": "model"},
  "budgets": {
    "deadline": null,
    "tokens": {"used": 41200, "limit": 200000},
    "turns": {"used": 6, "limit": 40},
    "active_time": {"used": "2m10s", "limit": "15m0s"}
  },
  "run": "r-01",
  "instance": "i-01",
  "agent_type": "addepar-intake",
  "agent_version": "3",
  "phase": "running",
  "reason": {},
  "requests": [],
  "artifacts": [],
  "step": 7
}
```

Key order carries no meaning. The fields:

- `phase`, from the closed set below, and `reason`, RUN-3's reason
  (`code`, and `budget` for a budget failure) for a terminal phase, `{}`
  otherwise.
- `cancel_requested`: the run's cancel request (`run`, `cause`,
  `requested_at`), or null.
- `state`: the current state's name and kind while the run is not
  terminal; null before its start is committed and once it is terminal.
  The kind is the manifest's (`code`, `model` or `terminal`: an
  activation interrupted as it entered a terminal state leaves the run
  runnable there, for the next activation to end). Only a run
  checkpointed before kinds were recorded may have no `kind`.
- `step`: the step of the run's latest execution-record entry.
- `budgets`: null before the run's start is committed. Each budget has
  `used` and, when one is set, `limit`; `active_time`'s are Go duration
  strings; `deadline` is a time or null. For a run that is not
  terminal, `used` includes the model attempts since its last
  checkpoint, charged as recovery charges them. A token budget admitted
  on an estimate (`agent.TokenBudget.Estimated`) has `"estimated": true`,
  and its `used` may pass its `limit` by one attempt's error. Cost budgets are not built and not shown.
- `last_activity`: when the run's latest execution-record entry was
  committed, by the store's clock (`store.Entry.At`), or the run
  record's latest change if that is later.
- `requests`: the run's open requests (section 4: `id`, `kind`,
  `title`). Always `[]` until #31; a run that succeeded may then still
  have open requests: that is the between-runs wait.
- `artifacts`: the run's published artifacts (`name`, `media_type`,
  `digest`, `size`). Always `[]` until #30.

**Phase** is a closed set:

| Phase | Meaning |
|---|---|
| `queued` | `runnable`, and no live lease on the instance |
| `running` | `runnable`, and a live lease, judged by the store's clock (`store.Ownership.AsOf`, the store's reading when it returned the record), never the reader's |
| `waiting` | `waiting` for input (STM-8, planned, #34; not produced before then) |
| `stopping` | a cancel request, not yet terminal (whatever else holds) |
| `succeeded`, `failed`, `cancelled` | terminal, with RUN-3's reason |

`agent.InstanceStatus(ctx, reader, instance)` is for agents that run
continuously, such as data pipelines. It returns an
`agent.InstanceReport`:

```json
{
  "active": {"run": "r-07", "phase": "running", "...": "a RunStatus"},
  "last_succeeded": {"at": "2026-11-19T17:00:02Z", "run": "r-06", "reason": {"code": "changed"}},
  "last_failed": null,
  "queued": {"oldest": "2026-11-19T18:03:00Z", "count": 2},
  "instance": "i-01",
  "requests": []
}
```

`active` is the status of the instance's run that is not terminal, or
null. `last_succeeded` and `last_failed` are its newest succeeded and
failed runs (a cancelled run is neither), with `at` the run record's last
change, or null. `queued` counts the queued triggers and gives the
oldest's enqueue time. `requests` is the instance's open requests,
always `[]` until #31.

Both are bounded by a run, never by the instance's history. The last
succeeded and failed runs come from the reader's
`Runs(ctx, instance, store.RunQuery)`, which pages an instance's runs by
creation time and then run ID:

| `RunQuery` field | Meaning |
|---|---|
| `After` | The last run ID of the previous page, empty for the first. It must be a run of the instance (`ErrNotFound` if unknown, `ErrInvalid` if another instance's); the page continues past it whether or not it matches `Status`. |
| `Status` | Keep only runs with one of these statuses; empty keeps all. An invalid one is `ErrInvalid`. |
| `Limit` | The page size: zero asks for the store's default, negative is `ErrInvalid`. A store may cap it (the reference stores: default 100, cap 1000). |
| `Newest` | Newest first; oldest first otherwise. |

It returns the page and whether more runs follow it. A host store
answers it from an index on (instance, status, creation time, run ID).

A new phase or reason is a contract change and comes with an ADR.

## 3. Stores: Postgres and the wire form

### The Postgres store (planned, #28)

An adapter package implements `store.Store` on Postgres (STORE-4,
STORE-5). It follows `maestro-llms`' `bucket/postgres` exactly:

- `Migrations()` returns an embedded `fs.FS` of numbered files in
  golang-migrate's naming (`000001_init.up.sql` / `.down.sql`). **The
  store never runs DDL.** Apply the files with your own migration tool
  and credentials, before you deploy.
- `SchemaVersion` is the version the release needs. The constructor and
  `CheckSchema(ctx, db)` return a `*SchemaError` against an older
  schema. A newer schema is accepted.
- Migrations are expand-only. A release that raises `SchemaVersion`
  opens its notes with **"Database migration required"**.
- The schema holds only `store.Store`'s fixed records (with ADR 0024's
  additions, the cancel requests and the artifact-to-instance links),
  and the telemetry events `store.Telemetry` keeps (so the `store`
  telemetry sink works on it). Agents' data lands in those records:
  object values (JSON or raw bytes), checkpoint state, record entries
  and artifacts (manifests, configurations, transcripts), whose content
  is in Postgres unless a blob backend holds it (below), so adding an
  agent never changes the schema. The schema changes only with
  `store.Store` or `store.Telemetry`, or if the blob backend's
  bookkeeping (#28) needs a table, each by an ADR.
- **Artifact content can live elsewhere.** Pass an optional blob
  backend (your implementation of a small interface, such as Cloud
  Storage) and artifact content is kept there by digest. The digest,
  size, media type and instance links stay in Postgres. Without one,
  content is kept in Postgres. The interface and how uploads are
  coordinated with commits come with #28; it keeps content readable
  only through a committed link, and never lets a committed link point
  at missing content.
- The schema keeps purging an instance's records possible later; the
  store has no delete today (ADR 0024, "Not decided here"). Objects are
  keyed by namespace, which instances may share, so they are outside a
  per-instance purge: retiring a namespace is the host's.

It has its own Postgres schema, beside `maestro-llms`' bucket schema in
the same database.

### The store's wire form (planned, #29)

A library package serves any `store.Store` over HTTP, and provides a
client that is itself a `store.Store`. Morris runs the handler in its
web service over the Postgres store, behind its internal API's
authentication, and its worker uses the client. `storetest` runs
against the client.

- **Routes** are under `/v1/`: one per `store.Store` method, plus
  `GET /v1/runs/{id}/status`, `GET /v1/instances/{id}/status` and an
  unauthenticated `GET /v1/healthz` (`{"version", "schema_version",
  "store": "ok" | "unavailable"}`).
- **Errors keep their class.** These arrive as themselves:
  - `ErrOwnership`
  - `ErrInvalid`
  - `*ConflictError`, with the current version
  - `ErrNotClaimable`
  - `ErrInstanceBusy`
  - `ErrNotFound`: a read's answer, and from a write (`RequestCancel`
    for an unknown run) a definite one, since nothing was written

  `store.IsDefinite` reports all of these. Anything else is
  indeterminate, a dropped connection included. Reread before
  retrying.
- **Authentication is the host's.** Wrap the handler with your own. Pass
  an `Authorizer`, which sees each call's operation and the instance,
  run and namespace it concerns. Morris uses it to confine a run's token
  to that run's instance and namespace.
  - A call that names only a run (`Run`, `Entries`, `CancelRequest`,
    `RequestCancel`, the run status route) is authorized on that run's
    instance: the handler reads the run record first. A refused call
    answers exactly as a missing run does (`ErrNotFound`).
  - `Claimable` names no instance. It is the executor's store-wide
    scan: grant it only to an executing service (Morris's worker),
    never to a token scoped to a run or an instance.
  - Every other read names an instance or a namespace, artifact reads
    included (above).

- **Artifact content** travels on its own routes as raw bytes, never
  inside JSON (their exact shape, naming the instance and fence, comes
  with #29). Content over the host's maximum (64 MiB
  unless the host sets another) is refused with `ErrInvalid`.

This carries the store only. Running an agent across a process boundary
is MODE-8, which is not built.

## 4. Requests and responses (planned, #31)

A run asks for something with a **request** and gets back a
**response**. The model is generic: an approval, a choice, a question or
a review all use it, whatever the agent. Hosts render requests from
data. A custom renderer is optional (section 5).

### A request

```json
{
  "id": "r-01.7.0",
  "run": "r-01",
  "instance": "i-01",
  "kind": "review",
  "title": "Review the extracted records",
  "prompt": "Check each record against the statement and approve or correct it.",
  "blocks_version": 1,
  "body": [ /* display blocks, below */ ],
  "response_schema": { /* JSON Schema, the manifest subset (ADR 0011) */ },
  "handler": "addepar-review",
  "opened_at": "2026-11-19T18:04:11Z"
}
```

- **`id`** comes from the run's action identity: unique, and stable
  across recovery.
- **`kind`** is one of:
  - `approval`: yes or no on a stated action;
  - `choice`: one or more of the stated options;
  - `input`: a value the requester asks the responder to supply;
  - `review`: a judgement on presented content, possibly with
    corrections.
- **`response_schema`** is the authority on what may be answered. No
  renderer may offer more than it accepts. A responder never rewrites
  the requested action unless the request asks for input.
- **`handler`** is the submitted agent type that runs on the response.
  The requesting type's manifest declares which kinds it opens and
  which handlers they use.

### A response

```json
{
  "request": "r-01.7.0",
  "value": { /* accepted by response_schema */ },
  "by": "user:erik@example.com",
  "at": "2026-11-19T18:20:03Z"
}
```

`by` is a principal, which may be a person or an agent (a coding
agent's architect answers its coder). **The host decides who may answer
a request** (Morris: only the run's principal), and `by` is the
principal the host authenticated, never a value a client supplies. The
handler's run takes that principal as its own. A host that
authenticates only a session (the app host) uses its configured
principal for `by` and the handler's run, and carries the typed
operator name as attribution only.

### Lifecycle

1. **Open.** A step appends a `request_opened` entry to the execution
   record, committed with the step. The run's status lists the request.
2. **Wait.**
   - *Between runs* (built first): the run finishes, and its requests
     stay open.
   - *In the run* (STM-8, planned, #34): the run is `waiting`.
3. **Answer, once.** The host checks the value against the schema, then
   submits a run of `handler` on the request's instance, with the
   response as its payload and **the request's ID as its trigger ID**.
   `CreateRun` is conditional on (instance, trigger ID), so a second
   answer finds the first one's run and changes nothing. If the
   instance has an unfinished run, no run is made and the request stays
   open: the host retries, or asks the responder to try again. A request
   is closed once that run exists.
4. **Continue.** The handler's run acts on the response. If it needs
   another answer, for instance on a correction to review again, it
   opens a new request.

**Headless hosts** declare it when configured. They leave requests open
and visible in the status. Once STM-8 exists, a run that would wait in
such a host ends instead.

### Display blocks, version 1

A body is a list of blocks. Every host renders every block. A host that
meets a `blocks_version` it does not know shows the title and prompt,
and says the body needs a newer host.

```json
{"type": "text", "text": "Three records failed check 4."}

{"type": "fields", "fields": [
  {"label": "Fund", "value": "Keelson Partners III"},
  {"label": "Period end", "value": "2026-09-30"}]}

{"type": "table", "id": "records",
 "columns": [{"id": "investor", "label": "Investor"},
             {"id": "ending", "label": "Ending balance", "align": "right"}],
 "rows": [{"id": "rec-03", "cells": {"investor": "RMS Trust", "ending": "1,204,550.00"},
           "citations": ["c1"]}],
 "answer_per_row": true}

{"type": "list", "items": ["Unmatched account 4410", "Unmatched account 4415"]}

{"type": "code", "language": "diff", "artifact": "<sha256 hex>"}

{"type": "citation", "id": "c1",
 "source": {"artifact": "<sha256 hex>", "page": 3,
            "boxes": [[72.0, 410.5, 188.2, 422.0]]}}
{"type": "citation", "id": "c2", "source": {"document": "sharepoint:item:01ABC"}}
{"type": "citation", "id": "c3", "source": {"url": "https://example.com/report"}}
```

- A table with `answer_per_row` is answered row by row. The response
  schema then has an object property keyed by row ID. That covers a
  pipeline's batch of unmatched accounts and a batch of records to
  review alike.
- A citation's `document` is resolved by the host (Morris resolves
  SharePoint items, for example). Boxes are in PDF points, from the
  page's top-left corner.
- The exact JSON is finalised with the requests issue, which may
  refine this first version before it ships.

## 5. What an agent module exports (planned, #31)

Each agent is a Go module of its own (in `morris-agents`). It exports a
`Bundle`:

- the module's **name** and **version**;
- its **agent types** (`agent.Definition`). The host registers them, so
  registration stays explicit (MAN-3);
- its **handler types**, the submitted types that act on responses;
- a **default configuration** per type;
- optionally a **renderer**: an `http.Handler` that draws the module's
  requests better than the generic pages do. Hosts with a web UI mount
  it under `/agents/<module>/`, and it answers through the same request
  and response path.

Everything a host reads is a public package of the module, and nothing
a consumer needs is under `internal/`. `cmd/<agent>` is a thin `main`
over the standalone host. A host that wants the agent's data for a
richer view reads it through the module's public packages over
`store.Reader`. Answers still go through requests.

## 6. The app process (planned, #32)

The macOS app (or any launcher) starts one host executable, which has
agent bundles linked in:

| Aspect | Contract |
|---|---|
| Arguments | `-workspace DIR -addr HOST:PORT`. Port `0` picks a free port. Loopback only. |
| Session token | `MAESTRO_AGENTS_SESSION_TOKEN`, at least 16 characters (shorter fails the start), or 32 random bytes if unset. Valid for the life of the process. A host that drew its own token writes the sign-in link to stderr once, after the ready line. |
| Ready | Exactly one JSON line on stdout once the listener is bound and, in a configured workspace, the store is open. On first run (no configuration yet) it is written anyway, and the `url` serves setup: `{"event":"ready","url":"http://127.0.0.1:PORT/","version":"…"}`. The `url` carries the IP actually bound (`127.0.0.1` or `[::1]`, never a name, even for `-addr localhost`), with the assigned port. Nothing else goes to stdout. |
| Failure to start | A failed bind, or a configured workspace whose store will not open: exit non-zero, with the reason on stderr and no ready line. |
| Logs | stderr. |
| Health | `GET /healthz`, no session needed: `{"version", "store": "ok" \| "unavailable" \| "unconfigured"}` (`unconfigured` until first-run setup completes). |
| Browser sign-in | Open `<url>auth/session?token=<token>`: a launcher that set the token builds it from the ready line's `url`, percent-encoding the token as a query value, and a host that drew its own writes it to stderr. It sets a per-port session cookie and redirects to `/`. The link stays valid, so it can reopen the UI. |
| Control calls | `/api/...` with `Authorization: Bearer <token>`. |
| Stop | SIGTERM (or SIGINT). The host stops at the next step boundary. Wait at least 5 seconds before SIGKILL. |
| First run | Setup in the browser: model provider, model and key. The key is kept in the user's configuration folder, never in the workspace. |

The host runs every linked agent type over one store. Each type has its
own namespace, trigger, inbox and configuration under the workspace. It
serves generic request and status pages, plus each bundle's renderer.
Its configuration format and workspace layout come with its ADR.

## 7. MCP tools

Issue [#23](https://github.com/SnapdragonPartners/maestro-agents/issues/23)
(TOOL-6), [ADR 0025](adr/0025-mcp-served-tools.md). The runtime's seam
and the MCP adapter (`mcptools`, a nested module of its own) exist.

- **Granting.** An agent type grants a served tool in a model state's
  `Tools` with `agent.ServedTool[D](source, name, effect, contract)`,
  beside its own tools. The type, not the server, classifies the
  effect and contract. A served tool cannot be a wrap-up exit or
  declare `atomic_local`. The manifest lists it with its `source` and no
  schema.
- **Sources.** The host passes `ActivationInput.ToolSources`, an
  `agent.ToolSource` per source name, built for the run's principal.
  The host owns the transport, the token and its renewal, reconnecting
  and closing; the runtime never closes a source.
- **Start.** The start lists each source and requires every granted
  served tool: a missing source, a failed listing or a missing tool
  fails the run with `config_error` (`Registry.CheckToolSources` runs
  the same check without a run). The granted tools' definitions are
  recorded with the run's start (`RunStarted.Served`), and the model is
  offered those for the whole run. A later activation whose sources
  lack a tool or offer a different definition fails the run with
  `tool_error`.
- **Calls.** A served call takes the in-process path: policy,
  permission, attempt and result records, wrap-up, telemetry. The
  source receives an `agent.ServedCall`: the call, its run context (the
  server re-checks the principal), the activation's fence (instance,
  owner, epoch: a host that keeps the store refuses a call whose epoch
  is not the instance's current one), its action identity and, for a
  write, its idempotency key.
- **Action identity and idempotency key.** A call's action identity is
  (scope, step, call): the run's scope (`{"kind":"run","id":<run ID>}`);
  the step, 1-based and per run, increasing across activations and
  claims (it is checkpointed); and the call, the 0-based index of the
  call in the recorded model turn. It is unique within the run and the
  same on every issue of the call, whichever claim issues it. A write's
  idempotency key is `ActionID.IdempotencyKey()`: the lowercase hex
  SHA-256 of five fields in order, each written as its length (8 bytes,
  big-endian) followed by its bytes: the domain tag
  `maestro-agents/action/v1`, the scope kind, the scope ID (the run
  ID), the step and the call. The step's and the call's bytes are their
  values as 8 bytes, big-endian, so each is written as the length 8 and
  then those 8 bytes, like every other field. The tool's
  name and arguments are not in it: the action identity names exactly
  one recorded call, whose tool and arguments are fixed before it is
  first sent. The fence is not in it either: a later claim's reissue
  has the same key and a newer epoch; the fence check is on the epoch
  alone, deduplication on the key (ADR 0025, item 5).
- **No wire result for approval.** A write that waits on a person's
  approval is decided before the call is sent (the policy's `pending`
  decision, TOOL-4 and STM-8, not yet built): an approval is a host or
  UI action, never a tool the model calls, so no call goes out until it
  is decided and the wire has no "awaiting approval" result. This is
  deliberate.
- **Failures.** A result with `IsError` is model-visible. A source's
  error is classified by what it wraps and recorded as
  `ResultEntry.Failure` (and the result event's `error_kind`):
  `unreachable` (`agent.ErrSourceUnreachable`: never sent),
  `source_error` (`agent.ErrSourceError`: the server answered with an
  error) or `unknown`. A read's failure, and an unreachable write, are
  model-visible errors the model may retry within its budgets; a write
  the server failed fails the run with `tool_error`, and one sent and
  not answered is an unknown outcome (TOOL-12) and fails it too.
- **Tests.** `agent/agenttest.Source` stands in for a server: scripted
  tools, failure injection and a log of the calls it received.
- **The MCP adapter.** `github.com/SnapdragonPartners/maestro-agents/mcptools`
  is a nested module (tags `mcptools/vX.Y.Z`), so only a host that
  imports it takes on the MCP SDK. `mcptools.New(dial, opts)` is an
  `agent.ToolSource` over one MCP server reached through
  `maestro-llms/mcp`'s `mcpclient`: `dial` returns a fresh
  `mcpclient.Transport` (Streamable HTTP with the host's `http.Client`,
  which carries and renews the token, or a local command), called on
  first use and after a failure. `opts.Filter` keeps only the named
  tools (a listing fails if any kept tool cannot be offered to a
  model). The host closes the source (`Source.Close`).
- **What a server receives.** Each `tools/call` carries in its `_meta`
  the host's claims about the call, JSON values under these keys
  (`mcptools.Meta*`), every one on every call, reads included, except
  the idempotency key:

  | Key | Value |
  |---|---|
  | `maestro-agents/v` | the key set's version, the integer `1` |
  | `maestro-agents/agent_instance` | the agent instance, a string; unchanged when the run resumes on another worker |
  | `maestro-agents/agent_type` | the agent type, a string |
  | `maestro-agents/principal` | the run's principal, a string; empty is a run under the host's own authority, by the host's choice, never a dropped field |
  | `maestro-agents/owner` | the owner of the activation's claim, a string |
  | `maestro-agents/epoch` | the claim's owner epoch, a JSON integer: unsigned 64-bit, 1 or more, one per claim of the instance, never decreasing (a decoder into `float64`, as the go-sdk's `map[string]any` is, is exact below 2^53, which an epoch never reaches in practice) |
  | `maestro-agents/action` | the action identity, `{"scope":{"kind":"run","id":<run ID>},"step":N,"call":N}` (above); the run is its scope |
  | `maestro-agents/idempotency_key` | a write's idempotency key (above); only for a write |

  A call missing its run context or fence is never sent. The server
  re-checks the principal against the request's token, and a server
  that keeps the store refuses a call whose epoch is not the
  instance's current one; the check is on the epoch alone, so a newer
  claim's reissue (a new owner and epoch, the same key) is accepted and
  deduplicated by its key. A change a server must know of increments
  `maestro-agents/v`. A server built on `maestro-llms`' `mcpserver`
  reads the keys with `mcpserver.MetaFromContext` (`mcp/v0.3.0`), which
  gives numbers as `float64`. The keys are unverified input: the
  token authorizes, and the claims only narrow what it may do. The adapter
  maps `mcpclient`'s classes one to one: not sent is `unreachable`, the
  server's error (a JSON-RPC error or a malformed result) is
  `source_error`, an unknown outcome is `unknown`; a call to a tool the
  server no longer lists is `source_error`. After a failure other than
  the server's answer, the next use opens a new session; calls still in
  flight on the old one finish first.