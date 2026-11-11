package agent_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

func manifestOf(t *testing.T, typ agent.Definition) agent.Manifest {
	t.Helper()
	m, ok := registry(t, typ).Manifest(typ.TypeName())
	if !ok {
		t.Fatalf("no manifest for %q", typ.TypeName())
	}
	return m
}

func toolOf(t *testing.T, m *agent.Manifest, name string) agent.ToolManifest {
	t.Helper()
	for _, tool := range m.Tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("manifest has no tool %q", name)
	return agent.ToolManifest{}
}

func TestManifest(t *testing.T) {
	typ := ledgerType(&execLog{})
	typ.Config = json.RawMessage(`{ "type": "object" }`)
	typ.Budgets = agent.Budgets{Turns: 20}
	r := registry(t, typ)
	m, ok := r.Manifest("ledger")
	if !ok {
		t.Fatal("no manifest")
	}
	if m.Format != agent.ManifestFormat || m.Name != "ledger" || m.Version != "1" || m.Initial != "work" ||
		!slices.Equal(m.Triggers, []string{agent.TriggerOneShot}) || string(m.Config) != `{"type":"object"}` ||
		m.Budgets == nil || m.Budgets.Turns != 20 {
		t.Fatalf("manifest = %+v", m)
	}
	wantStates := []agent.StateManifest{
		{Name: "work", Kind: agent.StateKindModel, Next: []string{"review", "work"}, Tools: []string{"lookup", "record", "finish"}, WrapUp: []string{"finish"}},
		{Name: "review", Kind: agent.StateKindCode, Next: []string{"done", "rejected"}},
		{Name: "done", Kind: agent.StateKindTerminal, End: agent.EndChanged},
		{Name: "rejected", Kind: agent.StateKindTerminal, End: agent.EndInvalidOutput},
	}
	if len(m.States) != len(wantStates) {
		t.Fatalf("states = %+v", m.States)
	}
	for i, want := range wantStates {
		got := m.States[i]
		if got.Name != want.Name || got.Kind != want.Kind || got.End != want.End || !slices.Equal(got.Next, want.Next) ||
			!slices.Equal(got.Tools, want.Tools) || !slices.Equal(got.WrapUp, want.WrapUp) {
			t.Errorf("state %d = %+v, want %+v", i, got, want)
		}
	}
	if len(m.Tools) != 3 {
		t.Fatalf("tools = %+v", m.Tools)
	}
	record := toolOf(t, &m, "record")
	if record.Effect != agent.EffectWrite || record.Contract != agent.ContractAtomicLocal || record.Version != "1" ||
		string(record.InputSchema) != `{"type":"object"}` || len(record.Identity) != 64 {
		t.Fatalf("record = %+v", record)
	}
	// The digest is the SHA-256 of the manifest's JSON form.
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if digest, _ := r.ManifestDigest("ledger"); digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest = %s, want the SHA-256 of %s", digest, b)
	}
	// The manifest is the caller's own copy.
	m.States[0].Tools[0] = "changed"
	m.Tools[0].Name = "changed"
	if again, _ := r.Manifest("ledger"); again.States[0].Tools[0] != "lookup" || again.Tools[0].Name != "lookup" {
		t.Fatalf("registry's manifest changed: %+v", again)
	}
	if _, ok := r.Manifest("missing"); ok {
		t.Fatal("manifest for an unregistered type")
	}
	if _, ok := r.ManifestDigest("missing"); ok {
		t.Fatal("digest for an unregistered type")
	}
}

