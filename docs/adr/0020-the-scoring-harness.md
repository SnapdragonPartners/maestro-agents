# 0020. The scoring harness

- **Status:** Amended by ADR-0021; the harness (items 1 to 3 and 6) stays in force here; the intake agent's comparator and `addepar score` (items 4, 5 and 7 to 10) moved to morris-agents as [addepar ADR 0004](https://github.com/SnapdragonPartners/morris-agents/blob/main/addepar/docs/adr/0004-the-scoring-harness.md)
- **Date:** 2026-11-16

## Context

MVP.md's step 5 is the scoring harness (TEST-4) against the 3/31/2026
answer key. TEST-4 is a library requirement: "Run an agent type over a set
of inputs and compare its outputs to an answer key, reporting matches,
mismatches, exceptions and token use per input." The intake requirements'
build step 3 is its first use: run the agent on a folder of PDFs, one at
a time, compare each record to the answer key, and report matches,
mismatches, exceptions and token use per statement. The answer key is
test data, never a runtime input.

The answer key is the operators' 3/31/2026 upload, the workbook's "New
Import Template" sheet. It has the same 19 columns as the exporter's CSV
([ADR 0015](0015-intake-verifier-exporter-and-test-data.md)): owner and
asset entity IDs and names, trade date, value and currency, one row per
position. CSV is also the native format of Addepar's Import Tool API
(closed beta); the generally available Snapshots API takes JSON.

The standalone host
([ADR 0017](0017-the-standalone-host.md)) already runs one input in
process (`standalone.Run`, `run -input`) and prints its outcome, tokens
included. But that path opens and closes the store itself, so a caller
cannot read what the run wrote unless the store is a file store and the
caller reopens it.

