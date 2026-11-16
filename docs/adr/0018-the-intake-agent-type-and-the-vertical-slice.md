# 0018. The intake agent type and the vertical slice

- **Status:** Accepted
- **Date:** 2026-11-16

## Context

MVP.md's step 2 is the vertical slice, requirements section 6 steps 1 to
5: read a statement through its layout-mode text, extract a record, verify
it in code and commit it durably; interrupt the activation after the
commit and before its acknowledgement and continue it, with nothing
duplicated or lost (TEST-5); and run both TEST-6 interleavings against the
submission on the file store. The verifier and exporter exist
([ADR 0015](0015-intake-verifier-exporter-and-test-data.md)), and so does
the standalone host ([ADR 0017](0017-the-standalone-host.md)). The library
proves TEST-5 and TEST-6 on a generic agent in `store/storetest`; the slice
proves them on the intake agent itself, whose writes are the ones that
matter.

The intake requirements (kept outside this repository) give the agent four
tools (`get_document`, `submit_extraction`, `set_aside`,
`request_manual_review`), a resubmission cap (E-6), one record per
investor and fund (E-4), and say a run ends when every statement is
verified, set aside or sent to manual review. This ADR decides how the
agent type realizes them. It is about `cmd/addepar` only and changes no
library API.

## Decision

1. **The agent type** is `intake.Type` (`cmd/addepar/internal/intake`),
   named `addepar-intake`, run by `addepar run` and `addepar config
   validate` through `standalone.Main`; `addepar verify` stays. It accepts
   the `one_shot` and `folder` triggers. Its states are `load` (code),
   `extract` (model) and the terminal states `verified`, `set_aside`,
   `manual_review` (each succeeding with `changed`) and `duplicate`
   (`no_change`). Its default budgets are 16 turns, 10 minutes of active
   time and an estimated 600,000 tokens; its configuration has one
   optional key, `max_attempts`.
2. **`load` stores the document.** It reads the input with
   `FileTrigger.Read`, loads it with `document.Load` (layout mode) and
   commits the document, with each page's text, with its transition, so
   no later step or recovery runs Poppler again. A document already stored
   under the same hash goes to `duplicate` and writes nothing (intake I-2;
   the standalone host already runs a file once per store, but another
   host may not). A document with no usable text layer goes to
   `manual_review` without a model call (I-3). A file `document.Load`
   cannot read fails the run with `state_error`.
