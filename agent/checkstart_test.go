package agent_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

const checkSchema = `{"type":"object","required":["model"],"additionalProperties":false,
	"properties":{"model":{"type":"string","minLength":1}}}`

// checkType is the counter type with a configuration schema and default
// budgets.
func checkType() *agent.Type[counterData] {
	typ := counterType()
	typ.Config = json.RawMessage(checkSchema)
	typ.Budgets = agent.Budgets{Turns: 5, Tokens: agent.TokenBudget{Limit: 1000}}
	return typ
}

type startCase struct {
	name, kind, config string
	budgets            agent.Budgets
	want               []agent.StartProblem
}

func cfgProblem(pointer, detail string) agent.StartProblem {
	return agent.StartProblem{Check: agent.CheckConfig, Reason: agent.ReasonConfigError, Pointer: pointer, Detail: detail}
}

func budgetProblem(budget, detail string) agent.StartProblem {
	return agent.StartProblem{Check: agent.CheckBudgets, Reason: agent.ReasonConfigError, Budget: budget, Detail: detail}
}

var costUnenforceable = agent.StartProblem{Check: agent.CheckBudgets, Reason: agent.ReasonBudgetUnenforceable,
	Budget: agent.BudgetCost, Detail: "cost budgets are not built yet (MVP.md, BUD-1)"}

func startCases() []startCase {
	const good = `{"model":"m"}`
	return []startCase{
		{name: "passes", kind: agent.TriggerOneShot, config: good},
		{name: "defaults fill zero budgets", kind: agent.TriggerOneShot, config: good, budgets: agent.Budgets{ActiveTime: time.Minute}},
		{name: "undeclared kind", kind: agent.TriggerSchedule, config: good, want: []agent.StartProblem{{
			Check: agent.CheckTriggerKind, Reason: agent.ReasonConfigError,
			Detail: `trigger kind "schedule" is not one agent type "counter" declares (MAN-2)`,
		}}},
		{name: "no kind", kind: "", config: good, want: []agent.StartProblem{{
			Check: agent.CheckTriggerKind, Reason: agent.ReasonConfigError,
			Detail: `trigger kind "" is not one agent type "counter" declares (MAN-2)`,
		}}},
		{name: "invalid JSON", kind: agent.TriggerOneShot, config: `{`, want: []agent.StartProblem{
			{Check: agent.CheckConfig, Reason: agent.ReasonConfigError, Detail: "store: invalid request: value is not valid JSON"},
		}},
		{name: "schema rejects", kind: agent.TriggerOneShot, config: `{"model":"","extra":1}`, want: []agent.StartProblem{
			cfgProblem("/extra", "/extra: is not an allowed property"),
			cfgProblem("/model", "/model: must be at least 1 characters long"),
		}},
		{name: "absent configuration", kind: agent.TriggerOneShot, want: []agent.StartProblem{
			cfgProblem("", "(root): must be object, not null"),
		}},
		{name: "invalid budgets", kind: agent.TriggerOneShot, config: good,
			budgets: agent.Budgets{Tokens: agent.TokenBudget{Limit: -1}, Turns: -2, ActiveTime: -time.Second, Cost: -3},
			want: []agent.StartProblem{
				budgetProblem(agent.BudgetTokens, "token budget -1 is negative"),
				budgetProblem(agent.BudgetTurns, "turn budget -2 is negative"),
				budgetProblem(agent.BudgetActiveTime, "active-time budget -1s is negative"),
				budgetProblem(agent.BudgetCost, "cost budget -3 is negative"),
			}},
		// An estimated budget with no limit takes no default (ADR 0011,
		// item 7): the host set the token budget's kind.
		{name: "estimated, no limit", kind: agent.TriggerOneShot, config: good,
			budgets: agent.Budgets{Tokens: agent.TokenBudget{Estimated: true}},
			want:    []agent.StartProblem{budgetProblem(agent.BudgetTokens, "token budget is estimated but sets no limit")}},
		{name: "cost unenforceable", kind: agent.TriggerOneShot, config: good, budgets: agent.Budgets{Cost: 100},
			want: []agent.StartProblem{costUnenforceable}},
		// Every problem is listed; the start fails with config_error,
		// which outranks the unenforceable budget.
		{name: "everything at once", kind: agent.TriggerSchedule, config: `{}`, budgets: agent.Budgets{Turns: -1, Cost: 1},
			want: []agent.StartProblem{
				{Check: agent.CheckTriggerKind, Reason: agent.ReasonConfigError,
					Detail: `trigger kind "schedule" is not one agent type "counter" declares (MAN-2)`},
				cfgProblem("", `(root): missing required property "model"`),
				budgetProblem(agent.BudgetTurns, "turn budget -1 is negative"),
				costUnenforceable,
			}},
	}
}

