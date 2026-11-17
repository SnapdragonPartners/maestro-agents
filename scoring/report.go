package scoring

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

// WriteJSON writes the report as indented JSON.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return fmt.Errorf("scoring: write report: %w", err)
	}
	return nil
}

// WriteText writes the report as a table, one row per input, then every
// item that is not a match, then the totals.
func (r *Report) WriteText(w io.Writer) error {
	var b strings.Builder
	if err := r.table(&b); err != nil {
		return err
	}
	for _, block := range [][]string{r.details(), r.notes()} {
		if len(block) > 0 {
			b.WriteString("\n" + strings.Join(block, "\n") + "\n")
		}
	}
	verdict := "PASS: no wrong output"
	if r.Failed() {
		verdict = "FAIL: a mismatch, a hallucination, an error or an input not run"
	}
	b.WriteString("\n" + verdict + "\n")
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("scoring: write report: %w", err)
	}
	return nil
}

// table writes a row per input and the totals.
func (r *Report) table(b *strings.Builder) error {
	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	line := func(cells []string) { _, _ = fmt.Fprintln(tw, strings.Join(cells, "\t")) } // a strings.Builder does not fail
	header := []string{"input", "status"}
	for _, v := range Verdicts() {
		header = append(header, string(v))
	}
	line(append(header, "tokens", "turns", "wall"))
	for i := range r.Results {
		res := &r.Results[i]
		counts := make(map[Verdict]int)
		for _, it := range res.Items {
			counts[it.Verdict]++
		}
		var tokens int64
		var turns int
		if c := res.Report.Consumed; c != nil {
			tokens, turns = c.Tokens, c.Turns
		}
		line(row(filepath.Base(res.Input.Path), status(res), counts, tokens, turns, res.Wall))
	}
	t := &r.Totals
	line(row("total", fmt.Sprintf("%d inputs", t.Inputs), t.Verdicts, t.Tokens, t.Turns, t.Wall))
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("scoring: write report: %w", err)
	}
	return nil
}

func row(name, status string, counts map[Verdict]int, tokens int64, turns int, wall time.Duration) []string {
	cells := []string{name, status}
	for _, v := range Verdicts() {
		cells = append(cells, fmt.Sprint(counts[v]))
	}
	return append(cells, fmt.Sprint(tokens), fmt.Sprint(turns), seconds(wall))
}

// details lists every error and every item that is not a match.
func (r *Report) details() []string {
	var out []string
	for i := range r.Results {
		res := &r.Results[i]
		name := filepath.Base(res.Input.Path)
		if res.Error != "" {
			out = append(out, fmt.Sprintf("%s: error: %s", name, res.Error))
		}
		for _, it := range res.Items {
			if it.Verdict == Match {
				continue
			}
			line := fmt.Sprintf("%s: %s %s", name, strings.ToUpper(string(it.Verdict)), it.Key)
			if it.Want != "" || it.Got != "" {
				line += fmt.Sprintf(" (want %q, got %q)", it.Want, it.Got)
			}
			if it.Reason != "" {
				line += ": " + it.Reason
			}
			out = append(out, line)
		}
	}
	return out
}

// notes says what the totals leave out.
func (r *Report) notes() []string {
	var out []string
	if n := r.Totals.Uncovered; n > 0 {
		out = append(out, fmt.Sprintf("%d input(s) not in the answer key (uncovered): run, not scored", n))
	}
	if n := r.Totals.Recorded; n > 0 {
		out = append(out, fmt.Sprintf("%d input(s) reported from runs already in the store, not run again: score a model or prompt change against a fresh store", n))
	}
	if !r.Complete {
		out = append(out, "incomplete: stopped before every input ran")
	}
	return out
}

func status(res *Result) string {
	s := string(res.Report.Status)
	switch {
	case res.Error != "" && s == "":
		s = "error"
	case s == "":
		s = "-"
	}
	if res.Recorded {
		s += " (recorded)"
	}
	if res.Uncovered {
		s += " (uncovered)"
	}
	return s
}

func seconds(d time.Duration) string { return fmt.Sprintf("%.1fs", d.Seconds()) }
