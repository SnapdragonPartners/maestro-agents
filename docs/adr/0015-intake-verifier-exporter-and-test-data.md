# 0015. The intake agent's verifier, exporter and test data

- **Status:** Accepted
- **Date:** 2026-11-12

## Context

The MVP's executable is `cmd/addepar`, the capital-statement intake agent
([MVP.md](../MVP.md)). Its order of work puts the verifier and exporter
first, with no model and no store: the intake requirements' build step 1
(the document service, the seven checks, the CSV and manifest writer, and
hand-written records for the five samples that must pass while mutated
copies fail with the right reason). The intake requirements (kept outside
this repository) define the checks, the CSV and the manifest; requirements
section 6 makes the verifier, exporter and decimal handling the agent's own
code, not library requirements. So this ADR is about `cmd/addepar` only and
changes no library API.

Two facts shaped it. This repository is public, and the five samples are
client statements with real investor names and balances. And two of the
five (both from one fund administrator) are encrypted with copying
forbidden, while intake I-3 says a copy-restricted PDF goes "to manual
entry, never to OCR or restriction removal", and build step 1 expects all
five to verify.

## Decision

1. **Test data.** Real statements and their records are never committed.
   They live in `.local/addepar/` (statements in `statements/`, case files
   in `cases/`), gitignored, following Morris's `.local/` convention of one
   subdirectory per concern; `ADDEPAR_LOCAL_DIR` may point elsewhere. The
   tests over them are behind the `integration` tag and skip when the
   directory is absent. Committed tests use **synthetic** statements
   (`cmd/addepar/internal/verify/testdata/synthetic`): fictional text, one
   per sample layout and trap, rendered to PDF at test time by
   `internal/synthpdf` (a minimal text-PDF writer in Courier, so layout mode
   reads the columns back; no dependency, no committed binaries).
2. **Copy-restricted PDFs: ask the operator.** A restricted PDF's text is
   read only under an `Override`, an operator's recorded acceptance (who,
   when, why). Accepting reads the text layer and verifies the record as
   usual; declining sends the statement to manual entry. The question is
   asked **at intake, before any run starts**, so the library needs no
   input-waiting support (MVP.md: no waits for input): interactively or by
   flag on the command line, in the review UI for a folder drop (later
   PRs). The enforcement point is `document.Load`: without an override it
   never runs pdftotext on a restricted PDF, and the verifier fails
   grounding with `text_not_read`. The override is kept on the document and
   written to the manifest. The service never modifies or re-saves the
   PDF. This amends intake I-3 to: *route to manual entry unless an
   operator explicitly accepts reading the text layer; that acceptance is
   recorded*. The intake requirements document is to be updated to match.
   Restriction is read from `pdfinfo`'s report; an encrypted PDF whose copy
   permission is unstated counts as restricted.
3. **Fail closed.** The verifier establishes every fact from the page;
   where it cannot (the evidence is ambiguous, across pages, or only the
   record asserts it), that is an error, or a warning a person must accept,
   never a pass. Points 4 to 6 apply this rule.
4. **Text in layout mode, read as a character grid.** Pages come from
   `maestro-cms` v0.9.0's `pdftotext` engine with `WithLayout`, called
   directly (its `Extractor` normalizes whitespace and would erase the
   columns). The verifier reads each line as words with their character
   columns. An amount is grounded when it appears in the **run** after its
   label, and the label is the whole printed label (it starts the line or
   follows a cell, never another word, so "Contributions" does not match
   "Less: contributions"); the run is the cells (amounts, dashes, percentages) following the label up
   to the next word with letters; a cell stands at least two spaces from
   the label, so a label cut short ("Balance, March 31") does not adopt the
   year as a cell. A column is a right edge the roll-forward's amounts
   share within 4 characters (measured on the samples: within about 3 in a
   column, about 20 between columns); when edges tie (QTD and YTD print the
   same figures in a first quarter), the one under the record's own header
   wins. The table is bound from the page: its column header row is the
   first header row (two or more phrases, no cells; one phrase only in a
   one-column table) within six lines above its first row with no table
   row between, and the header nearest the edge on that row must be the
   record's column label, matched whole. A heading anywhere else on the
   page, or prose containing the label, confirms nothing. A column is verified only on the amounts' own page: a column
   header, or a roll-forward amount, cited on another page than the ending
   fails (`column_unverified`). The ending must be the closing balance,
   established from the printed table rather than from what was submitted:
   its label not an opening balance's, its row below the beginning and
   every movement, no row after it printing a cell in the column before
   the table ends at the first line with no cells, and no later row in the
   column, past any heading, whose amount is the ending plus the rows
   between (the roll-forward continuing) (`ending_not_closing`); and every row between the beginning and the
   ending that prints a cell in the column must be submitted
   (`row_not_submitted`). Names (investor, fund, period label,
   currency evidence) are matched keeping every digit; only labels and
   headers drop a glued footnote reference.
