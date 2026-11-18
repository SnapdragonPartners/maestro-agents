package score

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/export"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/intake"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
	"github.com/SnapdragonPartners/maestro-agents/scoring"
	"github.com/SnapdragonPartners/maestro-agents/standalone"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Comparator is the intake agent's deterministic comparator (ADR 0020,
// item 8): each verified record against its position's answer row, by the
// hand-written mapping.
type Comparator struct {
	statements map[string][]answer
}

// answer is a mapped position and its row.
type answer struct {
	row      *Row
	investor string // verify.NameKey of the printed investor
	fund     string // verify.NameKey of the printed fund
}

// NewComparator checks that every mapped position names exactly one
// answer row.
func NewComparator(k *Key, m *Mapping) (*Comparator, error) {
	c := &Comparator{statements: make(map[string][]answer, len(m.Statements))}
	for i := range m.Statements {
		s := &m.Statements[i]
		as := make([]answer, 0, len(s.Positions))
		for _, p := range s.Positions {
			row, err := k.Lookup(p.OwnerID, p.AssetID)
			if err != nil {
				return nil, fmt.Errorf("statement %s: %w", s.SHA256, err)
			}
			as = append(as, answer{row: row, investor: verify.NameKey(p.Investor), fund: verify.NameKey(p.Fund)})
		}
		c.statements[s.SHA256] = as
	}
	return c, nil
}

// Compare scores one statement's run.
func (c *Comparator) Compare(ctx context.Context, out *scoring.Output) ([]scoring.Item, error) {
	sha := out.Report.SHA256
	answers, ok := c.statements[sha]
	if !ok {
		return nil, scoring.ErrNoAnswer
	}
	if out.Report.Status != store.StatusSucceeded {
		reason := "the run " + runState(&out.Report)
		return exceptions(answers, reason, true), nil
	}
	var o intake.Outcome
	if err := readJSON(ctx, out, intake.OutcomeKey(sha), &o); err != nil {
		return nil, err
	}
	if o.Disposition != intake.DispositionVerified {
		reason := o.Disposition
		if o.Reason != "" {
			reason += ": " + o.Reason
		}
		return exceptions(answers, reason, false), nil
	}

	return compareVerified(ctx, out, answers, o.Records)
}

// compareVerified pairs each verified record with its answer and compares
// them (ADR 0020, item 8).
func compareVerified(ctx context.Context, out *scoring.Output, answers []answer, ids []string) ([]scoring.Item, error) {
	taken := make([]bool, len(answers))
	items := make([]scoring.Item, 0, len(ids)+len(answers))
	for _, id := range ids {
		var rec export.Record
		if err := readJSON(ctx, out, intake.RecordKey(id), &rec); err != nil {
			return nil, err
		}
		if rec.Record == nil {
			return nil, fmt.Errorf("score: record %s has no extraction record", id)
		}
		r := rec.Record
		got := fmt.Sprintf("%s %s %s", r.Period.End, r.Ending.Amount, r.Currency.Code)
		name := r.Investor.Text + " / " + r.Fund.Text
		k := pair(answers, verify.NameKey(r.Investor.Text), verify.NameKey(r.Fund.Text))
		switch {
		case k < 0:
			items = append(items, scoring.Item{Key: name, Got: got, Verdict: scoring.Hallucination,
				Reason: "record " + id + " matches no position the answer key has for this statement"})
			continue
		case taken[k]:
			items = append(items, scoring.Item{Key: name, Got: got, Verdict: scoring.Hallucination,
				Reason: "record " + id + " repeats a position an earlier record took"})
			continue
		}
		taken[k] = true
		row := answers[k].row
		var diff []string
		if !r.Ending.Amount.Equal(row.Value) {
			diff = append(diff, "value")
		}
		if r.Period.End != row.Date {
			diff = append(diff, "date")
		}
		if r.Currency.Code != row.Currency {
			diff = append(diff, "currency")
		}
		it := scoring.Item{Key: row.Name(), Want: row.String(), Got: got, Verdict: scoring.Match}
		if len(diff) > 0 {
			it.Verdict = scoring.Mismatch
			it.Reason = "verified record " + id + " differs in " + strings.Join(diff, ", ") + ": a wrong value passed every check"
		}
		items = append(items, it)
	}
	for i, a := range answers {
		if !taken[i] {
			items = append(items, scoring.Item{Key: a.row.Name(), Want: a.row.String(), Verdict: scoring.Missed,
				Reason: "no verified record for this position"})
		}
	}
	return items, nil
}

// pair is the index of the answer with the investor's and fund's name
// keys, or -1.
func pair(answers []answer, investor, fund string) int {
	for i, a := range answers {
		if a.investor == investor && a.fund == fund {
			return i
		}
	}
	return -1
}

// exceptions is an exception per answer; for a statement that expects no
// record, one for the statement when the run itself did not succeed.
func exceptions(answers []answer, reason string, runFailed bool) []scoring.Item {
	if len(answers) == 0 {
		if !runFailed {
			return nil // nothing expected and nothing produced
		}
		return []scoring.Item{{Key: "(statement)", Verdict: scoring.Exception, Reason: reason}}
	}
	items := make([]scoring.Item, 0, len(answers))
	for _, a := range answers {
		items = append(items, scoring.Item{Key: a.row.Name(), Want: a.row.String(), Verdict: scoring.Exception, Reason: reason})
	}
	return items
}

// runState says how a run that did not succeed ended.
func runState(r *standalone.Report) string {
	s := "ended " + string(r.Status)
	if r.Status == "" {
		s = "did not end"
	}
	if r.Reason.Code != "" {
		s += " (" + r.Reason.Code + ")"
	}
	if r.Error != "" {
		s += ": " + r.Error
	}
	return s
}

func readJSON(ctx context.Context, out *scoring.Output, key string, v any) error {
	obj, ok, err := out.Store.Object(ctx, out.Namespace, key)
	if err != nil {
		return fmt.Errorf("score: read %s: %w", key, err)
	}
	if !ok {
		return fmt.Errorf("score: %s: %w", key, errMissing)
	}
	if err := json.Unmarshal(obj.JSON, v); err != nil {
		return fmt.Errorf("score: decode %s: %w", key, err)
	}
	return nil
}

var errMissing = errors.New("not in the store")
