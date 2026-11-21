# 0021. Review writes, approval and export

- **Status:** Amended by ADR-0022, ADR-0023; the standalone host's submitted runs and in-process host (items 1 to 4 and 14) and strict decoding (item 13, as ADR-0023 amends it) stay in force here; the review agent type, approval and export moved (item 11, which ADR 0024 changes, as addepar ADR 0005 item 11) to morris-agents as [addepar ADR 0005](https://github.com/SnapdragonPartners/morris-agents/blob/main/addepar/docs/adr/0005-review-writes-approval-and-export.md)
- **Date:** 2026-11-17

## Context

MVP.md's step 6 is the review UI. Its intake requirements are R-1 to R-4
and X-1, X-2: show each record with its evidence (R-1); let a person
correct a field, enter a value manually with page and note, or accept a
warning, each action recording who, when and why and rerunning
verification (R-2); enter the Addepar owner and asset (R-3); require a
named person's approval before export, voided by any later change (R-4);
export the approved records an operator chooses as the CSV and its
evidence manifest (X-1, X-2). The data model adds review actions,
append-only, and exports, with a record's state (needs review, approved,
exported).

The step is split in two pull requests, merged in order: this ADR's, the
review writes and export, headless and tested without a browser; then
the UI (`addepar serve`) on top, with its own ADR. The human operator
settled the shape on 2026-11-17: one process serves the folder watch
and the UI, because the end users need it simple; review actions are
runs, so their audit trail is the library's; and the audit material lives
in a hidden directory, out of a non-technical user's sight and whole for
an auditor.

Three facts of the library shape the design:

- **Every durable write an activation makes goes through one fenced
  commit** ([ADR 0005](0005-storage-one-fenced-commit.md)), and outside
  an activation only the control plane writes. A review action has
  nowhere to go unless it is an activation, or the store grows an
  unfenced object write.
- **The file store takes an exclusive lock**, so whatever writes review
  actions must be the process the host runs in
  ([ADR 0017](0017-the-standalone-host.md), item 9).
- **The intake records are written once**
  ([ADR 0018](0018-the-intake-agent-type-and-the-vertical-slice.md),
  item 5): `records/<sha>/<NN>` is the record as extracted and verified,
  and the requirements' reproducibility (N-2) wants it kept that way.

The standalone host runs one configured agent type, and a run's ID is its
instance's, `sha256-<hex>` of its input file (ADR 0017, items 9 and 11):
it has no way to start a run that has no file.

## Decision

### The host: submitted runs

1. **A program declares submitted agent types.** `Program.Submitted`
   lists agent types, besides the configured one, that the host runs when
   a caller submits a trigger in process, not from a file. Each must be
   registered and declare the trigger kind `standalone.SubmittedKind`
   (`submitted`) in its manifest; its configuration is absent (JSON
   null, which its schema must accept) and its budgets are its manifest's
   defaults; it runs in the configuration's namespace, under its
   principal, with its model client. Validation (item 7 of ADR 0017)
   checks all of this before anything runs, the model route included: a
   submitted type with a model state needs `model` as the configured
   type's would.
2. **`Host.Submit` runs one submission to its end.** `Host.Submit(ctx,
   Submission{AgentType, Instance, Payload}) (Report, error)` makes a
   run and activates it, synchronously, and returns its report, as
   `RunInput` does for a file. The host names the trigger and the run:
   the trigger ID is `submitted:<hex>`, the run ID
   `<instance>.<hex>`, `<hex>` 16 random bytes, so a submission is never
   deduplicated against another; a repeat is the caller's to prevent
   (item 6 does it for reviews). The instance is the caller's: letters,
   digits, `.`, `_` and `-`, at most 128 bytes, and short enough that the
   derived run ID fits a store path component once encoded (the stores
   write an uppercase letter or `.` in three bytes), else `ErrInvalid`,
   before anything is enqueued.
3. **One activation per instance, the submission's included.** The host
   serializes the scheduler and `Submit` on one per-instance lock in the
   process (ADR 0002). Under it, `Submit` first activates the instance's
   claimable runs, of any type the host serves, so a run left by a crash
   or a shutdown does not hold the instance; then it creates its run. An
   instance whose run is still unfinished after that, or whose run is
   another host's (a live lease), returns `ErrBusy`, and the caller retries
   later. Cancelling `ctx` shuts the activation down at its next step
   boundary, as for any run; the run is left runnable, and the next
   `Submit` on the instance or the scheduler finishes it.
