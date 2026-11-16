# 0019. The extraction agent reads the PDF

- **Status:** Accepted
- **Date:** 2026-11-16

## Context

MVP.md's step 4 is the extraction agent on the full library: visual
reading once LLM-2 and TOOL-11 land, the headless runner and the folder
trigger. Two of these are already built. The headless runner and the
folder trigger are the standalone host
([ADR 0017](0017-the-standalone-host.md)): `addepar run` processes one
statement, or watches a folder. The library already carries LLM-2 and
TOOL-11: `maestro-llms` v1.7.0 has document content parts
(`llms.Document`, #147) and non-text tool results (`ToolResult.Parts`,
#148), and `agent.Result.Parts` carries a tool's document or image parts
through the tool loop into the execution record
([ADR 0008](0008-model-states.md)).

What remains is the intake agent's own use of them. In the vertical slice
([ADR 0018](0018-the-intake-agent-type-and-the-vertical-slice.md), item
7), `get_document` returns only the layout-mode text. A model reading
text alone has to infer a table's columns and spans from spacing, which
is where extraction goes wrong (the total-fund column, a heading over two
columns). The intake requirements' `get_document` returns the PDF itself
for visual reading (TOOL-11).

The verifier stays authoritative: every value is checked in code against
the layout-mode text (ADR 0015). So the PDF helps the model understand
the page, but it never becomes evidence.

## Decision

1. **`load` stores the PDF.** Beside the stored document, `load` puts the
   input's bytes, as received, at `pdfs/<sha256>` (`Env.PutBytes`,
   expected version zero, like every object; ADR 0018 item 5), in the
   same commit as its transition. Nothing after `load` reads the input
   file again. The PDF is stored even when it is not sent (item 3) or
   goes to manual review, since the review needs it. Its size is bounded
   by the host's `max_input_bytes`.
2. **`get_document` returns the PDF and its text.** Its result is two
   parts: the PDF as an `application/pdf` document part, then a text
   part with the page count, the text-layer status and each page's
   layout-mode text, as before. It sends the PDF only if the stored bytes
   hash to the document's SHA-256; otherwise the call fails with
   `tool_error`. The prompt tells the model to use the pages to
   understand the layout (which column is the investor's, where a table
   starts and ends) and to copy every name, label, date and amount from
   the text, since code verifies against the text. Where the two seem to
   disagree, the text counts; where the text is garbled or ambiguous, the
   model asks for manual review. Sending both, not the PDF alone, was the
   human operator's decision: values read off the image would differ from
   the text layer in spacing, footnote digits and wrapped labels, failing
   checks and spending submissions, at about twice the input tokens.
3. **An oversized PDF is read through its text alone.** A PDF over
   `intake.MaxPDFPages` (100) pages or `intake.MaxPDFBytes` (20 MiB) is
   not sent. These limits sit within Anthropic's PDF limits (100 pages, a
   32 MB request), with room for the base64 encoding, the text and the
   conversation. `load` decides this and records it on the stored
   document (`reading`, `text_only`, with a note naming the limit) and in
   the run's data. `get_document` then returns the text with a line
   saying the PDF is not attached and why, and each attempt records the
   reading (`Attempt.Reading`). The statement is verified as usual; it is
   not sent to manual review for its size. This was the human operator's
   decision: a statement covering many investors can be long.
