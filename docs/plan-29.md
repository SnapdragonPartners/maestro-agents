# Plan: #29, the store's wire form

A working plan, for review before the code is written. It is removed
before the branch is pushed: what it decides is recorded in ADR 0026
(the wire form) and in the doc comments, and ADR 0024 item 9 already
records the rest.

Controlling text: ADR 0024 item 9 (the wire form), item 8 (the blob
backend's guarantees, #28) and its amendment of ADR 0005 item 3 (a
re-store for a new instance is a write, "the doc comments in
`store/store.go` and `store/ops.go` change with #29"); issue #29 and its
two comments (the artifact-to-instance link rule; buggrit f-4ba64f's
`storetest` case); issue #28's comment (buggrit f-046011: the upload
route must name the instance and carry the fence).

## Two PRs, merged in sequence

### PR A: instance-linked artifacts (branch `feat/artifact-instance-links`)

A store-contract change that stands without the wire.

1. **`Reader.Artifact(ctx, instance, digest)`** answers only for a
   digest linked to `instance`; otherwise `ErrNotFound`, exactly as
   for a digest never stored, so a reader learns nothing of other
   instances' artifacts. An invalid instance ID or digest is
   `ErrInvalid`.
2. **`put_artifact` links the digest to the fence's instance**, the
   link written whether or not the content is already stored. The
   first metadata still stands and the operation still has no
   precondition and never conflicts. A commit that only re-stores
   content the instance is already linked to stays a no-op in the
   reference stores; one that links a new instance writes.
3. **Reference stores (`internal/storecore`):** a link is its own
   record under the instance, `instances/<instance>/artifacts/<digest>`
   (empty, or the time linked), so it is keyed by instance for a later
   purge (ADR 0024 item 8) and written in the commit's own atomic
   batch. Content and metadata stay at `artifacts/<digest>`, stored
   once.
4. **Artifacts stored before links existed** (file stores on disk
   today, the intake agent's operators' included): such an artifact
   has metadata and content but no link, so under item 1 every
   recovery that rereads a run's recorded configuration would fail.
   **A runtime-aware migration from authoritative references; nothing
   ambiguous is guessed** (rounds 1 and 2, P1: no manufactured links,
   no digest-in-arbitrary-JSON evidence, no silent loss).
   - Only the runtime writes artifacts (`agent/controller.go:675-680`:
     the manifest, configuration and served definitions;
     `agent/modelstate.go:202`: model-state content), and it names each
     one in a typed field of its own entries (`RunStarted.Manifest`,
     `.Config`, `.Served` in `agent/record.go`, and the model-state
     entries' content digests). So `agent` exports
     `ArtifactReferences(ctx, store.Reader, instance) ([]string, error)`:
     it pages `Runs(instance)`, reads each run's entries, decodes only
     the recognized kinds into their own types and collects those
     fields. Version-aware: an entry kind it does not recognize names
     nothing; a recognized kind whose payload does not decode is an
     error, never skipped.
   - The file store detects a legacy layout (artifacts present, no
     link-format marker) and **refuses to open** it with
     `filestore.ErrMigrationRequired`, unless opened with
     `filestore.WithLegacyArtifactLinks(fn)`, where `fn` is
     `agent.ArtifactReferences`'s shape. Then, before serving anything,
     it enumerates its instances (it knows its own layout; `Reader` has
     no instance list), links each digest `fn` returns for an instance
     to that instance where the artifact exists, and requires every
     stored artifact to be claimed by at least one instance.
   - **An artifact no instance references** (valid today: storetest's
     `durability` case commits one, and a host store or an older
     agent might) makes the migration fail with nothing written, the
     error listing the digests. The operator then states their owners
     explicitly with `filestore.WithArtifactOwners(map[digest][]instance)`,
     each instance named must exist; an unresolved digest still fails.
     Nothing is hidden silently and nothing is granted by inference.
   - Links and the marker are written in one atomic batch (the store's
     journal), so a crash repeats the migration whole.
   - The standalone host opens its file store with
     `WithLegacyArtifactLinks(agent.ArtifactReferences)`, so an
     operator's store written by the runtime migrates on first open
     with no step; a store with unreferenced artifacts fails to open
     with a message naming the digests and the owners option. (A CLI
     flag for the owners map is left until someone needs it; a
     runtime-written store has none.)
   - The memory store has no legacy; the Postgres store (#28) starts
     with links.
   - Tests: a store in today's layout with instances A and B, each
     with runs naming their artifacts, plus a digest appearing only
     inside B's unrelated checkpoint data and A's entries (linked to A
     only), plus storetest's `durability` shape (an artifact no entry
     names): open without the option refuses; with it, the migration
     refuses listing that digest and writes nothing; with an owners
     map it links as stated; a reopen does not migrate again; a crash
     mid-batch repeats it.
   Rejected: linking unlinked artifacts to every instance (grants B
   A's history); legacy artifacts readable by any instance (an
   isolation exception the contract does not have); evidence by digest
   text in an instance's records (round 2: checkpoint state and entry
   payloads are arbitrary JSON, so recording a digest proves nothing).