// A tool's identity covers its definition, effect, contract and version,
// and changes whenever any of them does (MAN-1, ADR 0011 item 3).
func TestToolIdentity(t *testing.T) {
	identity := func(typeVersion string, mutate func(*agent.Tool[ledgerData])) string {
		t.Helper()
		typ := ledgerType(&execLog{})
		typ.Version = typeVersion
		mutate(&typ.States[0].Model.Tools[0])
		m := manifestOf(t, typ)
		return toolOf(t, &m, "lookup").Identity
	}
	same := func(*agent.Tool[ledgerData]) {}
	base := identity("1", same)
	changed := map[string]func(*agent.Tool[ledgerData]){
		"description": func(tool *agent.Tool[ledgerData]) { tool.Definition.Description = "Look a key up." },
		"schema": func(tool *agent.Tool[ledgerData]) {
			tool.Definition.InputSchema = json.RawMessage(`{"type":"object","required":["key"]}`)
		},
		"effect": func(tool *agent.Tool[ledgerData]) {
			tool.Effect, tool.Contract = agent.EffectWrite, agent.ContractIdempotent
		},
		"version": func(tool *agent.Tool[ledgerData]) { tool.Version = "2" },
	}
	for name, mutate := range changed {
		if identity("1", mutate) == base {
			t.Errorf("changing the %s left the identity unchanged", name)
		}
	}
	unchanged := map[string]func(*agent.Tool[ledgerData]){
		"schema formatting": func(tool *agent.Tool[ledgerData]) {
			tool.Definition.InputSchema = json.RawMessage(" { \"type\" : \"object\" } ")
		},
		"type's version declared": func(tool *agent.Tool[ledgerData]) { tool.Version = "1" },
		"execute": func(tool *agent.Tool[ledgerData]) {
			tool.Execute = func(context.Context, *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
				return agent.TextResult[ledgerData]("other"), nil
			}
		},
	}
	for name, mutate := range unchanged {
		if identity("1", mutate) != base {
			t.Errorf("changing the %s changed the identity", name)
		}
	}
	// A tool with no version of its own takes the type's.
	if identity("2", same) == base {
		t.Error("a new type version left an unversioned tool's identity unchanged")
	}
	pinned := func(tool *agent.Tool[ledgerData]) { tool.Version = "7" }
	if identity("1", pinned) != identity("2", pinned) {
		t.Error("a new type version changed the identity of a tool with its own version")
	}
	// Unversioned: neither the tool nor the type declares a version.
	typ := ledgerType(&execLog{})
	typ.Version = ""
	m := manifestOf(t, typ)
	if v := toolOf(t, &m, "lookup").Version; v != "" {
		t.Fatalf("unversioned tool's version = %q", v)
	}
}

func TestRegisterRefusesManifest(t *testing.T) {
	mutate := func(f func(*agent.Type[ledgerData])) *agent.Type[ledgerData] {
		typ := ledgerType(&execLog{})
		f(typ)
		return typ
	}
	// secondState adds a model state granting tool, beside work's tools.
	secondState := func(tool agent.Tool[ledgerData]) func(*agent.Type[ledgerData]) {
		return func(typ *agent.Type[ledgerData]) {
			ms := *typ.States[0].Model
			ms.Tools = []agent.Tool[ledgerData]{tool}
			ms.WrapUp = agent.WrapUp{Tools: []string{tool.Definition.Name}}
			typ.States = append(typ.States, agent.State[ledgerData]{Name: "again", Next: []string{"review"}, Model: &ms})
		}
	}
	lookup := ledgerTools(&execLog{})[0]
	otherLookup := lookup
	otherLookup.Definition.Description = "a different lookup"
	cases := map[string]*agent.Type[ledgerData]{
		"no trigger kinds":     mutate(func(typ *agent.Type[ledgerData]) { typ.Triggers = nil }),
		"empty trigger kind":   mutate(func(typ *agent.Type[ledgerData]) { typ.Triggers = []string{""} }),
		"invalid trigger kind": mutate(func(typ *agent.Type[ledgerData]) { typ.Triggers = []string{"\xff"} }),
		"duplicate trigger":    mutate(func(typ *agent.Type[ledgerData]) { typ.Triggers = []string{"folder", "one_shot", "folder"} }),
		"unsupported keyword":  mutate(func(typ *agent.Type[ledgerData]) { typ.Config = json.RawMessage(`{"oneOf":[]}`) }),
		"nested format": mutate(func(typ *agent.Type[ledgerData]) {
			typ.Config = json.RawMessage(`{"properties":{"a":{"format":"uri"}}}`)
		}),
		"schema not JSON":       mutate(func(typ *agent.Type[ledgerData]) { typ.Config = json.RawMessage(`{`) }),
		"schema invalid value":  mutate(func(typ *agent.Type[ledgerData]) { typ.Config = json.RawMessage(`{"minimum":"1"}`) }),
		"default deadline":      mutate(func(typ *agent.Type[ledgerData]) { typ.Budgets.Deadline = time.Now().Add(time.Hour) }),
		"default cost":          mutate(func(typ *agent.Type[ledgerData]) { typ.Budgets.Cost = 10 }),
		"negative default":      mutate(func(typ *agent.Type[ledgerData]) { typ.Budgets.Turns = -1 }),
		"estimated, no limit":   mutate(func(typ *agent.Type[ledgerData]) { typ.Budgets.Tokens.Estimated = true }),
		"tool approval":         mutate(func(typ *agent.Type[ledgerData]) { typ.States[0].Model.Tools[1].Approval = agent.ApprovalPropose }),
		"state approval":        mutate(func(typ *agent.Type[ledgerData]) { typ.States[0].Model.Approval = agent.ApprovalSuspend }),
		"unknown approval":      mutate(func(typ *agent.Type[ledgerData]) { typ.States[0].Model.Tools[0].Approval = "later" }),
		"invalid tool version":  mutate(func(typ *agent.Type[ledgerData]) { typ.States[0].Model.Tools[0].Version = "\xff" }),
		"one name, two tools":   mutate(secondState(otherLookup)),
		"one name, two version": mutate(secondState(func() agent.Tool[ledgerData] { v := lookup; v.Version = "9"; return v }())),
	}
	for name, typ := range cases {
		t.Run(name, func(t *testing.T) {
			if err := agent.NewRegistry().Register(typ); !errors.Is(err, agent.ErrInvalidType) {
				t.Fatalf("Register = %v, want ErrInvalidType", err)
			}
		})
	}
	// The same tool granted to two states is one tool.
	typ := mutate(secondState(lookup))
	m := manifestOf(t, typ)
	if len(m.Tools) != 3 || !slices.Equal(m.States[len(m.States)-1].Tools, []string{"lookup"}) {
		t.Fatalf("manifest = %+v", m)
	}
}

