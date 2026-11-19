# 0022. The review UI: `addepar serve`

- **Status:** Accepted
- **Date:** 2026-11-18

## Context

MVP.md's step 6 is the review UI. [ADR 0021](0021-review-writes-approval-and-export.md)
built its writes headless: every review action and export is a run of
the review type, submitted in process (`Host.Submit`), and `addepar
export` writes the CSV and its manifest. This ADR is the second half:
the UI an operator uses, R-1 to R-4 and X-1 in a browser, and what it
takes to hand the agent to an operator who is not technical.

The human operator settled the shape on 2026-11-17 and 2026-11-18:

- **One process.** `addepar serve` watches the inbox and serves the UI,
  so an operator starts one thing, and the file store's exclusive lock
  (ADR 0017, item 9) never keeps the UI out of a store a watch holds
  (ADR 0021, item 14). `addepar run` stays the headless runner (N-8).
- **A workspace**: visible `Inbox/` and `Exports/`, and a hidden
  `.agent/` holding the store and the manifests (ADR 0021, item 12).
- **Standard library only**: `net/http`, `html/template` and `embed`,
  with a little vanilla JavaScript for the evidence overlay; JSON routes
  beside the HTML, so a menu-bar app (the `maestro-macos` pattern), a
  control plane or Morris can drive the same operations.
- **Loopback only**, with a typed operator name per session, a Host
  check and CSRF protection, and the `maestro-macos` session-token
  contract, so a control-panel app can launch the binary and open the
  UI without a password prompt.
- **First-run setup** in the browser: the workspace, the model and its
  credential, written by `serve` itself; the next step is a signed
  binary download (GoReleaser, Homebrew for Poppler), whose design
  follows this PR.

## Decision

### The host: an in-process watch

1. **`Host.Watch(ctx) (int, error)` runs the folder watch in process.**
   It is `run`'s watch, scheduler included, on a host from
   `standalone.Open`, and returns `run`'s exit code when `ctx` ends or
   the store must be reopened (ADR 0017, items 21 and 22); a
   configuration whose trigger is not `folder` is `ErrNotFolder`, and a
   second watch on a host while one runs `ErrWatching`, both at once.
   The watch and `Submit` already share the per-instance locks (ADR 0021,
   item 3), so a review submitted while the watch runs waits for, or
   finishes, the document's run, and the watch skips an instance a
   submission holds. This amends ADR 0021, item 14 ("the in-process host never watches").

### The command and the workspace

