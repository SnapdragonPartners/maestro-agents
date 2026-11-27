# 0026. The store's wire form

- **Status:** Proposed
- **Date:** 2026-11-26

## Context

ADR 0024 item 9 decided that the library serves any `store.Store` over
HTTP and provides a client that is itself a store, so Morris's worker,
which holds no database credentials, reaches Morris's Postgres store
through Morris's web service (issue #29). It fixed the outline: routes
under `/v1/`, every error keeping its class, authentication left to the
host with an `Authorizer` that sees each call's operation, instance, run
and namespace, calls that name only a run authorized on the run's
instance and refused as a missing run, `Claimable` authorized as
store-wide, artifact content on raw routes under a host-set maximum
(64 MiB by default), the status routes, and an unauthenticated healthz.
It left the artifact routes' exact shape, and how an upload names its
instance and fence, to #29 with #28's design.

Instance-linked artifact reads (ADR 0024 item 9, `Reader.Artifact(ctx,
instance, digest)`) landed first, as #29's part A (PR #47), so every
read the wire serves names an instance, a namespace or a run. This ADR
decides the rest. It is built in the package `store/storehttp`.

## Decision

1. **One package, standard library only.** `store/storehttp` holds
   `NewHandler(store.Store, HandlerOptions) (http.Handler, error)` and
   `NewClient(baseURL, ClientOptions) (*Client, error)`. `*Client` is a
   `store.Store` and a `store.Telemetry`; it also offers `Status`,
   `InstanceStatus` and `Health`, the status routes in one call each. The
   package imports `agent` for the status routes; `agent` never imports
   it. It needs `net/http`, `mime/multipart` and `strictjson`, nothing
   outside the module.

2. **Routes and encoding.** One route per store method, all under
   `/v1/` (the table is in the package documentation and in
   [docs/contract.md](../contract.md)). Instances, runs, namespaces,
   trigger IDs and digests are path segments; object keys and prefixes,
   and the run query (`after`, repeated `status`, `limit`, `newest`), are
   query parameters, since a key may hold `/`.
   - **A store accepts any non-empty UTF-8 identifier**, `/`, `.` and
     `..` included, and `http.ServeMux` cleans the escaped path. So the
     client percent-encodes every byte of a segment outside ALPHA,
     DIGIT, `-`, `_` and `~`, `.` included: no identifier becomes a
     segment a mux would clean or redirect, whether the host mounts the
     handler on its own mux or not. An empty identifier cannot be a
     segment; the client refuses it with `ErrInvalid`, as the store does.
   - **Bodies are the store's own JSON types** (P-6), wrapped in an
     envelope only where a method takes or returns more than one value
     (claim's owner and lease, a heartbeat's ownership and cancel
     request, `CreateRun`'s run and `created`, an object and `ok`, a page
     of runs and `more`, an absent active run or checkpoint as `null`).
     A lease is nanoseconds.
   - **The handler decodes request bodies strictly** (`strictjson`:
     exact keys, no repeats, no trailing data), refuses an unknown or
     repeated query parameter, and refuses a body whose instance is not
     the path's, each with `ErrInvalid` before the `Authorizer` is asked.
     **The client decodes responses leniently**, so a server may add
     response fields within `/v1/`.
   - **The client refuses a request holding a string that is not valid
     UTF-8** with `ErrInvalid`, as the store does: `encoding/json` would
     replace the bytes with U+FFFD, and the store would keep another
     string than the caller's. Raw JSON values travel byte for byte and
     the handler refuses invalid UTF-8 in them; identifiers in a path or
     query arrive byte for byte and the store refuses them.
   - An object's `Bytes` (a PDF from `Env.PutBytes`) travels in the JSON
     body as base64, under the body limit. Item 9 of ADR 0024 puts only
     artifact content on raw routes.

3. **Artifact content travels raw, and the upload is the commit.**
   - **A commit that stores artifacts is one `multipart/mixed`
     request**: the first part the JSON commit, each `put_artifact` with
     its digest and media type and `content` null, then one
     `application/octet-stream` part per `put_artifact`, in operation
     order, each naming its digest in `Maestro-Artifact-Digest`. The
     handler matches each part to its operation, checks the limit,
     refuses a missing part, an extra one, a digest that differs, and
     content inside the JSON, and only then calls the store's `Commit`,
     which verifies each digest against its content. A `put_artifact` in
     an `application/json` commit is refused. So the upload names its
     instance and fence to the `Authorizer` and the store by
     construction (f-046011): it is the fenced commit. The handler keeps
     no state between requests, so several replicas behind a load
     balancer serve one store.
   - **An artifact read** answers the raw content as
     `application/octet-stream`, with its metadata (`store.ArtifactMeta`)
     as base64 JSON in `Maestro-Artifact-Meta`, since a media type is any
     UTF-8 string and need not be a valid header value. The client checks
     the content's digest and size against it; a mismatch is
     indeterminate.
   - **Limits:** `MaxArtifactBytes` per artifact (64 MiB by default,
     ADR 0024 item 9), and `MaxBodyBytes` for a request's whole body
     (128 MiB by default). Over either is `ErrInvalid` (HTTP 413) before
     the store is called. The library holds artifact content as `[]byte`
     end to end, so the handler's memory is bounded by `MaxBodyBytes`
     per concurrent request.
   - **Reserved for #28: a `put_artifact` with no part**, meaning
     "content already uploaded for this fence". Today it is refused with
     `ErrInvalid`; #28 may accept it, without a wire version, when the
     store implements an optional upload interface. Two shapes fit, and
     #28 chooses: a fenced streaming upload through the handler
     (`PUT /v1/instances/{instance}/artifacts/{digest}`, the fence in the
     request and passed to the `Authorizer` as `Call.Fence`), or a
     short-lived signed URL to the blob backend, minted on a fenced and
     authorized request, so the bytes never pass through the handler.
     Either keeps ADR 0024 item 8's guarantees. The server would
     advertise it in healthz, and the client would use it above a size
     threshold.