4. **The acceptance covers sending the PDF too.** Running the agent
   already accepts reading a copy-restricted statement's text (ADR 0018,
   item 3). The same implicit acceptance covers sending the restricted PDF
   itself to the model; the recorded reason says so. The human operator's
   rule is that **every permission is implicit until the gates are
   built** (approval, TOOL-4; per-document acceptance,
   [#17](https://github.com/SnapdragonPartners/maestro-agents/issues/17)).
   When they are built, this permission is to be gated along with
   reading the text: #17 covers both.
5. **No-text PDFs still go to manual review.** A scanned statement
   (`none` or `not_read` text layer) gets no model call, as before (ADR
   0018, item 2). The model could read it visually, but the verifier has
   no text to check its values against, and a value nothing checks is
   never stored as verified.
6. **The type's version is 2.** The tool's definition, the prompt and the
   stored objects' shape changed. A run started under version 1 is not
   resumed by version 2 (ADR 0011: recovery enforces each tool's content
   identity).
7. **Real statements go to a live model only on a covered route.** The
   model API agreement covering confidential financial documents (intake
   N-5) is confirmed for production. For testing, the human operator
   decided that real statements go only through Vertex AI or OpenAI,
   never the direct Anthropic API. The standalone configuration names
   OpenAI directly; Vertex still needs a `Program.Providers` entry (ADR
   0017). Synthetic statements may use any provider.
8. **Evidence.** Scripted-model tests in `cmd/addepar/internal/intake`:
   `get_document` returns the stored PDF's bytes (hashing to the
   document's SHA-256) as its first part, beside the text; a
   copy-restricted PDF is sent under the acceptance; a PDF over the page
   limit is read as text, which the result, the stored document and the
   attempt record; a stored PDF that does not match its hash fails the
   call before anything is sent; and the size decision at both limits.
   TEST-5 and TEST-6 (ADR 0018, item 9) run unchanged over the new
   commit. Live-model runs on the statements are recorded in the PR that
   implements this ADR; scoring against the answer key is MVP step 5.

## Amendments

- [ADR 0018](0018-the-intake-agent-type-and-the-vertical-slice.md) item 2:
  `load` also stores the PDF's bytes (item 1 above).
- ADR 0018 item 3: the implicit acceptance also covers sending a
  copy-restricted PDF to the model (item 4 above).
- ADR 0018 item 7: `get_document` returns the PDF beside the text, or the
  text alone over the size limit (items 2 and 3 above).
- ADR 0018, Consequences, the last bullet: the agreement is confirmed for
  production; testing on real statements uses Vertex AI or OpenAI (item 7
  above).

## Consequences

- Each run's execution record holds the PDF once per `get_document`
  call, base64-encoded, as it holds every tool result (ADR 0008), and the
  store holds it once at `pdfs/`. The store's footprint grows by the
  input's size per statement. The file store does not encrypt (ADR 0005);
  the MVP relies on the operating system's full-disk encryption.
- Input tokens per turn grow by the PDF's pages: each page is sent as an
  image and as text. Token budgets for these requests stay estimated
  (MVP.md, BUD-3: no known-safe bound for documents).
- Until #17 and approval are built, every permission a run needs is
  implicit, including reading and sending a copy-restricted PDF; each
  such document is identifiable by its recorded override.
- A provider that takes no documents in a tool result (Ollama,
  OpenAI-compatible servers) refuses the call before it is sent
  (`maestro-llms` ADR-0041), failing the run; those providers suit text
  only.

## Not decided here

- **Page images** (rendering through `maestro-cms#25`) for providers
  without PDF input, and the review UI's evidence highlighting: later
  steps.
- **Prompt caching** of the document part: an optimization for later.
- **Scoring** against the answer key (TEST-4): MVP step 5.

## References

- [MVP.md](../MVP.md), order of work step 4; requirements LLM-2, TOOL-11
- [ADR 0008](0008-model-states.md), [0011](0011-manifests.md),
  [0015](0015-intake-verifier-exporter-and-test-data.md),
  [0017](0017-the-standalone-host.md),
  [0018](0018-the-intake-agent-type-and-the-vertical-slice.md)
- `maestro-llms` v1.7.0: #147 (document parts), #148 (tool-result parts),
  ADR-0041
- The intake requirements (outside this repository): I-3, N-5, TOOL-11's
  `get_document`
- [#17](https://github.com/SnapdragonPartners/maestro-agents/issues/17)
