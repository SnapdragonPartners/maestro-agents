package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
)

// Budget names, as Reason.Budget carries them (RUN-3, BUD-1).
const (
	BudgetTokens     = "tokens"
	BudgetTurns      = "turns"
	BudgetActiveTime = "active_time"
	BudgetDeadline   = "deadline"
	BudgetCost       = "cost"
)

// Budgets are a run's totals (BUD-1, ADR 0009). The host passes them with
// the run's first activation; they are recorded with the run's start and
// fixed for the run, so a later activation ignores ActivationInput.Budgets.
// Zero means no limit.
type Budgets struct {
	// Deadline bounds elapsed calendar time across activations and waits
	// (BUD-2). It is enforced by the activation's context deadline, and an
	// activation that starts after it fails the run before doing any work.
	Deadline time.Time `json:"deadline,omitzero"`
	// Tokens bounds the tokens the run's model attempts consume.
	Tokens TokenBudget `json:"tokens,omitzero"`
	// Turns bounds the model turns the run is admitted: each logical call
	// to the model client counts once it is admitted, whether it then
	// succeeds, fails or is interrupted.
	Turns int `json:"turns,omitempty"`
	// ActiveTime bounds the time workers spend executing the run (BUD-2),
	// enforced by the activation's context.
	ActiveTime time.Duration `json:"active_time,omitempty"`
	// Cost bounds the run's cost in the unit of the host's Pricing
	// (BUD-4). Cost budgets are not built yet (MVP.md, BUD-1): a run that
	// sets one fails at its start with ReasonBudgetUnenforceable.
	Cost int64 `json:"cost,omitempty"`
}

// Pricing is the host's pricing function (BUD-4): the library does not
// price. Price returns the cost of usage on model in the host's unit, and
// false when the model is unpriced, which is an explicit unknown, never
// zero. It is a contract: cost budgets, which will call it, are not built
// yet (MVP.md, BUD-1).
type Pricing interface {
	Price(model ModelRef, usage Usage) (cost int64, ok bool)
}

// TokenBudget is a token budget and its kind (BUD-1). A hard budget (the
// default) is admitted only on a known-safe upper bound of each attempt's
// input; with none available it fails the run with
// ReasonBudgetUnenforceable at the first attempt (BUD-3). An estimated
// budget is admitted on maestro-llms' estimate and may overshoot by one
// attempt's error.
type TokenBudget struct {
	Limit     int64 `json:"limit,omitempty"`
	Estimated bool  `json:"estimated,omitempty"`
}

func (b *Budgets) validate() error {
	var errs []error
	if b.Tokens.Limit < 0 {
		errs = append(errs, fmt.Errorf("token budget %d is negative", b.Tokens.Limit))
	}
	if b.Tokens.Estimated && b.Tokens.Limit == 0 {
		errs = append(errs, errors.New("token budget is estimated but sets no limit"))
	}
	if b.Turns < 0 {
		errs = append(errs, fmt.Errorf("turn budget %d is negative", b.Turns))
	}
	if b.ActiveTime < 0 {
		errs = append(errs, fmt.Errorf("active-time budget %s is negative", b.ActiveTime))
	}
	if b.Cost < 0 {
		errs = append(errs, fmt.Errorf("cost budget %d is negative", b.Cost))
	}
	return errors.Join(errs...) //nolint:wrapcheck // the caller adds its context
}

// Consumption is what a run has consumed of its budgets (BUD-1, BUD-4),
// carried across activations in its checkpoints and rebuilt from the
// execution record's tail on recovery (ADR 0009).
type Consumption struct {
	// Models is the consumption per model route (BUD-4), ordered by
	// provider and name.
	Models []ModelConsumption `json:"models,omitempty"`
	// Tokens is the tokens charged: each attempt's observed usage, or its
	// reservation where usage is unknown.
	Tokens int64 `json:"tokens,omitempty"`
	// Turns is the model turns admitted.
	Turns int `json:"turns,omitempty"`
	// ActiveTime is the active time spent, as of the last record of it.
	ActiveTime time.Duration `json:"active_time,omitempty"`
	// TokensEstimated reports that Tokens includes an estimate: an attempt
	// whose usage is unknown, charged a reservation that is not a bound.
	TokensEstimated bool `json:"tokens_estimated,omitempty"`
}

// ModelConsumption is one model route's share of a run's consumption.
// InputTokens and OutputTokens are observed usage (output includes
// reasoning); Unknown counts attempts whose usage is unknown, charged
// their reservation.
type ModelConsumption struct {
	Model        ModelRef `json:"model"`
	Tokens       int64    `json:"tokens,omitempty"`
	InputTokens  int64    `json:"input_tokens,omitempty"`
	OutputTokens int64    `json:"output_tokens,omitempty"`
	Turns        int      `json:"turns,omitempty"`
	Unknown      int      `json:"unknown,omitempty"`
}

// Reservation is what a model attempt was admitted on (BUD-3), recorded in
// its model_attempt entry: Tokens, the input's size plus the output cap,
// and MaxTokens, the output cap sent (zero: none). Bound reports that the
// input's size is a known-safe upper bound rather than an estimate; until
// maestro-llms#149 supplies a bound it is always false.
type Reservation struct {
	Tokens    int64 `json:"tokens"`
	MaxTokens int   `json:"max_tokens,omitempty"`
	Bound     bool  `json:"bound,omitempty"`
}

func (c *Consumption) clone() Consumption {
	out := *c
	out.Models = slices.Clone(c.Models)
	return out
}