// A run is refused before it starts for an undeclared trigger kind or a
// configuration its schema rejects (MAN-2), with config_error, and the
// refused start is on the record with the manifest (ADR 0011, items 8
// and 11).
func TestRunStartRefusedForConfiguration(t *testing.T) {
	const schema = `{"type":"object","required":["model"],"additionalProperties":false,
		"properties":{"model":{"type":"string","minLength":1}}}`
	cases := []struct {
		name, kind, config, schema string
		detail                     []string
	}{
		{name: "undeclared kind", kind: agent.TriggerSchedule, detail: []string{`trigger kind "schedule"`}},
		{name: "no kind", kind: "", detail: []string{`trigger kind ""`}},
		{name: "invalid JSON", kind: agent.TriggerOneShot, config: `{`, detail: []string{"configuration"}},
		{name: "schema rejects", kind: agent.TriggerOneShot, config: `{"model":"","extra":1}`, schema: schema,
			detail: []string{"/extra: is not an allowed property", "/model: must be at least 1 characters long"}},
		{name: "absent configuration", kind: agent.TriggerOneShot, schema: schema, detail: []string{"(root): must be object, not null"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, b backend) {
				s := b.open()
				defer s.Close()
				if _, _, err := s.CreateRun(context.Background(), store.Run{
					ID: "r1", Instance: inst, AgentType: "counter", TriggerID: "t1", TriggerKind: tc.kind, Trigger: json.RawMessage(`{"target":1}`),
				}); err != nil {
					t.Fatal(err)
				}
				typ := counterType()
				if tc.schema != "" {
					typ.Config = json.RawMessage(tc.schema)
				}
				r := registry(t, typ)
				var cfg json.RawMessage
				if tc.config != "" {
					cfg = json.RawMessage(tc.config)
				}
				out, err := agent.NewController(s, r).Activate(context.Background(), agent.ActivationInput{
					Fence: claim(t, s), Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}, Config: cfg,
				})
				if err != nil || out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonConfigError {
					t.Fatalf("outcome = %+v, %v; want failed with config_error", out, err)
				}
				for _, d := range tc.detail {
					if !strings.Contains(out.Detail, d) {
						t.Errorf("detail %q lacks %q", out.Detail, d)
					}
				}
				es := entries(t, s, "r1")
				if got := kinds(es); !slices.Equal(got, []string{"activation_started", "run_started", "outcome"}) {
					t.Fatalf("entries = %v", got)
				}
				wantManifestRecorded(t, s, r, es[1], tc.kind)
			})
		})
	}
}

func wantManifestRecorded(t *testing.T, s store.Store, r *agent.Registry, e store.Entry, kind string) {
	t.Helper()
	var rs agent.RunStarted
	if err := json.Unmarshal(e.Payload, &rs); err != nil {
		t.Fatal(err)
	}
	digest, _ := r.ManifestDigest(rs.AgentType)
	if rs.Manifest != digest || rs.TriggerKind != kind {
		t.Fatalf("run_started = %+v, want manifest %s and trigger kind %q", rs, digest, kind)
	}
	_, raw, err := s.Artifact(context.Background(), rs.Manifest)
	if err != nil {
		t.Fatalf("manifest artifact: %v", err)
	}
	m, _ := r.Manifest(rs.AgentType)
	want, _ := json.Marshal(m)
	if string(raw) != string(want) {
		t.Fatalf("manifest artifact = %s, want %s", raw, want)
	}
}