4. **Authorization.** The `Authorizer` sees a `Call`: `Op` (the method
   or status route), `Instance`, `Run`, `Namespaces`, `Fence` (exactly as
   a fenced call, `Commit`, `Heartbeat` or `Release`, carries it),
   `Owner` (a claim's, so a host can bind an owner identity to a
   credential) and `StoreWide`. The request's context carries whatever
   the host's authentication put there. A nil error allows the call; an
   error wrapping `storehttp.ErrUnavailable` means the `Authorizer`
   could not decide (indeterminate); any other error refuses it. There
   is no default: a host that wants none passes `storehttp.AllowAll{}`.
   Every input that names a record outside the route's own instance or
   namespace goes through one rule:
   - **A call that names only a run** (`Run`, `Entries`,
     `CancelRequest`, `RequestCancel`, the run status route): the handler
     reads the run record and authorizes on its instance, with `Run` set.
     A refusal answers `ErrNotFound`, exactly as a missing run.
   - **`Runs` with a cursor** is asked twice: for the path's instance,
     then for the cursor's run on that run's own instance, the second
     refusal answering `ErrNotFound`, as a missing cursor does. An
     authorized cursor goes to the store unchanged, so a foreign cursor
     the caller may see still answers the store's `ErrInvalid`
     (`store.RunQuery.After`).
   - **Telemetry** (`AppendEvents`, `Events`): a store accepts events
     for any valid run ID, with or without a run record, so each run is
     authorized on its instance when its record exists, and as a
     store-wide call (`StoreWide`, no instance) when it does not. An
     executing service's identity, which writes telemetry, passes; a
     token scoped to an instance is refused both for a foreign run and
     for an absent one, which therefore look alike. `AppendEvents`
     authorizes every event's run before anything is written, and a
     refusal refuses the whole call; a refused `Events` answers
     `ErrNotFound`.
   - **`Claimable`** is store-wide, with no instance (ADR 0024 item 9).
   - **`Commit`** is authorized on the fence's instance, with every
     namespace its operations touch, each once, in first-use order. Its
     operations also name runs and an instance; the store refuses a
     missing or foreign one alike with `ErrInvalid`, and the message is
     withheld (item 5), so the two answers are the same.
   - **`CreateRun`** is authorized on its instance with `Run` set to the
     requested ID. A run ID already in use answers `ErrInvalid` whichever
     instance holds it: run IDs are unique per store (ADR 0003), so that
     one bit about a guessed ID cannot be hidden without changing that. A
     host that scopes credentials to instances should derive run IDs
     under the instance (the standalone host's `<instance>.<hex>`
     already does) and can enforce it in the `Authorizer` with
     `Call.Run`.
   - **Every other call** names its instance or namespace in its path,
     and nothing in it names another.
   - `GET /v1/healthz` is not authorized.

   A refusal that is not masked is HTTP 403, which the client returns as
   `storehttp.ErrForbidden`, wrapping `store.ErrInvalid`: definite and
   nothing applied, so a host does not reread and retry a bad credential
   for ever.