4. **A store serves one configuration, its submitted types included.**
   The scheduler activates claimable runs of the configured type and of
   every submitted type, and leaves other types' runs alone (amends ADR
   0017, items 9 and 14). A file trigger's run keeps the instance's ID
   (ADR 0017, item 11); a submitted run's ID is its own (item 2).

### The intake agent: review runs

5. **The review agent type** is `review.Type` (`addepar-review`,
   `cmd/addepar/internal/review`), registered beside the intake agent
   and listed in `Program.Submitted`. It has code states only: `apply`
   decodes the action from the trigger payload, strictly, reads what it
   needs, and transitions to `applied` (reason `changed`) with the
   action's object operations, or to `refused` (reason `no_change`)
   putting only `refusals/<run>`: who tried what, the refusal's code and a
   sentence for the person, which the caller reads (a terminal state
   cannot set the outcome's detail, and the refusal is worth keeping for
   an auditor). An action that does not decode strictly is refused as
   `malformed`, its refusal recording who and what as far as a lenient
   reading shows. A refusal is a completed run, not a failure: it is the
   person's mistake or a stale view, not the system's. A review of a record runs on its document's
   instance (`sha256-<hex>`), so it is serialized with the intake run and
   every other review of that document.
6. **The review log is append-only, numbered, and claims its number.**
   Each action on a record is one object, `reviews/<sha>/<NN>/<MMM>` for
   record `<sha>/<NN>`, `<MMM>` counted from 001, put at expected version
   zero. The action carries `seen`, the number of entries the person was
   shown; `apply` refuses with `stale` when the log has another length,
   and a concurrent action that slips past the read loses at the commit
   (`object_conflict`, ADR 0006). Either way nothing is overwritten, and
   the person reloads. Every entry records the action, who (`by`, never
   empty), when (`at`, the submitting process's clock, refused as `clock`
   when it is before the log's last entry, since the approval's voiding is
   decided by time), why (`reason`, never empty, except for an approval),
   what changed (`change`: a correction's or manual entry's fields, old
   and new, or the entities), and the record's verification result after
   the action.
7. **The actions** (R-2 to R-4):
   - `correct`: the whole corrected extraction record, strictly decoded
     (item 13), verified again against the
     stored layout text under the document's recorded override; the entry
     holds the record and its result. A correction may fail checks: it is
     stored, and the record stays unapprovable until it passes. A
     correction of a manual record is verified as a manual record (item
     10); one that changes nothing is refused.
   - `manual`: a record a person read off the page, for a record or for
     a document with none (sent to manual review, set aside, or with no
     text layer), its reason the note. It has the extraction record's shape and
     its required fields (investor and fund as printed, period, currency,
     column, the ending balance with its page and label); the
     roll-forward lines are optional, and without a beginning balance the
     record is a partial roll-forward (check 7). The seven checks run on
     it as on any record, to catch a person's slip as well as a model's:
     a roll-forward that does not add up, a value not printed on its page.
     But a person is the source, so every failed check is an overridable
     **warning** (item 10): the record is approvable once a named person
     accepts each one, with a reason. A check that cannot run (no text
     layer to ground against) is a warning too, saying so. A malformed
     record (a missing field, an amount that is not a decimal) is still
     refused. On a document with no record, the action also puts a new
     record, `records/<sha>/<NN>` at the next number (`seen` is then the
     number of records the person was shown), whose origin is `manual`.
   - `accept_warning`: one warning of the current result, by its finding
     ID (ADR 0015, item 7), refused if there is no such warning or it is
     already accepted.
   - `set_entities`: the Addepar owner and asset, each an ID, an exact
     name or both, without surrounding spaces, or cleared (R-3).
   - `approve`: refused with `not_approvable` unless the record passes
     the exporter's gate as it stands (`export.Gate`, the one gate), so
     every approval is exportable, and with `already_approved` when it is. The entry holds the **snapshot**: the
     complete record as approved, in the exporter's shape (`export.Record`:
     extraction, result, source, owner, asset, every review action), and
     its SHA-256.
