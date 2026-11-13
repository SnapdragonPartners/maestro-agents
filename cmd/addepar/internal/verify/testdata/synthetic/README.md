# Synthetic statements

Fictional capital account statements for the verifier's committed tests.
Each reproduces the layout and the extraction trap of one of the real
samples (A to E below), which are client data and never committed; not
even their funds' names are recorded here (this repository is
public; see ADR 0015). The real samples' tests read the gitignored
`.local/addepar/` instead.

Each `<name>.txt` is the statement's text, one line per printed line, with
pages separated by a line holding only `<<<page>>>`. The tests render it to
a PDF with `synthpdf` (monospaced, so pdftotext's layout mode reads the
columns back) and verify `<name>.json`, the case file: the hand-written
extraction record, the expected verdict and findings, and its mutations.

| Statement | Sample | Trap |
| --- | --- | --- |
| `halyard-ii` | A | cents; QTD, YTD and ITD all print the same ending |
| `brightwater-iv` | B | copy-restricted; footnote digits glued to labels; summary box; dashes; date range |
| `brightwater-ii` | C | same template, not restricted |
| `keelson-vi` | D | Limited Partner beside Total Fund; print timestamp near the period |
| `tributary-3` | E | cover page; Arabic numeral in the name; `##` glyphs; subtotal; off by 1 in rounding |
| `meridian-au` | (none) | states its currency (AUD) |
| `halyard-ii-inconsistent` | A | the ITD column prints a different ending |

No name, number or address here belongs to a real fund, investor or
administrator.