5. **Errors keep their class, and only their class.** An error response
   is `{"class", "message", "request_id"}`, plus `conflict`
   (`{"kind", "op", "current"}`) for a conflict. The classes are
   `ownership`, `invalid`, `not_found`, `not_claimable`,
   `instance_busy`, `conflict`, `forbidden` and `unavailable`, with HTTP
   409, 400 (413 for a body or artifact over its limit), 404, 409, 409,
   409, 403 and 503. The client decides by `class`, never by status, and
   returns a `*storehttp.RemoteError` that unwraps to the store's error
   of the class (`errors.Is`, `errors.As` and `store.IsDefinite` see it
   as the store returned it), carrying the request ID and status.
   - **The inner error's text never crosses the wire.** `message` is
     fixed per class and a conflict's `Detail` is dropped (the client's
     `ConflictError` says it was withheld), because a store's messages
     name other instances and runs (a foreign cursor, a commit naming a
     foreign run), and sending them would undo item 4's masking. The
     handler logs the inner error with the request ID, operation and
     class through the host's `*slog.Logger`; a caller debugging its own
     `ErrInvalid` finds the text there by the request ID, which every
     response also carries in `Maestro-Request-Id`.
   - **`unavailable`** is the store's `ErrUnusable` or any other error,
     and an `Authorizer` that could not decide. The client returns it as
     `storehttp.ErrUnavailable`, indeterminate, and never as
     `store.ErrUnusable`, which would tell the host to reopen the client
     when only the server's store needs it.
   - **Anything the client cannot classify is indeterminate** (ADR 0003
     item 14): an unknown class, a body that is not the error body, a
     status without one, a conflict without its body, a 200 whose body
     does not decode, an artifact that does not match its metadata, a
     response over `MaxResponseBytes`, and a dropped or refused
     connection.

6. **No retries; deadlines are the caller's.** The client never retries:
   an indeterminate error goes back to the host, whose
   reread-before-retry (ADR 0003 item 14) stays the one retry logic.
   Each request runs under the caller's context, plus an optional
   per-request `Timeout`. The handler sets no timeouts of its own; the
   host's `http.Server` does. `Client.Close` only stops the client
   (`ErrUnusable` from every method afterwards) and leaves the server's
   store open.

7. **Versioning and health.** `/v1/` is in every path; a breaking change
   to the wire is `/v2/`, served beside it. Adding a response field is
   not breaking (item 2). `GET /v1/healthz` returns
   `{"version", "schema_version", "store"}`: the module version from the
   binary's build information (overridable), the store's schema version
   if it has `SchemaVersion() int` (omitted otherwise), and `ok` or
   `unavailable` from the store's `Ping(ctx) error` if it has one, else a
   cheap read (the ownership record of a probe instance). It answers 503
   when the store does not answer, so a load balancer can use it.

8. **Tests.** `storetest.Run` runs against the client over `httptest`,
   wrapping the memory store and the file store (its reopen closes the
   store and serves the reopened one behind a new server); the pause
   hook is the inner store's, so TEST-6 exercises the real store across
   the wire. The package's own tests cover every error class both ways
   and each indeterminate case, every call's `Call` field by field, the
   masked run-only calls and cursors (a foreign run and a missing one
   answering alike), `Claimable` and the other refusals as
   `ErrForbidden`, a commit naming a foreign run and a missing one
   answering alike, store-wide telemetry for a run with no record, an
   `Authorizer` that cannot decide, each malformed multipart commit with
   nothing applied, the raw requests the client never sends, identifiers
   holding `/`, `.`, `..`, `%`, spaces and non-ASCII, healthz, and the
   status routes against `agent.Status` over the same store.

### Amendments

- **ADR 0024, item 9:** "`storetest` covers both through the client"
  (the run-only and `Claimable` authorization cases) is replaced:
  `storetest` is store-generic and has no notion of an `Authorizer`, so
  those cases are `store/storehttp`'s own tests (item 8), and
  `storetest` covers the client's contract as a store.

## Consequences

- Morris serves its Postgres store (#28) with this handler behind its
  internal API's authentication, and its worker uses the client as its
  store, with the same fencing and error classes as a store in process.
- A host's `Authorizer` decides on IDs alone; the handler does the
  reads that turn a run into its instance, so no host rewrites that
  logic, or gets the masking wrong.
- Debugging a refused call takes the server's log: the wire says only
  the class and the request ID. That is the price of item 4's masking.
- Every artifact passes through the handler's memory, up to the limits,
  until #28 accepts item 3's reserved extension.

## Rejected alternatives

- **A separate upload route now.** A generic handler cannot stage an
  upload: `store.Store` has no method that accepts content outside a
  commit, so a separate route needs a store-side, fenced upload only a
  blob-backed store can implement. That is #28's (item 3).
- **Identifiers in the query string** for every route. They would avoid
  path cleaning, but resource routes read better in logs and proxies,
  and escaping every reserved byte, `.` included, makes path segments
  safe.
- **Sending the store's error text** for a caller's own invalid
  requests, or a redaction hook. Any text a store writes may name
  another instance; the request ID reaches the same text without that
  risk.
- **Client retries** on an indeterminate error. A retried commit or
  claim may have landed; only the host, which rereads, can tell.
- **`storetest` authorization cases**: see the amendment.

## Not decided here

- The streaming or signed-URL upload (item 3's reserved extension):
  #28.
- The app host's process contract, which serves its own routes beside
  this handler: #32.

## References

- ADRs [0003](0003-run-model-identities-and-activation-results.md),
  [0005](0005-storage-one-fenced-commit.md),
  [0006](0006-store-interface.md),
  [0024](0024-the-external-contract.md) (items 8 and 9)
- Issues #29 (this), #28 (the Postgres store and blob backend)
- [docs/contract.md](../contract.md), section 3
