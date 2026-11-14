package standalone

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

func scriptedClient(script []llms.ChatResponse) *testllm.FakeChatClient {
	return &testllm.FakeChatClient{ModelRef: llms.ModelRef{Provider: "fake", Name: "scripted"}, Responses: script}
}

func toolCall(id, name string) llms.ChatResponse {
	c := llms.ToolCall{ID: id, Name: name, Parameters: json.RawMessage(`{}`)}
	return llms.ChatResponse{
		Message:   llms.Message{Role: llms.RoleAssistant, Content: []llms.ContentPart{{Type: llms.ContentToolCall, ToolCall: &c}}},
		ToolCalls: []llms.ToolCall{c},
		Usage:     llms.Usage{InputTokens: 10, OutputTokens: 5},
	}
}

func finishTool() agent.Tool[probeData] {
	return agent.Tool[probeData]{
		Definition: llms.ToolDefinition{Name: "finish", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Effect:     agent.EffectRead,
		Execute: func(context.Context, *agent.Call[probeData]) (agent.Result[probeData], error) {
			return agent.Result[probeData]{Content: "finished", To: "done"}, nil
		},
	}
}

// credType's fetch tool asks for the "db" credential and records what it
// got.
func credType(got *[]string, mu *sync.Mutex) *agent.Type[probeData] {
	fetch := agent.Tool[probeData]{
		Definition: llms.ToolDefinition{Name: "fetch", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Effect:     agent.EffectRead,
		Execute: func(ctx context.Context, c *agent.Call[probeData]) (agent.Result[probeData], error) {
			s, err := c.Credential(ctx, "db")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				*got = append(*got, "error")
			} else {
				*got = append(*got, s.Reveal())
			}
			return agent.TextResult[probeData]("fetched"), nil
		},
	}
	return &agent.Type[probeData]{
		Name: "creds", Version: "1", Triggers: []string{agent.TriggerOneShot}, Initial: "work",
		States: []agent.State[probeData]{
			{Name: "work", Next: []string{"done"}, Model: &agent.ModelState[probeData]{
				Prompt: func(context.Context, *agent.Env, probeData) (llms.ChatRequest, error) {
					return llms.ChatRequest{Messages: []llms.Message{llms.UserText("go")}}, nil
				},
				Tools:  []agent.Tool[probeData]{fetch, finishTool()},
				WrapUp: agent.WrapUp{Tools: []string{"finish"}},
			}},
			{Name: "done", End: agent.EndChanged},
		},
	}
}

// TestModelRouteAndCredentials runs a model state end to end: the provider
// gets the route's credential, a tool gets its credential through TOOL-8,
// and the store sink records the run's events (ADR 0017, items 2, 19, 20).
func TestModelRouteAndCredentials(t *testing.T) {
	dir := t.TempDir()
	var got []string
	var mu sync.Mutex
	r := agent.NewRegistry()
	if err := r.Register(credType(&got, &mu)); err != nil {
		t.Fatal(err)
	}
	var providerKey string
	secrets := filepath.Join(dir, "secrets.json")
	writeFile(t, secrets, `{"db":"db-secret"}`)
	p := Program{
		Name: "creds", Registry: r,
		Providers: map[llms.ProviderID]Provider{"fake": fakeProvider(&providerKey, toolCall("c1", "fetch"), toolCall("c2", "finish"))},
		getenv:    func(k string) string { return map[string]string{"API_KEY": "api-secret"}[k] },
	}
	cfg := oneShotConfig(dir)
	cfg["agent_type"] = "creds"
	cfg["principal"] = "ops"
	cfg["model"] = map[string]any{"provider": "fake", "name": "m", "credential": "api", "timeout": "30s", "rate_limit": map[string]any{"max_concurrency": 2}}
	cfg["credentials"] = map[string]any{"env": map[string]any{"api": "API_KEY"}, "file": "secrets.json"}
	cfg["telemetry"] = map[string]any{"sink": "store", "required": true}
	cfg["budgets"] = map[string]any{"turns": 5}
	path := writeConfig(t, dir, cfg)
	input := filepath.Join(dir, "in.txt")
	writeFile(t, input, "statement")
	o := runProgram(context.Background(), p, "run", "-config", path, "-input", input)
	if o.code != ExitComplete {
		t.Fatalf("exit %d: %s %s", o.code, o.stdout.String(), o.stderr.String())
	}
	if rep := o.report(t); rep.Status != store.StatusSucceeded || rep.Consumed == nil || rep.Consumed.Turns != 2 {
		t.Fatalf("report %+v", rep)
	}
	if providerKey != "api-secret" {
		t.Fatalf("provider got %q", providerKey)
	}
	mu.Lock()
	if len(got) != 1 || got[0] != "db-secret" {
		t.Fatalf("tool got %v", got)
	}
	mu.Unlock()
	s := openState(t, dir)
	rep := o.report(t)
	tel, ok := s.(store.Telemetry)
	if !ok {
		t.Fatal("the file store has no telemetry storage")
	}
	evs, err := tel.Events(context.Background(), rep.Run, 0)
	if err != nil || len(evs) == 0 {
		t.Fatalf("telemetry: %d events, %v", len(evs), err)
	}
	if run, err := s.Run(context.Background(), rep.Run); err != nil || run.TriggerKind != agent.TriggerOneShot {
		t.Fatalf("run %+v, %v", run, err)
	}
}

// TestMissingCredentialIsAUsageError: run reads the credentials before
// anything runs (ADR 0017, items 7 and 22).
func TestMissingCredentialIsAUsageError(t *testing.T) {
	dir := t.TempDir()
	var got []string
	var mu sync.Mutex
	r := agent.NewRegistry()
	if err := r.Register(credType(&got, &mu)); err != nil {
		t.Fatal(err)
	}
	p := Program{Registry: r, Providers: map[llms.ProviderID]Provider{"fake": fakeProvider(new(string))}, getenv: func(string) string { return "" }}
	cfg := oneShotConfig(dir)
	cfg["agent_type"] = "creds"
	cfg["model"] = map[string]any{"provider": "fake", "name": "m", "credential": "api"}
	cfg["credentials"] = map[string]any{"env": map[string]any{"api": "API_KEY"}}
	path := writeConfig(t, dir, cfg)
	if o := runProgram(context.Background(), p, "config", "validate", "-config", path); o.code != ExitComplete {
		t.Fatalf("validate needs no secrets: exit %d: %s", o.code, o.stderr.String())
	}
	input := filepath.Join(dir, "in.txt")
	writeFile(t, input, "x")
	if o := runProgram(context.Background(), p, "run", "-config", path, "-input", input); o.code != ExitUsage {
		t.Fatalf("exit %d: %s", o.code, o.stderr.String())
	}
}