func (c *Consumption) model(m ModelRef) *ModelConsumption {
	i, ok := slices.BinarySearchFunc(c.Models, m, func(e ModelConsumption, m ModelRef) int {
		return cmp.Or(cmp.Compare(e.Model.Provider, m.Provider), cmp.Compare(e.Model.Name, m.Name))
	})
	if !ok {
		c.Models = slices.Insert(c.Models, i, ModelConsumption{Model: m})
	}
	return &c.Models[i]
}

// admitTurn counts an admitted turn.
func (c *Consumption) admitTurn(m ModelRef) {
	c.Turns++
	c.model(m).Turns++
}

// charge charges an attempt: its observed usage, or, when u is nil or
// reports nothing, its reservation (BUD-3, BUD-4).
func (c *Consumption) charge(m ModelRef, u *Usage, res *Reservation) {
	mc := c.model(m)
	if n := usageTokens(u); n > 0 {
		c.Tokens = addSat(c.Tokens, n)
		mc.Tokens = addSat(mc.Tokens, n)
		mc.InputTokens = addSat(mc.InputTokens, int64(u.InputTokens))
		mc.OutputTokens = addSat(mc.OutputTokens, int64(outputTokens(u)))
		return
	}
	mc.Unknown++
	if res == nil {
		// An attempt recorded before budgets were built has no
		// reservation: its consumption is unknown and uncharged.
		c.TokensEstimated = true
		return
	}
	c.Tokens = addSat(c.Tokens, res.Tokens)
	mc.Tokens = addSat(mc.Tokens, res.Tokens)
	if !res.Bound {
		c.TokensEstimated = true
	}
}

// outputTokens is billable output, reasoning included (maestro-llms
// ADR-0016).
func outputTokens(u *Usage) int {
	return max(u.BillableOutputTokens, u.OutputTokens+u.ReasoningTokens)
}

// usageTokens is the tokens an observed usage is charged: all input plus
// billable output, or the provider's total where that is larger (Gemini's
// includes tool-use prompt tokens its input does not, maestro-llms
// ADR-0034). Zero means the usage is unknown.
func usageTokens(u *Usage) int64 {
	if u == nil {
		return 0
	}
	return max(int64(u.TotalTokens), int64(u.InputTokens)+int64(outputTokens(u)))
}

func addSat(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// budgetRefusal is an attempt the budgets refuse: the run fails with code,
// naming budget.
type budgetRefusal struct {
	code, budget, detail string
}

// admit admits a model attempt against the run's budgets (BUD-1, BUD-3):
// it counts the turn, and with a token budget lowers req.MaxTokens to what
// the budget leaves. It returns the attempt's reservation, or the refusal.
// Until maestro-llms#149 lands each call is a single provider attempt
// (MVP.md, BUD-1), so a turn and an attempt are admitted together.
func (a *activation) admit(req *llms.ChatRequest) (Reservation, *budgetRefusal) {
	b := &a.budgets
	if b.Turns > 0 && a.used.Turns >= b.Turns {
		return Reservation{}, &budgetRefusal{ReasonBudgetExhausted, BudgetTurns,
			fmt.Sprintf("turn budget %d exhausted", b.Turns)}
	}
	if b.Tokens.Limit > 0 && !b.Tokens.Estimated {
		return Reservation{}, &budgetRefusal{ReasonBudgetUnenforceable, BudgetTokens,
			"hard token budget: no known-safe input bound is available (maestro-llms#149); configure the budget as estimated"}
	}
	est := middleware.DefaultEstimator{}.EstimateChat(*req)
	input := int64(est.InputTokens)
	if b.Tokens.Limit > 0 {
		left := b.Tokens.Limit - a.used.Tokens
		if input >= left {
			return Reservation{}, &budgetRefusal{ReasonBudgetExhausted, BudgetTokens, fmt.Sprintf(
				"token budget %d: %d consumed, and the next request's input is estimated at %d", b.Tokens.Limit, a.used.Tokens, input)}
		}
		if out := left - input; req.MaxTokens <= 0 || int64(req.MaxTokens) > out {
			req.MaxTokens = int(min(out, math.MaxInt32))
		}
		est = middleware.DefaultEstimator{}.EstimateChat(*req)
	}
	return Reservation{Tokens: int64(est.Tokens()), MaxTokens: req.MaxTokens}, nil
}

// clocks derives the activation's context from the run's time budgets
// (BUD-2): the deadline as a context deadline, active time as a timeout of
// what is left. Either ends the context with a *BudgetExhausted cause, which
// fails the run naming the budget. An activation that starts with either
// spent ends at once, before doing any work.
func (a *activation) clocks(ctx context.Context) (context.Context, context.CancelFunc) {
	a.clockStart = time.Now()
	a.activeBase = a.used.ActiveTime
	cancels := []context.CancelFunc{}
	if !a.budgets.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadlineCause(ctx, a.budgets.Deadline, &BudgetExhausted{Budget: BudgetDeadline})
		cancels = append(cancels, cancel)
	}
	if a.budgets.ActiveTime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, a.budgets.ActiveTime-a.activeBase, &BudgetExhausted{Budget: BudgetActiveTime})
		cancels = append(cancels, cancel)
	}
	return ctx, func() {
		for _, c := range cancels {
			c()
		}
	}
}

// consumed returns the run's consumption now: active time includes this
// activation's so far.
func (a *activation) consumed() Consumption {
	c := a.used.clone()
	if !a.clockStart.IsZero() {
		c.ActiveTime = a.activeBase + time.Since(a.clockStart)
	}
	return c
}
