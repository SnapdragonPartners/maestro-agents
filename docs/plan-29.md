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
   Proposal: the file store, at open, backfills links from what it
   already holds, for each run's `run_started` entry linking the
   digests that entry names (configuration, served definitions) to
   the run's instance, and for model-state content the digests the
   run's entries name. **Open question for review:** the runtime
   knows which entries name digests, the store does not (entry kinds
   and payloads are the runtime's, opaque to the store). Alternatives:
   (a) the store links every unlinked artifact to every instance that
   existed when it was opened, a one-time, local-only widening that
   is no worse than today's digest-only reads; (b) refuse to open a
   store with unlinked artifacts and ship a one-shot migration in the
   runtime (`agent` walks each run's entries); (c) leave legacy
   artifacts readable by any instance, documented, in the reference
   stores only. I lean to (a): it keeps the store ignorant of entry
   payloads, it runs once (a marker file records it), and the stores
   it touches are single-operator local stores. The memory store has
   no legacy.
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
#28 can add it without a wire version change: an optional store
interface (`store.ArtifactUploader`: a fenced
`UploadArtifact(ctx, fence, digest, mediaType, io.Reader)`), served at
`PUT /v1/instances/{i}/artifacts/{digest}` only when the inner store
implements it, after which a commit's `put_artifact` may name an
uploaded digest without a part. The client uses it for content above a
threshold when the server advertises it (healthz or an `OPTIONS`).
Whether #28 needs it is #28's decision; this PR builds neither.
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
`Call{Op string; Instance string; Run string; Namespaces []string}`;
the request's context carries whatever the host's authentication put
there.

- A call that names only a run (`Run`, `Entries`, `CancelRequest`,
  `RequestCancel`, `Events`, run status): the handler reads the run
  first, passes its instance, and a refusal answers `ErrNotFound`,
  exactly as a missing run (ADR 0024 item 9).
- `Claimable`: `Call` with no instance (ADR 0024 item 9).
- `Commit`: the fence's instance, and every namespace the operations
  touch, deduplicated, so a run's token is confined to its instance
  and namespace.
- `AppendEvents`: events may span runs; each run's instance is read and
  authorized, all before any write.
- Healthz is not authorized.
- Any other refusal is HTTP 403, which the client returns as
  `storehttp.ErrForbidden`, wrapping `store.ErrInvalid`: definite,
  nothing applied, so a host does not reread and retry a bad token for
  ever.

**Errors.** The error body is `{"class", "message"}` plus, for a
conflict, `{"kind", "op", "current", "detail"}`. Classes: `ownership`,
`invalid`, `not_found`, `not_claimable`, `instance_busy`, `conflict`,
`forbidden`, and `unavailable` (the inner store's `ErrUnusable` or any
other error; indeterminate, never `store.ErrUnusable` on the client,
which would tell the host to reopen the client). Status codes follow
(409 conflict / ownership / not claimable / busy, 400 invalid, 404, 403,
503), but the client decides by `class`, and anything it cannot parse,
a dropped connection or a 5xx without a class included, is
indeterminate (ADR 0003 item 14). The message carries the inner
error's text; whether to redact it is a host option (`Redactor`-like
hook) **or not needed (question for review)**.

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
  `AppendEvents` across runs refused as a whole); multipart commit
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
