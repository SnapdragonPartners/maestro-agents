# 0024. The external contract

- **Status:** Proposed
- **Date:** 2026-11-19

## Context

Three kinds of consumer will run agents built on this library:

- **Morris's worker service** (Morris ADRs 0068 and 0069). It holds no
  database credentials and reaches Morris's data only through Morris's
  internal API. Morris draws every interface itself: a confirmation box
  to start a run, the run's progress and questions posted into the chat
  that started it, and a runs page with Stop.
- **A native macOS app** (`maestro-agents-macos`). It launches one Go
  host executable, with agent modules compiled in, and opens its web UI.
- **Each agent's own CLI**, on the standalone host (ADR 0017).

The agents themselves are leaving this repository: each becomes its own
Go module in `SnapdragonPartners/morris-agents` (ADR 0023). They are not
alike. The first is the capital-statement intake agent (ADRs 0015 and
0018 to 0022), which reviews extracted records against PDF evidence.
Next in line are a coding agent ported from Maestro, whose architect
answers its coder's questions; continuously running data pipelines
between family-office systems (Sage and Addepar), which meet unmatched
accounts that a person must map; and a research agent that searches a
SharePoint server and cites what it found.

Morris ADR 0069 names this library's seams by what they do and waits
for "maestro-agents settles its external contract" to wire them. This
ADR settles it, as **one interface for every consumer**: a consumer is
never given a variant of its own. That way Morris, the app and the CLI
behave alike, and an agent written once runs under all three. The
contract has three layers:

1. **Host and runtime**: what a host implements and calls.
2. **Agent module and host**: what an agent module exports, and how a
   run asks for something and gets an answer.
3. **The app process**: how the macOS app starts, reaches and stops the
   host executable.

Most of layer 1 already exists (ADRs 0003 to 0014, 0016, 0017 and
0021). This ADR states it as a contract, names the gaps, and decides
how each gap is filled. Code comes in later PRs, one issue for each
gap, each with an ADR where it decides something new. The
consumer-facing reference is [docs/contract.md](../contract.md).

## Decision

### Principles