8. **A record's current state is derived.** `review.Current(record,
   entries)` folds the log over the stored record: the latest `correct`
   or `manual` gives the extraction and result, the latest `set_entities`
   the owner and asset, and every `accept_warning` since the latest
   extraction-changing entry the accepted warnings (a correction resets
   them: its warnings are new findings, even when an ID repeats). That
   ordering is the exporter's gate's own rule, so a snapshot that carries
   every review action cannot pass on an acceptance a correction voided. The record is **approved** only
   when its last entry is an approval (R-4: any later entry voids it), and
   **exported** when an export (item 9) holds that approval. The UI shows
   this state; nothing stores it.

### Export

9. **An export is a review-type run, and its snapshot is what is
   exported.** `addepar export` submits an `export` action naming record
   IDs, `by` and `at`, on its own instance, `export-<hex>` (an export spans
   documents). `apply` reads each record's log and refuses with
   `not_approved` any record whose last entry is not an approval; it then
   builds the CSV and manifest from the approval snapshots
   (`export.Build`, at `at`) and puts `exports/<run>`: the record IDs and
   approval entries, the CSV's and manifest's bytes and their SHA-256. A
   review that lands after the export read the log voids the approval for
   the future, not the export: what was exported had been approved. The
   store is the authority: the files are written after the commit, and
   can be written again from it, byte for byte.
10. **A manual record's failed checks are warnings.** For a record whose
    origin is `manual` (`export.Record.Origin`), the verifier's errors are
    reported as warnings marked `downgraded` (`verify.Result.AsWarnings`),
    each keeping its finding ID, so the exporter's gate (ADR 0015, item 9)
    passes it only once a named person has accepted every one. The gate
    refuses a downgraded finding on a record whose origin is not
    `manual`, and an unknown origin. A check that could not run because
    the document's text was not read (a scan, or a restricted PDF read
    under no acceptance: checks 3 to 6) becomes a downgraded `not_checked`
    warning, which the person accepts by confirming it from the page; a
    check skipped because the record is malformed still fails. The manifest (version 2) marks each
    row's `origin`, and each downgraded finding as such, with its
    acceptance, whose reason and the manual entry's are the notes. An extracted record,
    corrected or not, keeps its errors: its text was there to match, and
    a person who cannot make a correction pass enters a manual record,
    which says so in the manifest. The approval in the
    manifest is the snapshot's, and the manifest records the snapshot's
    SHA-256 and every review action in the log, of whatever kind.
11. **Files: the CSV in sight, the manifest hidden.** The CSV goes to the
    operator's exports folder as `addepar-<export date>-<first 8 of the
    CSV's SHA-256>.csv`; the manifest goes to a hidden folder as
    `<CSV SHA-256>-<export time, UTC, to the nanosecond>.json`, so an auditor holding only the
    CSV finds its evidence by hashing it, and a re-export of the same CSV
    bytes gets its own manifest. Neither replaces an existing file (ADR
    0015, item 9), except that an existing CSV holding exactly the export's
    bytes is kept, so writing a stored export again after a failed write
    finds what the first attempt left. `addepar export -config FILE -out
    DIR -manifests DIR (-by NAME (-record ID ... | -approved) | -rewrite
    RUN)`: `-approved` takes every approved record not yet exported;
    naming an exported record exports it again, deliberately (the same
    snapshots give the same CSV bytes, N-3); `-rewrite` writes the files of
    the export a run stored, byte for byte, for a write that failed after
    the commit (whose approvals already count as exported), and the
    failure message names it. It prints the paths and the run, and exits 0,
    1 on a refusal or failure, 2 on a usage error.
