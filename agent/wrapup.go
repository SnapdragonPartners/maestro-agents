package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// errWrapUp stops toolloop when the model state enters its wrap-up (ADR
// 0010): the loop runs again with the wrap-up's tools forced.
var errWrapUp = errors.New("agent: wrap-up begins")

// errWrapUpDue is the cause that cuts a model call off at the wrap-up point.
var errWrapUpDue = errors.New("agent: wrap-up point reached")

// WrapUpPoint is where a model state's wrap-up begins on each budget the
// run sets (ADR 0010). Each field applies only when its budget is set.
type WrapUpPoint struct {
	// Deadline is when the wrap-up begins by the deadline: the deadline
	// less the state's reserve.
	Deadline time.Time
	// ActiveTime is the active time at which the wrap-up begins: the
	// active-time budget less the state's reserve.
	ActiveTime time.Duration
	// Turns is the turns admitted at which the wrap-up begins: the turn
	// budget less the state's wrap-up turns.
	Turns int
}

// wrapUpPoint returns a model state's wrap-up point for the run's budgets.
func (a *activation) wrapUpPoint(ms *erasedModel) WrapUpPoint {
	b := &a.budgets
	var p WrapUpPoint
	if b.ActiveTime > 0 {
		r := ms.reserve
		if r == 0 {
			r = b.ActiveTime / 10
		}
		p.ActiveTime = max(b.ActiveTime-r, 0)
	}
	if !b.Deadline.IsZero() {
		r := ms.reserve
		if r == 0 {
			r = max(b.Deadline.Sub(a.run.CreatedAt), 0) / 10
		}
		p.Deadline = b.Deadline.Add(-r)
	}
	if b.Turns > 0 {
		p.Turns = max(b.Turns-ms.wrapTurns, 0)
	}
	return p
}

// wrapUpDue reports whether the run's consumption has reached the wrap-up
// point on any budget. Consumption only grows, so once due it stays due,
// and recovery decides as the live run did.
func (a *activation) wrapUpDue(p *WrapUpPoint) bool {
	b := &a.budgets
	return b.ActiveTime > 0 && a.activeTime() >= p.ActiveTime ||
		!b.Deadline.IsZero() && !time.Now().Before(p.Deadline) ||
		b.Turns > 0 && a.used.Turns >= p.Turns
}

// wrapUpAt returns when the wrap-up becomes due by the clocks, if the run
// has a time budget: the earlier of the active-time and deadline points.
func (a *activation) wrapUpAt(p *WrapUpPoint) (time.Time, bool) {
	var at time.Time
	ok := false
	if a.budgets.ActiveTime > 0 {
		at, ok = a.clockStart.Add(p.ActiveTime-a.activeBase), true
	}
	if !a.budgets.Deadline.IsZero() && (!ok || p.Deadline.Before(at)) {
		at, ok = p.Deadline, true
	}
	return at, ok
}

// BudgetStatus is a run's budgets and consumption at a moment, as the model
// is shown them (BUD-2, ADR 0010). String renders it as the line each tool
// result ends with; a Prompt can state it with Env.Budget.
type BudgetStatus struct {
	// At is when the status was taken.
	At time.Time
	// WrapUp is the running model state's wrap-up point; nil outside a
	// model state.
	WrapUp   *WrapUpPoint
	Budgets  Budgets
	Consumed Consumption
}

// String renders the budgets the run sets, and their consumption, for the
// model: for example "[budget: active time 4 min of 20 min used, wrap-up at
// 18 min; turns 7 of 30 used, wrap-up after 27]". It is empty when the run
// sets no budget it shows (cost budgets are not built).
func (s *BudgetStatus) String() string {
	b, c, w := &s.Budgets, &s.Consumed, s.WrapUp
	var parts []string
	if b.ActiveTime > 0 {
		p := fmt.Sprintf("active time %s of %s used", minutes(c.ActiveTime), minutes(b.ActiveTime))
		if w != nil {
			p += ", wrap-up at " + minutes(w.ActiveTime)
		}
		parts = append(parts, p)
	}
	if !b.Deadline.IsZero() {
		p := "deadline in " + minutes(b.Deadline.Sub(s.At))
		if w != nil {
			p += ", wrap-up in " + minutes(w.Deadline.Sub(s.At))
		}
		parts = append(parts, p)
	}
	if b.Turns > 0 {
		p := fmt.Sprintf("turns %d of %d used", c.Turns, b.Turns)
		if w != nil {
			p += fmt.Sprintf(", wrap-up after %d", w.Turns)
		}
		parts = append(parts, p)
	}
	if b.Tokens.Limit > 0 {
		approx := ""
		if c.TokensEstimated || b.Tokens.Estimated {
			approx = "~"
		}
		parts = append(parts, fmt.Sprintf("tokens %s%d of %d used", approx, c.Tokens, b.Tokens.Limit))
	}
	if len(parts) == 0 {
		return ""
	}
	return "[budget: " + strings.Join(parts, "; ") + "]"
}

// minutes renders a duration for the model: whole minutes, or seconds when
// short (buggrit clock.go). A negative duration renders as zero.
func minutes(d time.Duration) string {
	d = max(d, 0)
	if d < 2*time.Minute {
		return fmt.Sprintf("%d s", int(d.Round(time.Second)/time.Second))
	}
	return fmt.Sprintf("%d min", int(d.Round(time.Minute)/time.Minute))
}

// budgetStatus returns the run's budget status now; p is the running model
// state's wrap-up point, nil outside a model state.
func (a *activation) budgetStatus(p *WrapUpPoint) BudgetStatus {
	s := BudgetStatus{At: time.Now(), Budgets: a.budgets, Consumed: a.consumed()}
	if p != nil {
		w := *p
		s.WrapUp = &w
	}
	return s
}

// addClock ends a tool result with the budget status, the visible clock
// (BUD-2, TOOL-7): appended to its text, or as a final text part of a
// result with parts. The result is recorded with it, as the model receives
// it.
func (r *modelRun) addClock(res *erasedResult) {
	st := r.a.budgetStatus(&r.point)
	line := st.String()
	switch {
	case line == "":
	case len(res.parts) > 0:
		res.parts = append(res.parts[:len(res.parts):len(res.parts)], llms.Text(line))
	case res.content == "":
		res.content = line
	default:
		res.content += "\n\n" + line
	}
}

// wrapUpSpent reports whether the state has used its wrap-up turns,
// counting a wrap-up turn that pending results complete.
func (r *modelRun) wrapUpSpent(completes bool) bool {
	n := r.progress.WrapUp
	if completes && r.turn != nil && r.turn.wrapUp {
		n++
	}
	return n >= r.ms.wrapTurns
}

// wrapUpFailed fails the run when the state has used its wrap-up turns
// without a transition (LOOP-3, ADR 0010).
func (r *modelRun) wrapUpFailed(ctx context.Context) *ending {
	return r.a.fail(ctx, ReasonWrapUpFailed, fmt.Sprintf(
		"state %q: no transition after %d wrap-up turns (LOOP-3)", r.st.name, r.ms.wrapTurns))
}