5. **Check 4 is decided from the page, not the record's claim.** The total
   fund column passes checks 1, 2, 3 and 5 when read consistently, so only
   check 4 stands between it and a wrong value. Among the four printed
   lines above the table's first row, a line with no cells that names the
   fund total, the general partner or all partners groups the ending line's
   columns: in order when they divide evenly among its phrases, otherwise
   each column to the nearest phrase; an ending under a fund phrase fails.
   Any other such heading above the table (one phrase, beyond those four
   lines, or nearer the table than the group row) leaves the table's
   ownership unverified, which fails (`fund_table`). A heading is a line
   whose numbers and dashes, if any, sit inside lettered phrases ("Total
   Fund (Including GP) - Class 1"); a line with a cell standing alone is a
   data row. The record's claimed section plays no part in finding
   the groups. When a total-over-all-partners header is printed, an
   ending that is the largest of the distinct balances on its line fails
   too. A declared `fund_total` must be larger than the value, and a
   column label or section that names the fund fails outright.
6. **Readings of the rules where the intake text is silent.** Check 2
   counts the beginning balance and each movement as "lines summed" for
   the whole-unit tolerance; subtotals (`kind: subtotal`) are grounded but
   not summed; the claimed precision must match how the ending is printed.
   A record with no roll-forward when the page prints a beginning balance
   fails check 2 (`roll_forward_omitted`) rather than earning check 7's
   warning, so omitting the roll-forward cannot turn an error into a
   warning. Check 3 finds the ending's copies itself: every other
   occurrence in the document of the ending's label (including one sharing
   the ending's layout line) must agree, an error on a page
   the record cites or that names the investor, else a warning (it may be
   another investor's statement in the same PDF, E-4). Check 5 accepts a label with a period phrase, a date range, or
   a date standing alone on its line, and rejects a date with a time of
   day or a word such as "printed"; another period end under a strong
   period phrase anywhere in the document fails it, as does a period line
   carrying more than one date ("as of" dates do not count). Check 6 fails, rather than warns, when the statement states a
   currency other than the record's: a wrong currency is a wrong row. A
   dash is zero only when the record states the table's convention
   (`dash_zero`).
7. **Findings are addressable.** Each finding has an ID, check/code/subject
   (`6/not_stated/currency`), that a warning's acceptance names. A record
   change reruns verification and voids every acceptance and the approval
   (R-4); the ID excludes the message so rewording one does not.
8. **Decimals.** A local exact decimal (`internal/decimal`, a `big.Int`
   coefficient and a scale) keeps the precision printed; amounts are JSON
   strings, and a JSON number is refused (N-4).
9. **The exporter is the last gate.** It refuses a record with no named
   approver, an approval older than a later review action, a verification
   error, an unaccepted warning, or a text cell beginning with a formula
   character. Rows sort by owner, asset, date and currency, so the same
   records give the same CSV bytes in any order (N-3). CSV: the template's
   19 columns, a header row, CRLF line ends (RFC 4180), UTF-8 without BOM,
   `Value Based` ownership, values in their printed precision, Trade Date
   as `YYYY-MM-DD` (one constant, pending the firm's Addepar date setting).
   A row with no entered Addepar IDs carries the statement's names and is
   flagged `names_only` in the manifest (R-3). The manifest (version 1)
   holds the CSV's SHA-256 and, per row, the document's hash, restriction
   and override, the exported extraction record (the lines used, each with
   label, amount and page) and its hash, the seven checks, every review
   action and the approval. `Write` writes the manifest before the CSV and
   never replaces an existing export.

## Consequences

- All five real samples verify (with the currency warning a person must
  accept; one also with a $1 rounding warning, which the statement
  itself notes), and every mutation fails its check: a digit changed, the
  total fund column (check 4 alone), the print timestamp as the period
  (check 5), and the generic and per-case mutations in both suites.
- Hosting `cmd/addepar` needs Poppler (`pdftotext`, `pdfinfo`). CI installs
  `poppler-utils` and sets `ADDEPAR_REQUIRE_POPPLER`, which turns the PDF
  tests' skip into a failure.
- `maestro-cms` becomes a dependency (first-party; module pruning keeps its
  cloud dependencies out of this module's build) and raises the `go`
  directive to 1.26.6, which consumers of this module inherit.
- `pdfinfo` is run locally because `maestro-cms` does not report
  encryption; if it gains that, the probe moves there.
- The checks rely on layout mode keeping a row on one line. A label that
  wraps onto a second line cannot be grounded; the extractor (a later PR)
  must then send the statement to manual review.

## References

- [MVP.md](../MVP.md), order of work step 3; requirements section 6
- The intake requirements (outside this repository): I-1 to I-3, E-2,
  E-3, V-1 and the seven checks, R-2 to R-4, X-1, X-2, N-1 to N-5
- `cmd/addepar/internal/verify/testdata/synthetic/README.md`