2. **`addepar serve [-workspace DIR] [-addr HOST:PORT] [-open=false]`.**
   The workspace defaults to `~/Documents/Addepar Intake` (the human
   operator's choice: where a Mac user looks for their files). `serve` creates it if
   missing (`Inbox/`, `Exports/`, and `.agent/` owner-only), then opens
   `.agent/config.json` with `standalone.Open`, starts `Host.Watch` and
   serves the UI on `-addr`, which must be a loopback address
   (`127.0.0.1`, `::1` or `localhost`; anything else is a usage error)
   and defaults to `127.0.0.1:8741`. The first SIGINT or SIGTERM stops
   the server (requests in flight get five seconds, then their context is
   cancelled, a submission stopping at its next step boundary, and every
   handler is joined before the host closes) and the watch at its next step
   boundary (ADR 0017, item 21); the exit code is the watch's.
3. **The configuration is the standalone host's** (ADR 0017), written
   by setup (item 4) and editable by a technical user: `agent_type`
   `addepar-intake`, namespace `intake`, store `{"kind": "file", "dir":
   "store"}` and trigger `{"kind": "folder", "dir": "../Inbox"}`
   (relative paths resolve against `.agent/`, ADR 0017 item 4), telemetry
   `store`, the model route and credential, and the example
   configuration's budgets. CSVs go to `Exports/` and manifests to
   `.agent/exports/` (ADR 0021, item 11). `addepar export -config
   .agent/config.json` still works while `serve` is stopped.
4. **First-run setup.** With no `.agent/config.json`, `serve` serves
   only the setup page (behind the session, item 6): the model provider
   (Anthropic or OpenAI; ADR 0019, item 7, covers both for production),
   the model name, prefilled, and the API key. On submit it writes the
   key to the secrets file (item 5) and the configuration, validates it
   as `config validate` does, opens the host and starts the watch,
   without a restart. A configuration that does not validate is shown
   with its errors, and nothing starts. Setup never overwrites an
   existing configuration; changing the model later is a technical
   user's edit, or deleting the configuration to run setup again.
5. **The API key lives outside the workspace**, in the workspace's own
   secrets file, `<user config dir>/addepar/<credential>.json` (on macOS
   `~/Library/Application Support/addepar/`), owner-only, as ADR 0017's
   secrets file (item 19): copying `.agent/` for an auditor must never
   carry the key. The credential is `workspace-` and the first 16 hex
   digits of the SHA-256 of the workspace's absolute path; the
   configuration names it and the file by absolute path. No file is
   shared between workspaces, so setting up two at once loses neither
   key, and setup creates the configuration exclusively before writing
   the key, so a second setup of one workspace stops before touching it. A control-panel
   app that keeps the key in the Keychain can pass it by environment
   instead (`credentials.env`); that is its configuration.

### Session and security

6. **The session.** `serve` reads `ADDEPAR_SESSION_TOKEN` at start, or
   generates 32 random bytes, and prints the link
   `http://<addr>/auth/session?token=<token>`, opening it in the default
   browser unless `-open=false`. `GET /auth/session?token=` compares the
   token in constant time and, on a match, sets a session cookie
   (`addepar_session_<port>`, since browsers scope cookies by host and
   not port, so two servers on one machine keep their own; 32 random bytes, `HttpOnly`, `SameSite=Strict`,
   `Path=/`, no `Max-Age`: it ends with the browser) and redirects to `/`.
   The token stays valid for the life of the process, so the link (or a
   control-panel app) reopens the UI after the browser is closed; the
   same user can already read the process's environment. Sessions live in
   memory and end with the process. Every other route answers 401 without
   a valid session cookie, or, for the JSON routes, `Authorization:
   Bearer <token>`, which a control-panel app uses.
7. **The operator's name.** A session has no name until the person
   types one (a form shown before anything else); it is the `by` of every
   action the session submits, shown in the header, and changeable. It is
   a recorded attribution, as R-4 asks ("a named person"), not
   authentication: whoever holds the session can type any name, as anyone
   at the operator's desk could.
8. **Request checks.** The server answers only requests whose `Host`
   header is the address it listens on, by IP or as `localhost`, with
   the port (against DNS rebinding). Every unsafe request (POST) carries
   the session's CSRF token, a form field or the `X-CSRF-Token` header,
   compared in constant time, and its `Origin`, when present, must match;
   a bearer-token request needs neither. Responses carry
   `Content-Security-Policy: default-src 'self'; frame-ancestors 'none'`
   (no inline script), `X-Content-Type-Options: nosniff`,
   `Referrer-Policy: same-origin` (under `no-referrer` a browser sends
   `Origin: null` on a same-origin form post) and `Cache-Control: no-store`: every
   page holds client data.

### The UI

9. **Pages, rendered on the server.** Forms post and redirect
   (post/redirect/get), so every action works without JavaScript:
   - `/`: the documents, newest first, each with its state: queued,
     being read, verified, set aside, sent to manual review, or failed
     (the intake run's reason code), from the intake run (`sha256-<hex>`,
     ADR 0017 item 11) and its outcome; under each, its records with
     their state (item 10). The page refreshes itself while a document
     is queued or being read.
   - `/documents/{sha}`: the document, its outcome and attempts, its
     records, and the form to add a manual record (ADR 0021, item 7),
     which is how a document sent to manual review, set aside, or failed
     gets a record.
   - `/records/{sha}/{nn}`: one record (R-1): the page image with the
     evidence highlighted (item 11), the fields with their pages, the
     seven checks with their findings, the owner and asset, the review
     log, and the actions: correct (the record as a form, prefilled,
     with a reason), accept a warning (by finding, with a reason), set
     the owner and asset, approve. Each form carries the log length the
     person saw (`seen`), so a stale view is refused (ADR 0021, item 6)
     and the refusal says to reload.
   - `/exports`: the approved records not yet exported, all selected by
     default, the export button, and past exports with their CSV (X-1).
   A refusal comes back as a message on the page it came from, in the
   refusal's own words (`review.Refusal.Message`).
10. **A record's state** is `review.Current` and the exports (ADR 0021,
    item 8): needs review (with what blocks approval: `export.Gate`'s
    reasons), approvable, approved, or exported. Nothing is stored for
    the UI.
