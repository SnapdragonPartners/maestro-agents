package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SnapdragonPartners/maestro-agents/internal/jsonschema"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// The checks a run's start makes (ADR 0024 item 5, ADR 0025 item 4), as
// StartProblem.Check names them.
const (
	// CheckAgentType: the agent type is not registered. The start makes
	// no run failure of it: Activate refuses the run with
	// ErrNotActivatable before writing anything.
	CheckAgentType = "agent_type"
	// CheckTriggerKind: the agent type does not declare the trigger kind
	// (MAN-2).
	CheckTriggerKind = "trigger_kind"
	// CheckConfig: the configuration is not valid JSON, or the agent
	// type's JSON Schema rejects it (ADR 0011, item 8).
	CheckConfig = "config"
	// CheckBudgets: a budget is invalid, or one the runtime cannot
	// enforce (ADR 0009, ADR 0011 item 8).
	CheckBudgets = "budgets"
	// CheckToolSources: a tool source the agent type declares is missing
	// or cannot list its tools, or does not offer a served tool the type
	// grants, or offers one the record could not keep (ADR 0025, item 4).
	// It is the only check that calls out of the process, so
	// Registry.CheckStart does not make it: Registry.CheckToolSources
	// does, and the start does.
	CheckToolSources = "tool_sources"
)

// StartProblem is one reason a run's start would be refused (ADR 0024,
// item 5).
type StartProblem struct {
	// Check is the check that failed: CheckAgentType, CheckTriggerKind,
	// CheckConfig, CheckBudgets or CheckToolSources.
	Check string `json:"check"`
	// Reason is the RUN-3 reason the start would fail the run with:
	// ReasonConfigError, or ReasonBudgetUnenforceable for a budget the
	// runtime cannot enforce. It is empty for CheckAgentType, which fails
	// no run.
	Reason string `json:"reason,omitempty"`
	// Budget names the budget a CheckBudgets problem is about
	// (BudgetTokens, BudgetTurns, BudgetActiveTime or BudgetCost).
	Budget string `json:"budget,omitempty"`
	// Pointer locates a schema problem in the configuration, as a JSON
	// Pointer: empty for the whole value.
	Pointer string `json:"pointer,omitempty"`
	// Detail says what is wrong, in words for a person.
	Detail string `json:"detail"`
}

// CheckStart checks a run's start without a run (ADR 0024, item 5): the
// checks Controller.Activate makes when it starts a run of agentType on a
// trigger of triggerKind with the instance configuration config (nil for
// none) and the host's budgets (a zero token, turn or active-time budget
// taking the type's default, as at the start). It returns every problem
// it finds, in a stable order, and none if the start would pass them.
//
// It is advice: the start checks again, and remains the authority. It
// does not check the run's namespace or principal, which the host
// assigns rather than the proposal (ADR 0012 item 5, ADR 0016 item 12):
// a host checks those itself, and the start fails a run with
// ReasonConfigError for them. It does not check what only a model
// attempt shows, such as a hard token budget on a request with no
// known-safe bound (BUD-3, ADR 0014), nor a budget's deadline.
func (r *Registry) CheckStart(agentType, triggerKind string, config json.RawMessage, budgets Budgets) []StartProblem { //nolint:gocritic // hugeParam: a value, as ActivationInput carries it
	m, ok := r.lookup(agentType)
	if !ok {
		return []StartProblem{{Check: CheckAgentType, Detail: fmt.Sprintf("agent type %q is not registered", agentType)}}
	}
	b := budgets.withDefaults(&m.defaults)
	return m.checkStart(triggerKind, config, &b)
}

// checkStart is the one code path for the checks a start makes of its
// proposal, shared by the start (startRun) and Registry.CheckStart.
// budgets already carry the type's defaults.
func (m *machine) checkStart(triggerKind string, config json.RawMessage, budgets *Budgets) []StartProblem {
	var ps []StartProblem
	if !m.triggers[triggerKind] {
		ps = append(ps, StartProblem{Check: CheckTriggerKind, Reason: ReasonConfigError,
			Detail: fmt.Sprintf("trigger kind %q is not one agent type %q declares (MAN-2)", triggerKind, m.name)})
	}
	ps = append(ps, m.checkConfig(config)...)
	ps = append(ps, budgets.Problems()...)
	if budgets.Cost > 0 {
		ps = append(ps, StartProblem{Check: CheckBudgets, Reason: ReasonBudgetUnenforceable, Budget: BudgetCost,
			Detail: "cost budgets are not built yet (MVP.md, BUD-1)"})
	}
	return ps
}

// checkConfig checks the instance configuration: valid JSON, and, if the
// type has a schema, a value it accepts (the absent configuration is
// JSON null).
func (m *machine) checkConfig(config json.RawMessage) []StartProblem {
	if err := store.ValidJSON(config); err != nil {
		return []StartProblem{{Check: CheckConfig, Reason: ReasonConfigError, Detail: err.Error()}}
	}
	if m.config == nil {
		return nil
	}
	raw := config
	if raw == nil {
		raw = json.RawMessage("null")
	}
	err := m.config.Validate(raw)
	if err == nil {
		return nil
	}
	var verr *jsonschema.ValidationError
	if !errors.As(err, &verr) {
		return []StartProblem{{Check: CheckConfig, Reason: ReasonConfigError, Detail: err.Error()}}
	}
	ps := make([]StartProblem, len(verr.Problems))
	for i, p := range verr.Problems {
		ps[i] = StartProblem{Check: CheckConfig, Reason: ReasonConfigError, Pointer: p.Pointer, Detail: p.String()}
	}
	return ps
}

// refuseStart ends a start its proposal's checks refused: with
// ReasonConfigError, listing each such problem, if there is any, and
// otherwise with the first budget the runtime cannot enforce. It returns
// nil if ps is empty.
func (a *activation) refuseStart(ctx context.Context, ps []StartProblem) *ending {
	var details []string
	for _, p := range ps {
		if p.Reason == ReasonConfigError {
			details = append(details, p.String())
		}
	}
	if len(details) > 0 {
		return a.fail(ctx, ReasonConfigError, strings.Join(details, "; "))
	}
	for _, p := range ps {
		if p.Reason == ReasonBudgetUnenforceable {
			return a.refuseBudget(ctx, &budgetRefusal{p.Reason, p.Budget, p.Detail})
		}
	}
	return nil
}

// String is the problem as a run's failure detail gives it: the
// configuration's and budgets' problems prefixed with what they are about.
// It has a value receiver, so a StartProblem renders with fmt as well.
func (p StartProblem) String() string { //nolint:gocritic // hugeParam: a value receiver makes StartProblem a fmt.Stringer
	switch p.Check {
	case CheckConfig:
		return "configuration: " + p.Detail
	case CheckBudgets:
		return "budgets: " + p.Detail
	}
	return p.Detail
}