// A configuration the schema accepts starts the run; an absent one is
// validated as null.
func TestRunStartValidConfiguration(t *testing.T) {
	cases := map[string]struct{ schema, config string }{
		"object":         {`{"type":"object","properties":{"model":{"type":"string"}}}`, `{"model":"a"}`},
		"null allowed":   {`{"type":["object","null"]}`, ``},
		"no schema":      {``, `[1, 2]`},
		"no schema, nil": {``, ``},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newBackend(t, "memstore").open()
			createRun(t, s, "r1", `{"target":1}`)
			typ := counterType()
			if tc.schema != "" {
				typ.Config = json.RawMessage(tc.schema)
			}
			var cfg json.RawMessage
			if tc.config != "" {
				cfg = json.RawMessage(tc.config)
			}
			r := registry(t, typ)
			out, err := agent.NewController(s, r).Activate(context.Background(), agent.ActivationInput{
				Fence: claim(t, s), Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}, Config: cfg,
			})
			if err != nil || out.Status != store.StatusSucceeded {
				t.Fatalf("outcome = %+v, %v", out, err)
			}
			wantManifestRecorded(t, s, r, entries(t, s, "r1")[1], agent.TriggerOneShot)
		})
	}
}

// The agent type's default budgets fill those the host leaves unset, and
// the result is what is recorded (ADR 0011, item 7).
func TestDefaultBudgets(t *testing.T) {
	defaults := agent.Budgets{Turns: 50, ActiveTime: time.Hour, Tokens: agent.TokenBudget{Limit: 1_000_000, Estimated: true}}
	cases := map[string]struct{ host, want agent.Budgets }{
		"none set": {agent.Budgets{}, defaults},
		"host overrides one": {agent.Budgets{Turns: 40},
			agent.Budgets{Turns: 40, ActiveTime: time.Hour, Tokens: agent.TokenBudget{Limit: 1_000_000, Estimated: true}}},
		"host sets all": {agent.Budgets{Turns: 30, ActiveTime: time.Minute, Tokens: agent.TokenBudget{Limit: 500, Estimated: true}},
			agent.Budgets{Turns: 30, ActiveTime: time.Minute, Tokens: agent.TokenBudget{Limit: 500, Estimated: true}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newBackend(t, "memstore").open()
			createLedgerRun(t, s)
			typ := ledgerType(&execLog{})
			typ.Budgets = defaults
			if _, err := activateBudgets(context.Background(), agent.NewController(s, registry(t, typ)), claim(t, s),
				scripted(ledgerScript), 0, tc.host); err != nil {
				t.Fatal(err)
			}
			var rs agent.RunStarted
			if err := json.Unmarshal(entries(t, s, "r1")[1].Payload, &rs); err != nil || rs.Budgets == nil || *rs.Budgets != tc.want {
				t.Fatalf("recorded budgets = %+v, %v; want %+v", rs.Budgets, err, tc.want)
			}
		})
	}
	// A default is enforced like the host's own.
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	typ := ledgerType(&execLog{})
	typ.Budgets = agent.Budgets{Turns: 1}
	out, err := activateBudgets(context.Background(), agent.NewController(s, registry(t, typ)), claim(t, s),
		scripted(ledgerScript), 0, agent.Budgets{})
	if err != nil || out.Status != store.StatusFailed {
		t.Fatalf("outcome = %+v, %v; want the default turn budget to end the run", out, err)
	}
	if out.Consumed.Turns != 1 {
		t.Fatalf("turns consumed = %d, want 1", out.Consumed.Turns)
	}
}

// The policy decides on the tool's identity, and the permission records it
// (ADR 0011, item 3).
func TestPermissionCarriesToolIdentity(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	typ := ledgerType(&execLog{})
	r := registry(t, typ)
	m, _ := r.Manifest("ledger")
	var mu sync.Mutex
	seen := map[string]string{}
	policy := func(_ context.Context, req *agent.PolicyRequest) (agent.Decision, error) {
		mu.Lock()
		defer mu.Unlock()
		seen[req.Tool] = req.Identity
		return agent.Decision{Verdict: agent.VerdictAllow}, nil
	}
	out, err := activateWith(context.Background(), agent.NewController(s, r, agent.WithPolicy(policy)), claim(t, s), scripted(ledgerScript), 0)
	if err != nil || out.Status != store.StatusSucceeded {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
	permissions := 0
	for _, e := range entries(t, s, "r1") {
		if e.Kind != agent.EntryPermission {
			continue
		}
		var p agent.PermissionEntry
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		permissions++
		if want := toolOf(t, &m, p.Tool).Identity; p.Identity != want || seen[p.Tool] != want {
			t.Errorf("tool %s: permission identity %q, policy saw %q; want %q", p.Tool, p.Identity, seen[p.Tool], want)
		}
	}
	if permissions == 0 {
		t.Fatal("no permission entries")
	}
}