11. **Evidence highlighting.** Each page image is the stored PDF
    (`pdfs/<sha>`) rendered by `maestro-cms`' `pdftoppm` renderer, cached
    in memory (bounded). The boxes come from the PDF's words
    (`maestro-cms`' pdftotext layout engine, word boxes in points, cached
    per document): for an amount, every word on the cited page whose
    value equals it (separators, currency signs and parentheses read as
    the verifier reads them) and the words of its label's line; for a
    text field (investor, fund, the period's label, the column header,
    the currency's evidence), the words that spell it, in order. Every
    copy on the page is highlighted: telling the copy the value came from
    apart (QTD beside an identical YTD) needs the verifier's match
    position, which it does not record (not decided here). Words are read
    only where the document's text was read (a text layer, and for a
    copy-restricted PDF the recorded acceptance, ADR 0018 item 3); a
    scan shows its pages without boxes.
12. **Export from the UI** submits the export action (ADR 0021, item 9)
    for the selected records, then writes the files from what the run
    stored, as `addepar export` does. A write that fails after the
    commit is shown as such, with the export's run and a button to write
    its files again (`-rewrite`'s path); every other failure shows its
    own message, never a rewrite hint that cannot succeed (buggrit
    f-825a89 on PR #22).
13. **The JSON routes** sit under `/api/` and run the same code as the
    pages: `GET /api/documents`, `GET /api/records/{sha}/{nn}`, `POST
    /api/records/{sha}/{nn}/actions` and `POST
    /api/documents/{sha}/manual` (a `review.Action` without `by` and
    `at`, which the server sets from the session and its clock; a bearer
    request names `by` in the body), `GET /api/exports` and `POST
    /api/exports`. A refusal is `409` with the refusal, an unknown record
    `404`, a malformed body `400`, a busy instance (`ErrBusy`) `503` with
    `Retry-After`.

### Amendments

- **ADR 0021, item 14:** the in-process host can watch (`Host.Watch`,
  item 1); `addepar serve` is the one process that watches and serves.
- **ADR 0021, item 12:** the workspace is created by `serve`, at
  `~/Documents/Addepar Intake` by default, and its configuration is
  `.agent/config.json` (items 2 and 3).

## Consequences

- An operator installs one binary, runs `addepar serve`, finishes setup
  in the browser, drops PDFs into `Inbox/` and takes CSVs from
  `Exports/`. A technical user still has `run`, `export` and the
  configuration file.
- Every action a person takes in the UI is a review run (ADR 0021): the
  UI holds no state of its own beyond sessions, so restarting `serve`
  loses nothing but sign-ins.
- The UI is as secure as the operator's login session: loopback only,
  a per-process token, cookies that never leave the machine. It is not a
  multi-user server.
- The page image and word boxes are computed on demand from the stored
  PDF, so nothing derived is stored, and a document reviewed months
  later renders the same way.

## Not decided here

- Remote or multi-user access, TLS, real authentication: not asked for.
- Highlighting the exact copy a value was read from (item 11).
- Re-running extraction from the UI (ADR 0021, Not decided).
- Vertex AI in setup (a `Program.Providers` entry, ADR 0019 item 7).
- The release: GoReleaser, signing and notarization, the Homebrew cask
  with Poppler, and whether a menu-bar app (`maestro-agents-macos`)
  wraps `serve`: the next PR.
- Per-document copy-restriction acceptance (#17): until then, running
  the agent is the acceptance (ADR 0018, item 3), in the UI as in `run`.

## References

- Intake requirements R-1 to R-4, X-1, N-8; MVP.md step 6
- [ADR 0017](0017-the-standalone-host.md): the standalone host
- [ADR 0018](0018-the-intake-agent-type-and-the-vertical-slice.md),
  [ADR 0019](0019-the-extraction-agent-reads-the-pdf.md): the intake
  objects; covered model routes
- [ADR 0021](0021-review-writes-approval-and-export.md): review writes,
  approval and export
- `maestro-macos` `MAESTRO_CHANGES.md`: the session-token contract
- `maestro-cms` v0.9.0, `docs/pdf-layout-and-rendering.md`: word boxes
  and page rendering