5. **Runtime callers** pass their run's instance:
   `agent/controller.go` (recorded configuration),
   `agent/modelstate.go` (stored model-state content),
   `agent/served.go` (served definitions). The scoring harness and
   standalone host read no artifacts.
6. **`storetest`** (f-4ba64f): a put links the digest to the committing
   instance; a read under another instance is `ErrNotFound` (the same
   error as a digest never stored); re-storing the same content from a
   second instance's commit links it there, keeps the first metadata
   and leaves the first link; a read of a linked digest after reopen
   (durable stores) still answers. Existing artifact cases gain the
   instance argument.
7. **Doc comments** per ADR 0024's amendment of ADR 0005 item 3:
   `Committer.Commit` ("a commit whose operations change nothing"
   no longer covers a re-store for a new instance), `store.Artifact`
   (`ops.go`) and `Reader.Artifact`, and `docs/contract.md`.
8. **Consumer impact:** `Reader.Artifact` changes signature, a breaking
   change to every `store.Store` implementation and caller before v1
   (ADR 0023 item 4: the release notes say so). Morris's worker has no
   store yet.

### PR B: the wire form (branch `feat/store-wire`), with ADR 0026

A package `store/storehttp`:

- `NewHandler(s store.Store, o HandlerOptions) http.Handler`, options:
  `Authorizer` (required; a host that wants none passes an explicit
  allow-all), `MaxArtifactBytes` (default 64 MiB, ADR 0024 item 9),
  `MaxBodyBytes` for JSON bodies, `Version` override for healthz.