1. **One contract, JSON at every seam.** Every value that crosses a
   seam is a Go type with one JSON form (P-6): run records, manifests,
   statuses, requests, responses and events. The same values travel
   over Go calls and channels in process (the standalone host, the app
   host) and over HTTP (Morris's worker). The Go types are the
   contract, and the JSON form is how they cross a process boundary.
   Neither has a consumer-specific variant.

2. **The contract is versioned with the module.** Before v1, a minor
   release may change it, and its release notes say so (ADR 0023,
   item 4). The wire form (item 9) carries its own version in its
   paths, and the Postgres schema (item 8) carries its own as well.

3. **The library still owns no infrastructure** (P-2). Every piece of
   this contract that serves something (the wire handler, the app
   host's HTTP server) is a library package a host mounts, behind the
   host's own authentication and listener.

### Layer 1: host and runtime

4. **The seams that exist are the contract as they stand**:
   - `store.Store` (ADR 0006): one fenced, atomic `Commit`, the control
     plane (`Claim`, `Heartbeat`, `Release`, `EnqueueTrigger`,
     `RemoveTrigger`, `CreateRun`) and the reader. A host's store proves
     itself with `store/storetest`, TEST-5 and TEST-6 included.
   - `agent.Registry`, manifests (ADR 0011) and `Controller.Activate`
     with `ActivationInput` (ADRs 0003 and 0007): the model client
     built only by `agent.NewModelClient` over the host's `maestro-llms`
     client and limiter (ADR 0014), budgets (ADR 0009), the run's one
     namespace (ADR 0012), and the principal, fixed at the run's start.
   - The host hooks (ADR 0016): `Policy`, `CredentialProvider`, keyed by
     the run context, which serves a credential at call time and never
     to agent code, `Sink`, `Redactor` and the `*slog.Logger`.
   - Submitted runs (ADR 0021, item 1), the way a run is started by an
     action rather than by an input.

   A Morris worker therefore implements `store.Store` as a client of
   Morris's internal API (item 9 gives it one ready-made), builds its
   model client from its own model seam, supplies a credential provider
   that asks Morris for a credential scoped to the run, its principal
   and the connection, and turns telemetry events into its audit and
   its chat entries.

5. **Starting a run can be checked without a run.**
   `Registry.CheckStart(agentType, triggerKind, config, budgets)` runs the
   same checks an activation runs at the start (trigger kind, the
   configuration against the type's schema, the budgets against its
   limits; ADR 0011) and returns every failure as data. Morris checks a
   proposed run with it before drawing the confirmation box, and the
   app's setup checks an agent's configuration with it before saving.
   A run that passed is still checked when it starts: the check is
   advice, and the start is the authority.

6. **A run can be stopped from outside, whatever is executing it.**
   - `ControlPlane.RequestCancel(ctx, run, cause)` records a cancel
     request, unfenced and idempotent, with the cause `operator` or
     `superseded`. **The request is a record of its own, keyed by the
     run, not a field of the run record.** The run record's version and
     its single writer (the owning activation, ADR 0006 item 5) are
     unchanged, so a request never makes an activation's commit
     conflict. The first request's cause stands, and a later one
     returns it unchanged. `Reader.CancelRequest(run)` reads it.
   - `Heartbeat` returns the request along with the ownership, so the
     executing host cancels the activation's context with the matching
     cause (`agent.ErrOperator` or `agent.ErrSuperseded`). The activation
     then commits `cancelled` with that reason at its next safe point
     (ADR 0003, item 17; RUN-2).
   - An activation that finds a cancel request when it starts commits
     `cancelled` before it takes any step, so a ready run is stopped by
     whoever claims it next. The standalone host claims such a run at
     once.
   - **Stopping is a request, and the run's own commit decides.** A
     run already terminal ignores the request. A request that races the
     activation's final commit loses if that commit lands first: the
     run keeps the outcome it committed, and its status shows that
     outcome, never `stopping`. A request recorded first is seen at the
     next heartbeat or the next start, whichever comes first. This
     works the same in process and across the wire, so Morris's Stop
     button and the app's need nothing of their own.

7. **A run's status is derived, never stored twice.**
   - `agent.Status(ctx, reader, runID)` computes a `RunStatus` from the
     run record, the ownership record and the execution record. Its
     **phase** is one of a closed set: `queued` (runnable, no live
     lease), `running` (runnable, a live lease), `waiting` (STM-8, once
     built), `stopping` (a cancel request, not yet terminal),
     `succeeded`, `failed` or `cancelled`, the last three with RUN-3's
     reason. Its **progress** is the current state and its kind, the
     step, the budgets consumed against their limits, the last
     activity's time, the run's open requests (item 11) and its
     published artifacts (item 10).
   - `agent.InstanceStatus` adds what a continuously running agent
     needs: the active run, the last succeeded and last failed runs,
     the open requests and the queued triggers. To find past runs, the
     reader gains `Runs(ctx, instance, after, limit)`, an instance's
     runs ordered by creation time and then run ID, paged: `after` is
     the last run ID of the previous page, empty for the first. Checkpoints cannot stand in for it:
     a run refused at its start ends without one.
   - Both are JSON values a host serves as it likes: Morris in its chat
     entries and runs page, the app host and the wire handler at
     `GET /v1/runs/{id}/status` and `GET /v1/instances/{id}/status`.
   - Phases and reasons are part of the contract: a new one is a
     contract change and gets an ADR.

8. **This library builds the Postgres store** (STORE-4, STORE-5), in an
   adapter package, following `maestro-llms`' `bucket/postgres` (its
   ADR 0025) exactly:
   - Its migrations are numbered SQL files in golang-migrate's naming,
     embedded and returned as an `fs.FS`. The store never runs DDL: the
     host applies the files with its own tool and credentials.
   - `SchemaVersion` is a constant, which a test keeps equal to the
     highest file number. The constructor and `CheckSchema` refuse a
     database behind it, and accept a newer one.
   - **Migrations are expand-only.** A migration that has to drop or
     rename is a breaking release. Any release that raises
     `SchemaVersion` opens its notes with "Database migration required".
   - **The schema is kept as static as possible.** It holds only the
     records `store.Store` fixes: runs, ownership, triggers,
     checkpoints, the execution record, artifacts and their instance
     links, cancel requests and objects. Every
     agent's data is a JSON payload in an object or a record entry, so
     a new agent, or a new version of one, never changes the schema.
     The schema changes only when `store.Store` does, and that is an ADR
     each time.

9. **The store has a wire form, built here.**
   - A library package serves any `store.Store` as an HTTP handler
     under `/v1/`, and provides a client that is itself a `store.Store`
     over that handler. `storetest` runs against the client, so the
     wire form proves the same contract as the store behind it.
   - Every error keeps its class across the wire. The definite errors
     (`ErrOwnership`, `ErrInvalid`, `*ConflictError` with its current
     version, `ErrNotClaimable`, `ErrInstanceBusy`) arrive as
     themselves, and so does `ErrNotFound`, which stays what it is
     today: a read's answer, not a definite error (`store.IsDefinite`
     is unchanged). Anything the client cannot classify, a dropped
     connection included, is indeterminate, and the host rereads
     before retrying (ADR 0003, item 14).
   - The handler authenticates nothing itself. The host wraps it with
     its own authentication, and passes an `Authorizer` that sees each
     call's operation and the instance, run and namespace it concerns,
     so Morris can confine a run's token to that run's instance and
     namespace. **A call that names only a run is authorized on that
     run's instance:** for `Run`, `Entries`, `CancelRequest`,
     `RequestCancel` and the run status route, the handler reads the
     run record first and passes its instance to the `Authorizer`, and
     a refused call answers exactly as a missing run does
     (`ErrNotFound`), so a token learns nothing of runs outside its
     scope. **`Claimable` names no instance**: it is the executor's
     store-wide scan, and the `Authorizer` sees it as such, with no
     instance, to grant only to an executing service's identity (Morris's
     worker), never to a token scoped to a run or an instance. `storetest`
     covers both through the client.
   - **Every other read names an instance or a namespace, artifacts
     included.** Artifacts stay content-addressed and stored once, but
     each `PutArtifact` links its digest to the committing instance,
     the link written even when the content is already stored, and
     `Reader.Artifact(ctx, instance, digest)` answers only for a digest
     linked to that instance (`ErrNotFound` otherwise). Every store
     enforces this, not only the wire handler, so knowing a digest
     never reaches another instance's content. The runtime's own reads
     (the recorded configuration, a model state's stored content) name
     their run's instance.
   - The same handler serves the status routes (item 7) and an
     unauthenticated `GET /v1/healthz`, which returns the module
     version, the schema version and whether the store answers.
   - This is not MODE-8. The wire protocol of MODE-8 runs an agent
     across a process boundary; this one only carries the store.

10. **A run's artifacts are linked to the run.** An artifact stays
    content-addressed and write-once (ADR 0005). Publishing one for
    people to download appends an `artifact_published` execution-record
    entry (name, media type, digest, size), committed with it. A run's
    artifacts are those entries, so the run record stays unchanged.
    The host reads each one with its run's instance (item 9) and serves
    it from its own store: Morris on its runs
    page and in chat, the app host as downloads. An agent's export is a
    published artifact, and writing a copy into a local folder is a
    host's choice, not the agent's (a change to ADR 0021, item 11, made
    when the intake agent adopts it).

### Layer 2: agent module and host

11. **A run asks for something with a request, and gets a response.**
    The model is generic. No agent defines it, the intake agent
    included. It follows Maestro's precedent: v1's typed request and
    response pairs, and v2's ADRs 0030 and 0032.
    - **A request** is a JSON value with:
      - an ID, built from the run's action identity, so it is unique
        and stable across recovery;
      - a kind, one of `approval`, `choice`, `input` or `review`;
      - a title, a prompt, and a body of display blocks (item 12);
      - a response schema, in the JSON Schema subset manifests already
        use (ADR 0011);
      - the submitted agent type that handles the response.
    - **A response** has the request's ID, a value the schema accepts,
      who answered (a principal, which may be a person or an agent) and
      when.
    - **The requester declares what may be answered**, through the
      response schema, and no renderer may widen it. As in Maestro's
      ADR 0030: "A UI free to compose scopes would become an authority
      on what may be approved." A responder never rewrites the requested
      action unless the request asks for input.
    - **Asking is an action, and the request is durable.** It is
      appended to the execution record as a `request_opened` entry,
      committed with the step that asks, and the run's status lists it
      while it is open.
    - **A request is answered once.** The response becomes a submitted
      run of the handler type, on the requesting run's instance, whose
      trigger ID is the request's ID. `CreateRun` is conditional on
      (instance, trigger ID), so a second answer finds the first one's
      run and changes nothing. (`CreateRun` returns an existing run for
      the trigger ID even after the trigger is removed.) A submission
      therefore carries the caller's trigger identity (amendment to ADR
      0021, item 2, below). If the instance has an unfinished run
      (`ErrInstanceBusy`, or `Host.Submit`'s `ErrBusy`), no run is
      made and the request stays open: the host retries, or tells the
      responder to try again. A request is open until that run exists.
      A handler that needs another answer, such as a correction that
      must be reviewed again, opens a new request.
    - **There are two ways to wait.** *Between runs*, built now: the
      run ends with its requests open, and the response starts the
      handler's run. This is Morris ADR 0069 decision 8's form. *In the
      run*, with STM-8 later: the run waits, and the response is
      delivered as its awaited input with the same identity. Requests
      and responses keep the same shape in both.
    - **A host with nobody to answer says so up front.** A host
      declares itself headless when it is configured, never by
      observing that nobody answered (Maestro ADR 0030). Its requests
      stay open and visible in the status. Once STM-8 exists, a run that
      would wait in such a host ends instead, with its requests kept.
    - **A manifest declares the kinds of request its type may open and
      their handler types.** The runtime refuses any other.

12. **Requests are drawn from display blocks, generically.**
    - A body is a list of blocks from a closed, versioned vocabulary
      (`blocks` version 1):
      - `text`;
      - `fields` (labelled values);
      - `table` (columns and rows, a row optionally answered on its
        own, through the response schema's per-row property);
      - `list`;
      - `code` (a language, and text or an artifact, for instance a
        diff);
      - `citation`, which points at an artifact page with optional
        boxes, at a document the host resolves (such as a SharePoint
        item, through Morris), or at a URL.
    - Every host renders every block, and that is all an answer needs:
      Morris as its confirmation boxes and chat tables, the app host as
      generic pages, a CLI as text.
    - A new block, or a new field on one, raises the vocabulary's
      version. A host that meets a version it does not know shows the
      request's title and prompt, and says that the body needs a newer
      host.

13. **A custom renderer is an enhancement, never a requirement.** A
    module may ship an `http.Handler` that draws its own requests better
    (the intake agent's evidence pages), which the app host and the
    standalone serve mount under `/agents/<module>/`. Morris may design
    a richer view for one type. Each of them answers the same requests
    with the same responses through the same path, so the generic
    rendering always works as well.

14. **An agent module exports a bundle.** A new library package defines
    `Bundle`: the module's name and version; its agent types
    (`agent.Definition`, registered by the host, which keeps MAN-3); its
    handler types; its default configuration per type; and optionally
    its renderer. Agent types, request bodies and anything else a host
    reads are public packages of the module. Nothing a consumer needs is
    under `internal/`, and `cmd/<agent>` is a thin `main` over the
    standalone host. A module exports no review API of its own beyond
    requests: a host that needs the agent's data for a richer view reads
    it through the module's public packages over `store.Reader`.

### Layer 3: the app process

15. **The host executable's process contract** is fixed so the macOS app
    (or any other launcher) needs nothing agent-specific:
    - It is started with `-workspace DIR` and `-addr HOST:PORT`, port 0
      allowed. It listens on loopback only.
    - The session token comes from `MAESTRO_AGENTS_SESSION_TOKEN`, or 32
      random bytes when that is unset. It stays valid for the life of
      the process, as in ADR 0022, item 6. A host that drew its own
      token writes the sign-in link to stderr once, after the ready
      line, so a person running it directly can still sign in; a
      launcher that set the token builds the link itself.
    - **Ready is one JSON line on stdout**, written only after the
      listener is bound and the store is open:
      `{"event":"ready","url":"http://127.0.0.1:PORT/","version":"…"}`.
      Nothing else is written to stdout, and logs go to stderr. A
      failure to bind or to open exits non-zero with the reason on
      stderr, and writes no ready line.
    - `GET /healthz` answers without a session (version, and whether
      the store answers).
    - Browser sign-in goes through `/auth/session?token=`, which sets a
      per-port session cookie. A control app calls `/api/` with
      `Authorization: Bearer <token>`.
    - SIGTERM or SIGINT shuts down at the next step boundary. A launcher
      waits at least 5 seconds before it kills.
    - First-run setup happens in the browser. The model credential is
      kept in the user's configuration folder, never in the workspace,
      as in ADR 0022, item 5. Keychain storage is later work.

    This avoids three weaknesses of `maestro-macos`'s contract with
    `maestro`: a port read from a file that an environment variable
    silently overrides, readiness judged by any answer on the port (an
    old server answers too), and a one-time token.

16. **The app host runs several agent modules in one process.** The
    standalone host grows to run several agent types from bundles over
    one store, each type with its own namespace, trigger, inbox and
    configuration under the workspace. Its HTTP server is a library
    package: the session and request checks of ADR 0022, items 6 and 8,
    the setup, the generic request pages and status pages, and the
    bundles' renderers. The app's executable is a `main` in
    `maestro-agents-macos` that links its bundles. That amends ADR 0017
    when built, and gets its own ADR.

### What is planned, stated so consumers wire against it

17. **MCP-served tools** (TOOL-6, issue #23): the host passes a per-run
    tool source in `ActivationInput`, whose transport and token it
    renews. A manifest grants those tools by name, exactly as it grants
    in-process ones, and a start whose granted tool the server does not
    offer fails with `config_error`. Calls carry the run context, and a
    failure is a classified tool error. The replay harness stands in
    for the server. The issue's own ADR decides the details.

18. **Gaps, each its own issue:** check before start (item 5, #35);
    cancel requests (item 6, #26); status (item 7, #27); the Postgres
    store (item 8, #28); the store's wire form and instance-linked
    artifact reads (item 9, #29); published artifacts (item 10, #30);
    requests, blocks and bundles (items 11 to 14, #31); the app host and
    process contract (items 15 and 16, #32); schedule triggers (MODE-4,
    for continuously running agents, #33); and the in-run wait (STM-8,
    #34). The policy's third verdict, holding a call for approval
    (TOOL-4), is not built either: today a `pending` verdict fails the
    run with `config_error` (ADR 0011), and a host's policy allows or
    denies. Until
    each lands, the contract says what it will be, and
    [docs/contract.md](../contract.md) marks it **planned**.

### Amendments

Each takes effect with the issue that builds it.

- **ADR 0003, item 7:** a cancel request (item 6) is a fact a host reads
  beside the run's status, from a record of its own. A ready run with a
  cancel request is claimed only to be cancelled.
- **ADR 0005, items 4 and 5, and ADR 0006:** the store's records gain
  the cancel request (one per run, first cause stands) and the
  artifact-to-instance link. The control plane gains `RequestCancel`,
  and `Heartbeat` returns the active run's cancel request. The reader
  gains `CancelRequest(run)`, `Runs(instance, after, limit)` (`after` a
  run ID, item 7) and a
  lookup of a run by (instance, trigger ID), which item 11's open
  requests need. `Reader.Artifact` takes the instance and answers only
  for a digest linked to it (item 9). ADR 0006 item 5's single writer
  of the run record is unchanged.
- **ADR 0017, item 18:** a heartbeat that returns a cancel request
  cancels the activation with the request's cause.
- **ADR 0021, item 2:** a submission may carry a caller-chosen trigger
  identity (`Submission`, `Host.Submit`). A response uses its request's
  ID, so a repeat finds the existing run rather than making another.
  Without one, the host still draws `submitted:<hex>` at random, never
  deduplicated, as today.
- **ADR 0021, item 11:** an export is a published artifact (item 10),
  and the folder copy is the host's.
- **ADR 0022, item 6:** the session token's variable becomes
  `MAESTRO_AGENTS_SESSION_TOKEN`, and the sign-in link moves off
  stdout, which carries the ready line alone (item 15): a launcher
  builds `<url>auth/session?token=<token>` from the ready line and the
  token it set, and a host that drew its own token writes the link to
  stderr. **ADR 0022, item
  8:** `GET /healthz` (and the wire handler's `GET /v1/healthz`, item 9)
  answers without a session. Both take effect when the intake agent
  moves to the app host.

## Consequences

- Morris wires against one written contract, and the app is built
  against the same one, while the gaps are built in the order of the
  issues.
- No agent defines how a host draws it. The intake agent's review
  becomes a stream of `review` requests, with its evidence pages as an
  optional renderer, when it adopts this in `morris-agents`.
- The library takes on two pieces of infrastructure code, the Postgres
  store and the store's wire form, so that fencing and its error
  classes are written once. A host still supplies the database, the
  listener and the authentication.
- The block vocabulary is a contract. A new block costs a version, and
  every host has to render it.

## Not decided here

- The block vocabulary's exact JSON beyond
  [docs/contract.md](../contract.md)'s first version, and the per-row
  answer's schema form: decided with the requests issue.
- The in-run wait's lifecycle details (STM-8) and its headless ending's
  reason code.
- The multi-agent host's configuration format and workspace layout:
  decided in its ADR.
- Bundling Poppler in the app, given its licence, as against requiring
  Homebrew's.

## References

- [docs/contract.md](../contract.md), the consumer-facing reference
- ADRs [0003](0003-run-model-identities-and-activation-results.md),
  [0005](0005-storage-one-fenced-commit.md),
  [0006](0006-store-interface.md), [0011](0011-manifests.md),
  [0014](0014-model-client-and-attempt-admission.md),
  [0016](0016-telemetry-logging-and-credentials.md),
  [0017](0017-the-standalone-host.md),
  [0021](0021-review-writes-approval-and-export.md),
  [0022](0022-the-review-ui.md),
  [0023](0023-public-strictjson-and-regularfile.md)
- Morris ADRs 0068 and 0069 (morris#720), issue morris#707
- `maestro-llms` ADR 0025 (`bucket/postgres` migrations)
- Maestro ADR 0030 (tool execution policy) and ADR 0032 (agent
  execution contract); v1's `pkg/proto/unified_protocol.go`
- Issue [#23](https://github.com/SnapdragonPartners/maestro-agents/issues/23)
  (TOOL-6)