3. **Copy-restricted PDFs are read under an implicit acceptance, for
   now.** Running the agent is the operator's acceptance: `load` passes
   `document.Load` an `Override` whose `by` is the run's principal (or
   `operator`), whose `at` is the load time and whose reason cites
   [#17](https://github.com/SnapdragonPartners/maestro-agents/issues/17).
   `Load` applies it only to a restricted PDF, and it is recorded on the
   stored document and carried into each record's source and the
   manifest, as ADR 0015 item 2 requires. This replaces, until #17 is
   built, item 2's per-document question at intake; the decision was the
   human operator's, for testing.
4. **One submission holds every record.** `submit_extraction` takes
   `{"records": [...]}`, one record per investor and fund (E-4). Each is
   decoded and verified on its own (ADR 0015). The submission passes when
   no record is malformed or has an error; warnings pass, for a person to
   accept. A record that may denote the same position as an earlier one is
   refused before it is verified. Names are no identity: grounding accepts
   any contiguous run of a printed name's words, compared as the verifier
   compares names (`verify.NameKey`: case, typographic punctuation, spacing
   and trailing commas folded, every digit kept). So two records are one
   position when each one's investor words run inside the other's, or
   contain them, and likewise their funds. Names alone cannot settle it:
   on a table printed again on another page, disjoint fragments
   ("Example", "Holdings IV") each ground against the one printed name,
   and the page's words do not mark where a name starts or ends, so no
   fragment resolves to a full identity. So the value decides as well: two
   records are also one position when their endings are within one unit of
   each other (a repeat may be printed rounded), whatever names, labels and
   pages they claim. The rule errs toward "same": distinct positions it
   cannot tell apart (nested names such as "Trust" and "Trust II", two
   positions whose balances are within a unit) are refused, so
   the statement fails closed into a correction or manual review and is
   never stored twice. Whether a record's investor is the one its column
   belongs to is the verifier's question (checks 1 and 4), not this
   rule's.
5. **Every write is an atomic local transaction, written once.** The three
   writing tools are `ContractAtomicLocal` (ADR 0005 items 7 to 10): they
   read, verify and return object operations that commit with the call's
   result, and with its transition. Every object is put at expected
   version zero, so a write that would replace one conflicts rather than
   overwriting. In the run's namespace, keyed by the document's SHA-256:
   - `documents/<sha>`: the document and its pages' text, written by `load`;
   - `attempts/<sha>/<NN>`: every submission, as submitted, with its
     records' check results and the run, step and call that made it, whose
     execution record holds the model, prompt and transcript (N-2);
   - `records/<sha>/<NN>`: each verified record, in the shape the exporter
     reads (`export.Record`, source `extracted`, no approval);
   - `outcomes/<sha>`: the disposition (`verified`, `set_aside`,
     `manual_review`), its reason, the records' IDs and the attempt count.
     A writing tool refuses a document that already has one.
6. **The cap is three submissions** (`intake.MaxAttempts`), and the
   configuration may lower it, never raise it. Attempts are numbered by
   counting the stored ones. A failed submission returns its errors with
   their evidence for the model to correct (E-6); the failure of the last
   allowed submission, or of one made in a wrap-up turn (ADR 0010), stores
   the `manual_review` outcome and ends the run, so the extract state
   always has an exit. `set_aside` and `request_manual_review` are the
   other exits; all three are the state's wrap-up tools.
7. **`get_document` returns text only.** It reads the stored document.
   Sending the PDF itself to the model (LLM-2, TOOL-11) comes with the
   extraction agent (MVP.md step 4), as does any live-model acceptance.
8. **The prompt is two templates** (`prompts/system.tmpl`,
   `prompts/task.tmpl`), listed in the manifest with their identities, so
   the prompt version of every run is its recorded manifest digest
   (MAN-4).
9. **The slice's evidence** is in `cmd/addepar/internal/intake`, on
   synthetic statements with a scripted model: extraction refused for the
   total-fund column by check 4 and then verified (steps 1 and 2); TEST-5,
   the activation stopped at every commit, before and after it lands, and
   continued after reopening the file store, with exactly the expected
   attempts, one set of records and every turn recorded once (steps 3 and
   4); and TEST-6, activation A paused in its verified submission's commit
   at both pause points while B takes over, accepting either
   serialization, with the submission stored once and nothing of A's
   landing after B's transfer (step 5). The integration test runs the
   five real samples through the agent with their hand-written records.

## Amendments

- [ADR 0015](0015-intake-verifier-exporter-and-test-data.md) item 2: until
  #17 is built, a copy-restricted PDF's acceptance is the operator's
  running the agent (item 3 above), recorded as an `Override` as item 2
  requires, rather than a question asked per document at intake. The
  enforcement point (`document.Load` reads no restricted text without an
  override) is unchanged.

## Consequences

- `cmd/addepar` is now a standalone executable: an operator runs one
  statement, or watches a folder, with a configuration naming the model
  (`cmd/addepar/config.example.json`: `anthropic`, `claude-opus-5-5`). No
  model is chosen by default (ADR 0017).
- Until #17 is built, every copy-restricted statement a run receives is
  read, including any dropped into a watched folder; each is identifiable
  by its recorded override.
- The records the agent stores are what the review UI and the exporter
  will read; approval and export remain human actions outside the agent.
- Real statements are sent to a live model only once the model API
  agreement is confirmed to cover them (intake N-5; an open question in
  the intake requirements).

## References

- [MVP.md](../MVP.md), order of work step 2; requirements section 6
- [ADR 0005](0005-storage-one-fenced-commit.md),
  [0010](0010-wrap-up-and-the-visible-clock.md),
  [0015](0015-intake-verifier-exporter-and-test-data.md),
  [0017](0017-the-standalone-host.md)
- The intake requirements (outside this repository): I-1 to I-3, E-1 to
  E-6, V-1, N-2, N-5
- [#17](https://github.com/SnapdragonPartners/maestro-agents/issues/17):
  per-document acceptance of copy-restricted PDFs