- `NewClient(baseURL string, o ClientOptions) (*Client, error)`, a
  `store.Store`, and a `store.Telemetry` that answers `ErrInvalid`
  (`unsupported`) when the server's store has none. Options: the
  `*http.Client` (the host's auth transport goes here), an optional
  per-request ceiling beside the caller's context.

**Routes, all under `/v1/`, one per method:**

| Method | Route |
|---|---|
| Commit | `POST /v1/instances/{i}/commit` (JSON, or `multipart/mixed` when it carries artifacts) |
| Claim / Heartbeat / Release | `POST /v1/instances/{i}/claim`, `/heartbeat`, `/release` |
| EnqueueTrigger / RemoveTrigger | `POST /v1/instances/{i}/triggers`, `DELETE /v1/instances/{i}/triggers/{id}` |
| CreateRun | `POST /v1/instances/{i}/runs` |
| RequestCancel / CancelRequest | `POST` / `GET /v1/runs/{id}/cancel` |
| Ownership, ActiveRun, Triggers | `GET /v1/instances/{i}/ownership`, `/active-run`, `/triggers` |
| Runs | `GET /v1/instances/{i}/runs?after=&status=&limit=&newest=` |
| LatestCheckpoint / Checkpoint | `GET /v1/instances/{i}/checkpoints/latest`, `/checkpoints/{seq}` |
| Run, Entries | `GET /v1/runs/{id}`, `GET /v1/runs/{id}/entries?after=` |
| Artifact | `GET /v1/instances/{i}/artifacts/{digest}`: raw bytes, metadata in headers (`Content-Type`, `Content-Length`, a digest header) |
| Object / ListObjects | `GET /v1/namespaces/{ns}/object?key=`, `GET /v1/namespaces/{ns}/objects?prefix=` (keys may hold `/`, so in the query) |
| Claimable | `GET /v1/claimable` |
| AppendEvents / Events | `POST /v1/events`, `GET /v1/runs/{id}/events?after=` |
| Status | `GET /v1/runs/{id}/status`, `GET /v1/instances/{i}/status` (`agent.Status`, `agent.InstanceStatus`) |
| Health | `GET /v1/healthz`, unauthenticated: `{"version", "schema_version", "store"}` |

Bodies are the store's own JSON types (P-6); no wire-only shapes except
the error body and the small request envelopes (claim's owner and lease,
heartbeat's fence and lease, cancel's cause). Path segments are IDs the
store already restricts; the handler percent-decodes and the store
validates.

**Artifact content in a commit (the f-046011 question).** A commit that
carries `put_artifact` operations is one `multipart/mixed` request: the
first part the JSON operations, each `put_artifact` with its digest and
media type and no content, then one raw part per artifact, in operation
order, each with its digest in a header. The handler reads the parts,
checks each against its digest and `MaxArtifactBytes`, rebuilds the
operations and calls the inner `Commit`. The upload is the fenced
commit, so it names its instance and fence to the `Authorizer` and the
store by construction, and the handler keeps no state between requests
(several replicas behind a load balancer work). Content over the limit
is `ErrInvalid` before the inner store is called.

**Size, and why not a separate upload route now.** #28's sketch
uploaded content before the commit because blob-backed content was
expected to be large. Two facts bound what a separate route would buy
today:

- The library holds artifact content as `[]byte` end to end
  (`store.Artifact.Content`, the runtime's builders, `Commit`). The
  worker already has every artifact in memory before the wire sees it.
- A generic handler cannot stage an upload: `store.Store` has no
  method that accepts content outside a commit. A separate route would
  need a store-side, fenced, streaming upload (the pin of #28's
  sketch), which only a blob-backed store could implement.

So the handler's memory is the cost: up to `MaxArtifactBytes` per
artifact, bounded per request by `MaxBodyBytes`, times concurrent
commits. Proposal: multipart commit now; **reserve** the extension, so
#28 can add it without a wire version change: a commit's
`put_artifact` may name a digest with no part, meaning "content already
uploaded for this fence", which today's handler refuses with
`ErrInvalid` and a later one accepts when the inner store implements an
optional upload interface. Two shapes fit, and #28 picks:

- **Through the handler:** `store.ArtifactUploader`, a fenced
  `UploadArtifact(ctx, fence, digest, mediaType, io.Reader)`, served at
  `PUT /v1/instances/{i}/artifacts/{digest}` with the fence in the
  request, passed to the `Authorizer` (as `Call.Fence`) and the store,
  which pins and streams it to the blob backend (#28's sketch).
- **Direct to the blob backend:** the handler, on a fenced and
  authorized request, returns a short-lived signed upload URL for that
  instance and digest (Cloud Storage for Morris), recording the pin;
  the worker uploads there itself, then commits the reference, so the
  bytes never pass through the handler. The commit's pin check and the
  sweep keep ADR 0024 item 8's guarantees (content is reachable only
  through a committed link; orphans are swept). This needs the store
  to mint URLs and the worker to reach the backend: #28's and Morris's
  call.

The client uses whichever the server advertises (healthz) for content
above a threshold. This PR builds neither.
**Question for review:** is reserving this enough, or should the
streaming route be designed now, given artifacts are expected to stay
well under 64 MiB (manifests, configurations, transcripts, exports)?

Related: an object's `Bytes` (a PDF from `Env.PutBytes`) travels in
the JSON body as base64 (`encoding/json`'s `[]byte`), about a third
larger, under `MaxBodyBytes`. ADR 0024 item 9 sends only artifact
content on raw routes; objects stay JSON unless review thinks
otherwise.

**Authorization.** `Authorizer` is
`Authorize(ctx context.Context, c Call) error` with
`Call{Op string; Instance string; Run string; Namespaces []string;
Fence *store.Fence; Owner string; StoreWide bool}`; the request's
context carries whatever the host's authentication put there. `Fence`
is set, exactly as the request carries it, for every fenced call
(`Commit`, `Heartbeat`, `Release`, and the reserved upload), and
`Owner` for `Claim`, so a host can bind an owner identity to a token
(round 1 P1: ADR 0024 item 9 and f-046011 ask that the upload name its
fence to the `Authorizer`). A test checks each call's `Call` field by
field.

The rule, one choke point (`resolve` in the handler) for every input
that names a record outside the route's own instance or namespace
(round 1 P1: indirect references):

- **Run-only routes** (`Run`, `Entries`, `CancelRequest`,
  `RequestCancel`, run status): the handler reads the run and
  authorizes on its instance; a refusal answers `ErrNotFound`, exactly
  as a missing run (ADR 0024 item 9).
- **Telemetry** (`Events`, `AppendEvents`), whose runs need no run
  record (`store.Telemetry` accepts events for any valid run ID, and
  `storetest` checks it on an empty store): authorized on the run's
  instance when the run exists, and as a store-wide call
  (`StoreWide`, no instance) when it does not, so the telemetry
  contract is unchanged, an executing service's identity (which writes
  telemetry) passes, and a token scoped to an instance is refused both
  for a foreign run and for an absent one, which therefore look alike.
  `AppendEvents` authorizes every event's run before any write and is
  refused as a whole; a refused `Events` answers `ErrNotFound`.
- **`Claimable`:** store-wide, no instance (ADR 0024 item 9).
- **`Runs` with `After`:** the handler reads the cursor's run first
  and authorizes it on that run's instance, as a run-only route. A
  refused cursor answers `ErrNotFound`, exactly as a missing one; an
  authorized cursor goes to the store unchanged, so a foreign cursor
  the caller may see still answers the store's `ErrInvalid`
  (`store.RunQuery.After`, `storetest/runs.go`), and the contract is
  unchanged (round 2 P1). The store's message naming the other
  instance never crosses the wire (below).
- **`Commit`:** authorized on the fence's instance, with every
  namespace its operations touch, deduplicated. Its operations name
  runs (`append_entry`, `update_run`, a checkpoint's scope) and an
  instance (a checkpoint); the store refuses a missing or foreign one
  alike with `ErrInvalid`, and the message is withheld (below), so the
  answer is the same for both.
- **`CreateRun`:** a run ID already in use answers `ErrInvalid`
  whichever instance holds it; run IDs are unique per store (ADR
  0003), so this one bit about a guessed ID cannot be hidden without
  changing that. ADR 0026 says so and advises a host that scopes
  tokens to derive run IDs under the instance (the standalone host's
  `<instance>.<hex>` already does); `Call.Run` carries the requested ID
  so the `Authorizer` can enforce that.
- **Instance- and namespace-keyed routes** (claim, triggers, runs,
  checkpoints, ownership, active run, artifacts, objects, instance
  status): authorized on the path's instance or namespace; nothing in
  them names another.
- Healthz is not authorized.
- Any other refusal is HTTP 403, which the client returns as
  `storehttp.ErrForbidden`, wrapping `store.ErrInvalid`: definite,
  nothing applied, so a host does not reread and retry a bad token for
  ever.

Review: enumerate every input at this boundary against the list above.

**Errors.** The error body is `{"class", "message", "request_id"}`
plus, for a conflict, `{"kind", "op", "current"}`. Classes:
`ownership`, `invalid`, `not_found`, `not_claimable`, `instance_busy`,
`conflict`, `forbidden`, and `unavailable` (the inner store's
`ErrUnusable` or any other error; indeterminate, never
`store.ErrUnusable` on the client, which would tell the host to reopen
the client). Status codes follow (409 conflict / ownership / not
claimable / busy, 400 invalid, 404, 403, 503), but the client decides
by `class`, and anything it cannot parse, a dropped connection or a 5xx
without a class included, is indeterminate (ADR 0003 item 14).
**The inner error's text never crosses the wire**: `message` is fixed
per class, the conflict's `Detail` is dropped, and the handler logs
the inner error with the request ID through the host's `*slog.Logger`
(an option). Store messages name other instances and runs (the cursor
and commit cases above), so sending them would undo the masking; a
caller debugging its own `ErrInvalid` finds the text in the server's
log by request ID. This answers the round's question 3 without a
redaction hook.

**Retries and timeouts.** The client never retries; an indeterminate
error goes back to the host, whose reread-before-retry (ADR 0003 item
14) stays the one retry logic. Deadlines come from the caller's
context, plus the optional ceiling. The handler sets no write timeout
of its own (the host's server does).

**Versioning.** `/v1/` in every path; a breaking change to the wire is
`/v2/` beside it. Healthz reports the module version (from build info,
overridable), the store's schema version through an optional
`interface{ SchemaVersion() int }` (absent: omitted), and `store`
`ok` / `unavailable` from an optional `Ping(ctx) error`, else a cheap
read (`Ownership` of a fixed probe instance ID).

**Tests.**

- `storetest.Run` against the client over `httptest`, wrapping the
  memory store and the file store (reopen included: the client's
  reopen reopens the inner store behind a new server). The pause hook
  is the inner store's, so TEST-6 exercises the real store across the
  wire.
- `storehttp` tests: every error class round-trips; unknown class,
  garbage body, connection drop mid-response all indeterminate;
  authorization (run-only calls answer `ErrNotFound` when refused,
  `Claimable` sees no instance, a commit sees every namespace,
  `AppendEvents` across runs refused as a whole, `Call` field by field
  including `Fence` and `Owner`, a `Runs` cursor on a foreign run and
  on a missing one answering alike, a commit naming a foreign run and a
  missing one answering alike, telemetry for a run with no record
  passing a store-wide grant and refused to an instance-scoped one, no
  inner error text in any body); multipart commit
  (digest mismatch, part missing, extra part, over the limit, all
  `ErrInvalid` with nothing applied); healthz without auth; status
  routes match `agent.Status` over the same store.
- ADR 0024 says `storetest` covers the run-only and `Claimable`
  authorization cases through the client. They need an `Authorizer`,
  which `storetest` (store-generic) has no notion of, so they live in
  `storehttp`'s tests; ADR 0026 records the deviation.

**Dependencies:** standard library only (`net/http`, `mime/multipart`).
`store/storehttp` imports `agent` for the status routes; `agent` never
imports it.

**Docs:** ADR 0026 (encoding, routes, authorization, errors,
versioning, timeouts, retry behavior, the reserved upload extension),
`docs/contract.md`'s wire section (planned → built), CLAUDE.md's state,
layout and load-bearing decisions.

## Not in either PR

The Postgres store and any blob backend (#28), published artifacts
(#30), the lookup of a run by (instance, trigger ID) (#31), the app
host (#32).