The hard part is pairing an output with its answer. Statements name
things differently from Addepar: a statement's "Tributary Fund 3, LP" can
be "Tributary Fund III" in the sheet (a fictional example; the real
samples' names stay out of this public repository, ADR 0015). Fuzzy name matching could pair a
record with the wrong row and make a wrong value look right, which is
exactly what the harness exists to catch. And outside this agent an
answer key may not be deterministic at all.

## Decision

1. **The library's harness is the package `scoring`.** `scoring.Run`
   takes an opened standalone host, a list of inputs and a `Comparator`,
   and runs the inputs **one at a time**, in the given order, each through
   the host's one-shot path (item 2). For each input it records the run's
   report (status, reason, consumption), its wall time, and the
   comparator's items. It returns a `Report`: the inputs' results, and
   totals of every verdict, tokens, turns and wall time. A host error on
   one input (an unreadable file) is recorded on that input and the run
   goes on. So is a failure the host's report carries: an activation
   whose outcome was not confirmed (`Report.Error`), or a run left
   unfinished (a status that is not terminal, as after a shutdown); the
   comparator still gives its items, but the input is an error whatever
   they are. Cancelling the context stops after the current input and
   leaves the report incomplete, including when the current input is the
   last. The report writes as JSON or as a text table.
2. **The standalone host opens in process.** `standalone.Open(ctx,
   program, configPath, stderr)` loads and validates the configuration as
   `run` does, refuses any trigger but `one_shot`, and builds the host
   (`setup`). The returned `*Host` has `RunInput(ctx, path) (Report,
   error)`, the one-shot path `run -input` takes, now shared with it;
   `Store()`, the store as a `store.Reader`; `Namespace()`; and `Close()`.
   `standalone.Report` is the one-shot output, exported with its JSON
   unchanged. `RunInput`'s error is what `run -input` reports as a usage
   error (the input cannot be read or its run made); a run that did not
   succeed is a report, not an error. This adds to ADR 0017's API and
   changes none of it.
3. **A comparator returns verdicts on items.**
   `Comparator.Compare(ctx, *Output) ([]Item, error)`, where `Output` is
   the input (path and SHA-256), its report and the host's store and
   namespace. Each `Item` names what it is (`Key`), what the answer says
   (`Want`) and what the agent produced (`Got`), a `Verdict`, and a
   `Reason`. The verdicts:
   - `match`: the output equals its answer;
   - `mismatch`: the output differs from its answer. For a type whose
     outputs are checked in code before they leave, a mismatch is a wrong
     value that passed every check: the report flags it;
   - `missed`: the answer has an item the output lacks;
   - `hallucination`: the output has an item the answer lacks, for an
     input the answer covers;
   - `exception`: the agent produced no answerable output for an item (a
     run that did not succeed, or one the agent sent to a person).

   A comparator returns `scoring.ErrNoAnswer` for an input the answer key
   does not cover; the input is reported as `uncovered`, with its status
   and tokens, and no items.
4. **The deterministic comparator is the first of two.** The intake
   agent's comparator (item 8) is exact and code-only. The planned second
   implementation is an **LLM judge**: a model asked to compare an output
   with its answer against a set of criteria, the way buggrit's
   `internal/bench/judge.go` scores a review. It is the right tool where
   the answer key is not deterministic, and for the intake agent once the
   explicit mapping (item 7) is replaced: the judge would pair records with
   answer rows instead of a person writing the pairing. `Item` and the
   verdicts are shaped so a judge can return them (a reason per verdict).
   The judge is not built here.
5. **The exit code follows the acceptance criterion.** `addepar score`
   exits 0 when no input has a `mismatch` or a `hallucination` and every
   input ran; 1 otherwise; 2 on a usage error. `missed` and `exception`
   do not fail it: neither exports a wrong value, and the trial's target
   (85% verified with no correction) is a rate the report shows, not a
   gate. The intake requirements' first criterion, zero wrong values
   exported, is what fails it: a verified record is exportable, so a
   mismatch or a hallucination is a wrong value that would leave.
6. **A store holds an input's run once.** An input's run is keyed by its
   SHA-256 (ADR 0017), so scoring against a store that already ran an
   input reports that run's recorded outcome (`Activated` false). The
   report marks each such input `recorded`, and the text report's totals
   say how many were. Scoring a model or prompt change (intake N-7) takes
   a fresh store directory, which the harness's documentation says; it
   does not refuse a used store, since re-reporting a finished scoring run
   is useful.
7. **The intake agent's answer key and mapping.** The answer key is a CSV
   in the exporter's 19 columns: the template sheet saved as CSV once,
   kept in `.local/addepar/` with the statements. The loader finds the
   header row as the row equal to `export.Columns()` and ignores the
   rows before it (the template's title and help rows), decodes strictly
   (19 cells a row), and takes from each row the owner and asset entity
   IDs and names, the trade date (`YYYY-MM-DD`, or `M/D/YYYY` as Excel
   writes it), the value (a decimal, with or without thousands commas)
   and the currency. A row is identified by its (owner ID, asset ID); a
   mapped pair that names no row, or more than one, is a usage error.
   The **mapping** is a strictly decoded JSON file in `.local/addepar/`:
   for each statement, by SHA-256 (renaming a file changes nothing), its
   positions, each the investor and fund as the statement prints them
   and the owner and asset IDs of the answer row. A statement with an
   empty list of positions is covered and expects no record. **The
   mapping is a stopgap**, accepted for the first test only: written by
   hand, per statement, it is brittle and does not extend to new
   managers. The judge (item 4) replaces it.
8. **The intake agent's comparator.** For a statement the mapping does
   not list: `uncovered`. Otherwise it reads the outcome
   (`outcomes/<sha256>`). When the disposition is `verified`, it reads each
   of the outcome's records, pairs it with the position whose investor
   and fund have the same `verify.NameKey`s as the record's, and compares
   the record's ending amount (decimal equality), period end and currency
   code with the answer row: all equal is `match`, else `mismatch`, naming
   the fields that differ. A record that pairs with no position, or with
   a position an earlier record already took, is a `hallucination`. A
   position no record took is `missed`. When the run did not succeed or
   the disposition is not `verified` (manual review, set aside), each
   position is an `exception` whose reason is the run's status or the
   disposition and its reason. For a statement with no positions, a
   verified outcome's records are each a `hallucination`, a run that did
   not succeed is one `exception` for the statement, and any other outcome
   gives no items. The
   comparator reads only the store; it never reads the PDF.
9. **`addepar score`.** `addepar score -config FILE -inputs DIR -key
   FILE -map FILE [-json]` opens the host on the configuration, takes
   the regular `.pdf` files directly in DIR (no subfolders, dotfiles or
   symlinks, as the folder watch skips them), sorted by name, and prints
   the report. It runs under the host's signal lifecycle
   (`standalone.SignalContext`, which `standalone.Main` uses too): the
   first SIGINT or SIGTERM shuts the current run down at its next step
   boundary and the report is incomplete; a second ends the process. The
   report carries values and names (the answer's and the agent's), so on
   real statements it is client data: the command prints it to stdout for
   the operator, and it goes to telemetry and logs never.
10. **Test data.** Committed tests score synthetic statements (the
    verifier's synthetic set, rendered with `synthpdf`) with a scripted
    model, a synthetic answer key and mapping, and cover every verdict and
    the exit code. Real statements are scored only from `.local/addepar/`,
    and go to a live model only through Vertex AI or OpenAI (ADR 0019).

## Consequences

- TEST-4 is built: any agent on the standalone host can be scored by
  writing a comparator.
- The standalone host has an in-process API, so a harness or a test can
  run inputs and read the store the runs wrote, with either reference
  store.
- Scoring needs a hand-written mapping per statement until the judge is
  built; statements absent from the mapping are run and reported as
  `uncovered`, with their tokens, but not scored.
- A position the operators left out of the answer key, which the agent
  rightly extracts, is reported as a `hallucination` and fails the exit
  code; a person settles it from the report. This is the deterministic
  comparator's limit, and one of the judge's reasons to exist.

## Not decided here

- The LLM judge: its criteria, its model, and how its verdicts are
  overridden by a person (buggrit's bench has overrides).
- Running inputs in parallel; the intake requirements score one at a
  time.
- Scoring several models or prompts in one invocation (buggrit's
  experiments and arms).

## References

- [docs/requirements.md](../requirements.md), TEST-4
- [docs/MVP.md](../MVP.md), step 5
- [ADR 0015](0015-intake-verifier-exporter-and-test-data.md): the
  exporter's columns, the test-data rules
- [ADR 0017](0017-the-standalone-host.md): the standalone host
- [ADR 0018](0018-the-intake-agent-type-and-the-vertical-slice.md): the
  outcome and record objects
- [ADR 0019](0019-the-extraction-agent-reads-the-pdf.md): live models for
  real statements
- buggrit `internal/bench` (judge, report)