func rawConfig(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	return json.RawMessage(s)
}

// TestCheckStart: each kind of problem the start checks is reported, all
// of them at once, as data (ADR 0024, item 5).
func TestCheckStart(t *testing.T) {
	r := registry(t, checkType())
	for _, tc := range startCases() {
		t.Run(tc.name, func(t *testing.T) {
			got := r.CheckStart("counter", tc.kind, rawConfig(tc.config), tc.budgets)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("CheckStart =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

// TestCheckStartAgreesWithTheStart: a start refused for its proposal fails
// the run exactly as CheckStart predicts: config_error listing each
// configuration problem if there is any, else the unenforceable budget;
// a start CheckStart passes is not refused.
func TestCheckStartAgreesWithTheStart(t *testing.T) {
	for _, tc := range startCases() {
		t.Run(tc.name, func(t *testing.T) {
			s := newBackend(t, "memstore").open()
			if _, _, err := s.CreateRun(context.Background(), store.Run{
				ID: "r1", Instance: inst, AgentType: "counter", TriggerID: "t1", TriggerKind: tc.kind, Trigger: json.RawMessage(`{"target":1}`),
			}); err != nil {
				t.Fatal(err)
			}
			r := registry(t, checkType())
			out, err := agent.NewController(s, r).Activate(context.Background(), agent.ActivationInput{
				Fence: claim(t, s), Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}, Namespace: ns,
				Config: rawConfig(tc.config), Budgets: tc.budgets,
			})
			if err != nil {
				t.Fatal(err)
			}
			problems := r.CheckStart("counter", tc.kind, rawConfig(tc.config), tc.budgets)
			if len(problems) == 0 {
				if out.Status != store.StatusSucceeded {
					t.Fatalf("CheckStart passed, but the run ended %+v", out)
				}
				return
			}
			var config []string
			for i := range problems {
				if problems[i].Reason == agent.ReasonConfigError {
					config = append(config, problems[i].String())
				}
			}
			switch {
			case len(config) > 0:
				if out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonConfigError || out.Detail != strings.Join(config, "; ") {
					t.Fatalf("outcome = %+v, want config_error with detail %q", out, strings.Join(config, "; "))
				}
			default:
				p := problems[0]
				wantBudgetFailure(t, out, p.Reason, p.Budget)
				if out.Detail != p.Detail {
					t.Fatalf("detail = %q, want %q", out.Detail, p.Detail)
				}
			}
		})
	}
}

// TestCheckStartUnknownType: an unregistered agent type is one problem,
// with no reason, since Activate refuses such a run without failing it.
func TestCheckStartUnknownType(t *testing.T) {
	got := registry(t, checkType()).CheckStart("nope", agent.TriggerOneShot, nil, agent.Budgets{})
	want := []agent.StartProblem{{Check: agent.CheckAgentType, Detail: `agent type "nope" is not registered`}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CheckStart = %+v, want %+v", got, want)
	}
}

// TestStartProblemJSON: a problem's wire form, for consumers that check a
// start across a process boundary (ADR 0024: JSON at every seam).
func TestStartProblemJSON(t *testing.T) {
	for _, tc := range []struct {
		p    agent.StartProblem
		want string
	}{
		{cfgProblem("/model", "/model: must be string, not number"),
			`{"check":"config","reason":"config_error","pointer":"/model","detail":"/model: must be string, not number"}`},
		{costUnenforceable,
			`{"check":"budgets","reason":"budget_unenforceable","budget":"cost","detail":"cost budgets are not built yet (MVP.md, BUD-1)"}`},
		{agent.StartProblem{Check: agent.CheckAgentType, Detail: "x"}, `{"check":"agent_type","detail":"x"}`},
	} {
		got, err := json.Marshal(tc.p)
		if err != nil || string(got) != tc.want {
			t.Errorf("Marshal = %s, %v; want %s", got, err, tc.want)
		}
	}
}
