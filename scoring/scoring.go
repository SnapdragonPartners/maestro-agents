// Package scoring runs an agent over a set of inputs and compares its
// outputs with an answer key (TEST-4, ADR 0020): matches, mismatches,
// missed and hallucinated items, exceptions, and token use per input.
//
// The inputs run one at a time through a standalone host opened in process
// (standalone.Open). The agent type supplies a Comparator, which reads what
// each run wrote from the host's store and returns a verdict per item. A
// comparator may be exact code, as the intake agent's is, or (planned) an
// LLM judge comparing against a set of criteria.
package scoring

import (
	"context"
	"errors"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/standalone"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Verdict is a comparator's judgment on one item.
type Verdict string

// The verdicts (ADR 0020, item 3).
const (
	// Match: the output equals its answer.
	Match Verdict = "match"
	// Mismatch: the output differs from its answer.
	Mismatch Verdict = "mismatch"
	// Missed: the answer has an item the output lacks.
	Missed Verdict = "missed"
	// Hallucination: the output has an item the answer lacks, for an input
	// the answer covers.
	Hallucination Verdict = "hallucination"
	// Exception: the agent produced no answerable output for the item (the
	// run did not succeed, or the agent sent the input to a person).
	Exception Verdict = "exception"
)

// Verdicts lists the verdicts in report order.
func Verdicts() []Verdict { return []Verdict{Match, Mismatch, Missed, Hallucination, Exception} }

// Failing reports whether the verdict fails a scoring run: an output that
// is wrong rather than absent (ADR 0020, item 5).
func (v Verdict) Failing() bool { return v == Mismatch || v == Hallucination }

func (v Verdict) valid() bool {
	switch v {
	case Match, Mismatch, Missed, Hallucination, Exception:
		return true
	}
	return false
}

// Item is one compared item.
type Item struct {
	// Key names the item: an answer row, or an output the answer lacks.
	Key string `json:"key"`
	// Want is what the answer says; empty for a hallucination.
	Want string `json:"want,omitempty"`
	// Got is what the agent produced; empty when it produced nothing.
	Got     string  `json:"got,omitempty"`
	Verdict Verdict `json:"verdict"`
	// Reason says why: the fields that differ, the run's status, the
	// agent's reason for sending the input to a person.
	Reason string `json:"reason,omitempty"`
}

// Input is one input to score.
type Input struct {
	Path string `json:"path"`
}

// Output is what a comparator compares: an input's run, as the host
// reported it, and the store it wrote to.
type Output struct {
	Store     store.Reader
	Namespace string
	Input     Input
	Report    standalone.Report
}

// ErrNoAnswer is a comparator's error for an input the answer key does not
// cover: the input is reported as uncovered, with no items.
var ErrNoAnswer = errors.New("scoring: the answer key does not cover the input")

// Comparator compares one input's output with its answer.
type Comparator interface {
	Compare(ctx context.Context, out *Output) ([]Item, error)
}

// ComparatorFunc adapts a function to Comparator.
type ComparatorFunc func(ctx context.Context, out *Output) ([]Item, error)

// Compare calls f.
func (f ComparatorFunc) Compare(ctx context.Context, out *Output) ([]Item, error) { return f(ctx, out) }

// Host is what Run needs of a host: *standalone.Host.
type Host interface {
	RunInput(ctx context.Context, path string) (standalone.Report, error)
	Store() store.Reader
	Namespace() string
}

// Result is one input's score.
type Result struct {
	Input Input `json:"input"`
	// Error is the host's error running the input (the input then has no
	// items), a failure its report carries (an outcome not confirmed, a run
	// not finished), or the comparator's.
	Error string `json:"error,omitempty"`
	Items []Item `json:"items,omitempty"`
	// Report is the host's report on the input's run; zero when Error is
	// a host error.
	Report standalone.Report `json:"report"`
	// Wall is the input's wall time, run and comparison.
	Wall time.Duration `json:"wall"`
	// Uncovered is set when the answer key does not cover the input.
	Uncovered bool `json:"uncovered,omitempty"`
	// Recorded is set when the store already held the input's finished
	// run: its report is the recorded outcome, not a new run (ADR 0020,
	// item 6).
	Recorded bool `json:"recorded,omitempty"`
}

// Failed reports whether the input fails the scoring run: an error, or a
// failing verdict.
func (r *Result) Failed() bool {
	if r.Error != "" {
		return true
	}
	for _, it := range r.Items {
		if it.Verdict.Failing() {
			return true
		}
	}
	return false
}

// Totals sums a report's results.
type Totals struct {
	Verdicts map[Verdict]int `json:"verdicts"`
	// Inputs counts the inputs scored; Uncovered, Recorded and Errors
	// count among them.
	Inputs    int           `json:"inputs"`
	Uncovered int           `json:"uncovered"`
	Recorded  int           `json:"recorded"`
	Errors    int           `json:"errors"`
	Tokens    int64         `json:"tokens"`
	Turns     int           `json:"turns"`
	Wall      time.Duration `json:"wall"`
}

// Report is a scoring run's results, in input order, and their totals.
type Report struct {
	Results []Result `json:"results"`
	Totals  Totals   `json:"totals"`
	// Complete is false when the context was cancelled before every input
	// ran.
	Complete bool `json:"complete"`
}

// Failed reports whether the scoring run fails (ADR 0020, item 5): an
// input not run, an error, or a failing verdict.
func (r *Report) Failed() bool {
	if !r.Complete {
		return true
	}
	for i := range r.Results {
		if r.Results[i].Failed() {
			return true
		}
	}
	return false
}

// Run scores the inputs one at a time, in order. A host or comparator
// error on one input is recorded on it and the run goes on. Cancelling ctx
// stops after the current input (whose run the host shuts down at its next
// step boundary); the report is then incomplete. now is the clock (nil is
// time.Now).
func Run(ctx context.Context, h Host, inputs []Input, c Comparator, now func() time.Time) *Report {
	if now == nil {
		now = time.Now
	}
	rep := &Report{Results: make([]Result, 0, len(inputs)), Complete: true}
	for _, in := range inputs {
		if ctx.Err() != nil {
			rep.Complete = false
			break
		}
		rep.Results = append(rep.Results, score(ctx, h, in, c, now))
	}
	if ctx.Err() != nil {
		// Cancelled during the last input: its run was cut short.
		rep.Complete = false
	}
	rep.Totals = total(rep.Results)
	return rep
}

func score(ctx context.Context, h Host, in Input, c Comparator, now func() time.Time) Result {
	start := now()
	res := Result{Input: in}
	r, err := h.RunInput(ctx, in.Path)
	if err != nil {
		res.Error = err.Error()
		res.Wall = now().Sub(start)
		return res
	}
	res.Report, res.Recorded = r, !r.Activated && r.Status.Terminal()
	// A report can carry its own failure: an activation whose outcome was
	// not confirmed, or a run left unfinished (a shutdown). Either fails the
	// input whatever the comparator makes of it (ADR 0020, item 1).
	switch {
	case r.Error != "":
		res.Error = "run: " + r.Error
	case !r.Status.Terminal():
		res.Error = "run: not finished (status " + string(r.Status) + reasonSuffix(r.Reason.Code) + ")"
	}
	items, err := c.Compare(ctx, &Output{Store: h.Store(), Namespace: h.Namespace(), Report: r, Input: in})
	switch {
	case errors.Is(err, ErrNoAnswer):
		res.Uncovered = true
	case err != nil:
		res.Error = joinErr(res.Error, "compare: "+err.Error())
	default:
		for i := range items {
			if !items[i].Verdict.valid() {
				res.Error = joinErr(res.Error, "compare: item "+items[i].Key+" has an unknown verdict "+string(items[i].Verdict))
				items = nil
				break
			}
		}
		res.Items = items
	}
	res.Wall = now().Sub(start)
	return res
}

func reasonSuffix(code string) string {
	if code == "" {
		return ""
	}
	return ", " + code
}

func joinErr(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

func total(results []Result) Totals {
	t := Totals{Verdicts: make(map[Verdict]int, len(Verdicts()))}
	for _, v := range Verdicts() {
		t.Verdicts[v] = 0
	}
	for i := range results {
		r := &results[i]
		t.Inputs++
		t.Wall += r.Wall
		if r.Uncovered {
			t.Uncovered++
		}
		if r.Recorded {
			t.Recorded++
		}
		if r.Error != "" {
			t.Errors++
		}
		if c := r.Report.Consumed; c != nil {
			t.Tokens += c.Tokens
			t.Turns += c.Turns
		}
		for _, it := range r.Items {
			t.Verdicts[it.Verdict]++
		}
	}
	return t
}
