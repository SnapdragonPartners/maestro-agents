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
7. [Planned: MCP tools](#7-planned-mcp-tools)

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
| MCP tool source | in `ActivationInput` | planned (#23) | See section 7. |

### What the host calls

| Call | Status | Use |
|---|---|---|
| `agent.NewRegistry`, `Registry.Register` | exists | Register each agent type explicitly (MAN-3). |
| `Registry.Manifest(name)` | exists ([ADR 0011](adr/0011-manifests.md)) | The type's `Manifest` value (and whether the type exists), whose JSON form is the catalogue entry: name, version, triggers, configuration schema, default budgets, states, tools, skills, templates. Morris builds its catalogue and its "propose a run" tool from it. |
| `Registry.CheckStart(agentType, triggerKind, config, budgets)` | planned (#35) | Checks a start without a run, returning every failure as data. Use it before a confirmation box or before saving a setup. The start itself checks again. |
| `ControlPlane.EnqueueTrigger`, `CreateRun`, `RemoveTrigger` | exists ([ADR 0005](adr/0005-storage-one-fenced-commit.md)) | Create a run, idempotently on (instance, trigger ID). |
| `ControlPlane.Claim`, `Heartbeat`, `Release` | exists ([ADR 0003](adr/0003-run-model-identities-and-activation-results.md)) | Lease the instance before activating, and keep the lease alive. |
| `agent.NewController(store, registry, opts...)` | exists | One controller serves every instance, concurrently. |
| `Controller.Activate(ctx, ActivationInput)` | exists ([ADR 0007](adr/0007-agent-types-and-the-run-controller.md)) | Run a claimed run up to `MaxSteps` steps, or to its end. |
| `ControlPlane.RequestCancel(ctx, run, cause)` | planned (#26) | Stop a run, whoever is executing it (below). |
| `agent.Status`, `agent.InstanceStatus` | planned (#27) | Section 2. |

`ActivationInput` (exists) carries:

- the model client;
- the scope (the run) and the fence from the claim;
- the configuration, the namespace, the principal and the budgets,
  which are recorded when the run starts and fixed for the run;
- `MaxSteps`.

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
   - (planned) a heartbeat that returns a cancel request cancels it with
     `context.WithCancelCause` and `agent.ErrOperator` or
     `agent.ErrSuperseded`.
4. **Read the result.**
   - An `Outcome` means its commit is confirmed. The activation released
     the claim in that commit.
   - An error means there is no confirmed outcome. Release the claim if
     you still hold it, reread the run record, and retry from step 2
     (ADR 0003, item 14). A store failure is never a run failure.
5. **Shut down.** On shutdown, cancel the context with no cause, or any
   cause other than the two above. The run keeps its disposition
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
| `cancelled` | `operator`, `superseded` |

There is no stored `running` status. Section 2 derives one.

### Stopping a run (planned, #26)

`RequestCancel(ctx, run, cause)` records a cancel request, with the cause
`operator` or `superseded`. It is unfenced and idempotent: the first
cause stands. The request is a record of its own, not part of the run
record, so it never makes the executing activation's commit conflict.
`Reader.CancelRequest(run)` reads it, and it does nothing to a terminal
run.

- **If the run is executing,** the executing host learns of the request
  from its next heartbeat and cancels the activation with the matching
  cause. The run commits `cancelled` at its next safe point.
- **If the run is ready,** the next activation commits `cancelled`
  before taking any step, so the host claims it at once.

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
(planned, #29): every store answers only for a digest linked to that
instance, because each put links the digest to the instance that
committed it. Knowing a digest is therefore never enough to read another
instance's content. Today's `Reader.Artifact(digest)` takes no instance. The host decides
where downloads appear: Morris on its runs page and in chat, the app
host as links, and an optional copy in a local folder.

## 2. Run status (planned, #27)

`agent.Status(ctx, reader, runID)` returns a `RunStatus`. It is derived
from the run record, the ownership record and the execution record, and
never stored.

```json
{
  "run": "r-01",
  "instance": "i-01",
  "agent_type": "addepar-intake",
  "agent_version": "3",
  "phase": "running",
  "reason": {},
  "cancel_requested": null,
  "state": {"name": "extract", "kind": "model"},
  "step": 7,
  "budgets": {
    "tokens": {"used": 41200, "limit": 200000},
    "turns": {"used": 6, "limit": 40},
    "active_time": {"used": "2m10s", "limit": "15m"},
    "deadline": null
  },
  "last_activity": "2026-11-19T18:04:11Z",
  "requests": [{"id": "r-01.7.0", "kind": "review", "title": "Review 12 records"}],
  "artifacts": [],
  "created_at": "2026-11-19T18:01:59Z",
  "updated_at": "2026-11-19T18:04:11Z"
}
```

**Phase** is a closed set:

| Phase | Meaning |
|---|---|
| `queued` | `runnable`, and no live lease on the instance |
| `running` | `runnable`, and a live lease |
| `waiting` | `waiting` for input (STM-8, planned, #34; not produced before then) |
| `stopping` | a cancel request, not yet terminal |
| `succeeded`, `failed`, `cancelled` | terminal, with RUN-3's reason |

`requests` lists the run's open requests (section 4). A run that
succeeded may still have open requests: that is the between-runs wait.

`agent.InstanceStatus(ctx, reader, instance)` is for agents that run
continuously, such as data pipelines. It returns:

- the active run's status;
- the last succeeded and last failed runs (ID, time, reason);
- the instance's open requests;
- the queued triggers (count, oldest).

It finds past runs with a new reader call, `Runs(instance, after,
limit)`: an instance's runs ordered by creation time and then run ID,
paged. `after` is the last run ID of the previous page, empty for the
first.

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
  additions, the cancel requests and the artifact-to-instance links).
  Agents keep their
  data as JSON payloads, so adding an agent never changes the schema.

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
  - `ErrNotFound`, a read's answer, which is not a definite error
    today and stays so

  Anything else is indeterminate (`store.IsDefinite` is false), a
  dropped connection included. Reread before retrying.
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
agent's architect answers its coder).

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
| Session token | `MAESTRO_AGENTS_SESSION_TOKEN`, or 32 random bytes if unset. Valid for the life of the process. A host that drew its own token writes the sign-in link to stderr once, after the ready line. |
| Ready | Exactly one JSON line on stdout once the listener is bound and the store is open: `{"event":"ready","url":"http://127.0.0.1:PORT/","version":"…"}`. The `url` is the address actually bound, with the assigned port (`[::1]` or `localhost` if `-addr` named it). Nothing else goes to stdout. |
| Failure to start | Exit non-zero, with the reason on stderr and no ready line. |
| Logs | stderr. |
| Health | `GET /healthz`, no session needed: `{"version", "store"}`. |
| Browser sign-in | Open `<url>auth/session?token=<token>`: a launcher that set the token builds it from the ready line's `url`, and a host that drew its own writes it to stderr. It sets a per-port session cookie and redirects to `/`. The link stays valid, so it can reopen the UI. |
| Control calls | `/api/...` with `Authorization: Bearer <token>`. |
| Stop | SIGTERM (or SIGINT). The host stops at the next step boundary. Wait at least 5 seconds before SIGKILL. |
| First run | Setup in the browser: model provider, model and key. The key is kept in the user's configuration folder, never in the workspace. |

The host runs every linked agent type over one store. Each type has its
own namespace, trigger, inbox and configuration under the workspace. It
serves generic request and status pages, plus each bundle's renderer.
Its configuration format and workspace layout come with its ADR.

## 7. Planned: MCP tools

Issue [#23](https://github.com/SnapdragonPartners/maestro-agents/issues/23)
(TOOL-6):

- The host passes a per-run tool source in `ActivationInput`. The host
  owns its transport and its token, and renews the token.
- A manifest grants MCP-served tools by name, exactly as it grants
  in-process tools. A start whose granted tool the server does not offer
  fails with `config_error`.
- Every call carries the run context, and the server re-checks the
  principal.
- An unreachable server or a dropped session is a classified tool
  error, never a crash.
- The replay harness stands in for the server in tests.