12. **The workspace** (the UI's ADR builds it): one folder an operator
    sees, holding `Inbox/` (the watched folder), `Exports/` (the CSVs) and
    the hidden `.agent/`, which holds the store (`.agent/store/`) and the
    manifests (`.agent/exports/`). Finder hides it, the folder watch skips
    it (dot entries, ADR 0017 item 16), and copying it gives an auditor
    everything: documents, attempts, records, review log, approval
    snapshots, exports, execution records and telemetry. Explicit paths
    stay available to a technical user.

13. **Extraction records decode strictly everywhere** (issue #21): a
    repeated key, or a key that matches a field only case-insensitively,
    is refused, as `internal/strictjson` refuses them in operators' files
    (ADR 0017, item 5), and so are invalid UTF-8, unknown fields, trailing
    data and a record over 1 MiB. One decoder, `statement.Decode`, serves
    `verify -record`, `submit_extraction` and the `correct` and `manual`
    actions, since a person's correction and a model's submission must
    not be read two ways; every intake tool's arguments and every review
    action decode through `strictjson` too. `strictjson` follows
    `encoding/json`'s embedded-struct rules (an untagged embedded struct's
    fields are promoted by JSON name), which the record's lines need. An
    amount that is absent or null is malformed, not zero: only `"0"` means
    zero, since a manual record's failed checks are acceptable warnings
    and nothing else would stop a missing value from being exported as 0.
    The tools' behavior changes, so the intake type's version, part of
    every tool's content identity, becomes `3` (ADR 0011, item 3): a run
    recorded under version 2 is not recovered by the stricter tools.
14. **The in-process host opens any configuration.** `standalone.Open`
    no longer refuses a trigger other than `one_shot`: `RunInput` does
    (`ErrNotOneShot`), and `Host.TriggerKind` tells a caller which it has
    (`addepar score` still refuses a folder configuration as a usage
    error). `addepar export` opens the operator's configuration, a folder
    watch's included, to submit its run; while `addepar run` watches,
    the file store's lock keeps it out, which `addepar serve` (the UI's
    ADR) resolves by being one process.

### Amendments

- **ADR 0015, item 9:** the manifest is written to the hidden manifests
  folder, named by the CSV's SHA-256, not beside the CSV (the human
  operator, 2026-11-17, after reviewing a sample), and is version 2
  (origin and snapshot hash per row); a manual record's failed checks are
  warnings (item 10, the human operator's choice the same day); an
  acceptance counts only after the latest correction or manual entry
  (item 8); the approval exported is a snapshot (item 7).
  The intake requirements' X-2 is reworded to match.
- **ADR 0017, items 9 and 14:** a store serves the configured agent type
  and the program's submitted types (item 4); **item 11:** a submitted
  run's ID is not its instance's (item 2).
- **ADR 0018, item 5:** the review type writes `reviews/`, `refusals/`, `exports/`
  and, for a manual entry on a document with none, `records/`; every
  object is still written once.
- **ADR 0020, item 2:** `standalone.Open` accepts any trigger, and
  `RunInput` refuses one other than `one_shot` (item 14).

## Consequences

- Every human action is an activation: fenced, in the execution record
  with its trigger (who, when, why, the values), and in telemetry as IDs
  and codes. An auditor reads one trail for the agent and the people.
- The extracted record is never rewritten; the reviewed record is a
  derivation, and the approved one a stored snapshot, which is exactly
  what reaches the CSV.
- A review action costs a claim, a commit and a release: milliseconds on
  the file store, and serialized per document, which the UI's single
  operator does not notice.
- Two operators acting on one record at once: one wins, the other is
  told to reload; neither overwrites.
- A manual record is checked like an extracted one, so a slip that
  breaks the roll-forward or misses the page is flagged; but every flag
  can be accepted, so a manual value is exported on a named person's
  word, with each accepted flag and its reason in the manifest.
- The headless runner still runs with no UI (acceptance criterion; N-8);
  review and export work headless too, through `Host.Submit` and
  `addepar export`.

## Not decided here

- The UI, `addepar serve`, its routes, session and workspace setup: the
  next ADR.
- Undoing an approval other than by a later action, and deleting a
  manual record: not asked for; a correction supersedes.
- Re-running extraction on a document from the UI: a new trigger
  identity, a control plane's job (ADR 0017, item 13).
- Per-document copy-restriction acceptance (#17).

## References

- Intake requirements R-1 to R-4, X-1, X-2, N-1, N-2; MVP.md step 6
- [ADR 0002](0002-serial-execution-within-an-agent-instance.md):
  serial execution per instance
- [ADR 0005](0005-storage-one-fenced-commit.md),
  [ADR 0006](0006-store-interface.md): one fenced commit, `object_conflict`
- [ADR 0015](0015-intake-verifier-exporter-and-test-data.md): the
  exporter's gate and manifest
- [ADR 0017](0017-the-standalone-host.md): the standalone host
- [ADR 0018](0018-the-intake-agent-type-and-the-vertical-slice.md): the
  intake objects
- [ADR 0020](0020-the-scoring-harness.md): the in-process host
- Issue #21: strict decoding of extraction records
