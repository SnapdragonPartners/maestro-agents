package standalone

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

// completion is an OpenAI Chat Completions response calling one tool.
func completion(id, tool string) string {
	return `{"id":"` + id + `","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":null,"tool_calls":[{"id":"` + id + `","type":"function",` +
		`"function":{"name":"` + tool + `","arguments":"{}"}}]},"finish_reason":"tool_calls"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
}

// TestBuiltinProvider runs a model state through a provider the host
// builds itself with providers.New, configured only by the configuration:
// an openaicompat endpoint, the credential as its bearer key (ADR 0017,
// item 2).
func TestBuiltinProvider(t *testing.T) {
	var mu sync.Mutex
	var auths, paths []string
	replies := []string{completion("c1", "fetch"), completion("c2", "finish")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		auths, paths = append(auths, r.Header.Get("Authorization")), append(paths, r.URL.Path)
		if len(replies) == 0 {
			http.Error(w, "no more replies", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, replies[0])
		replies = replies[1:]
	}))
	defer srv.Close()

	dir := t.TempDir()
	var got []string
	r := agent.NewRegistry()
	if err := r.Register(credType(&got, &mu)); err != nil {
		t.Fatal(err)
	}
	p := Program{Name: "creds", Registry: r, getenv: func(k string) string {
		return map[string]string{"API_KEY": "api-secret", "DB": "db-secret"}[k]
	}}
	cfg := oneShotConfig(dir)
	cfg["agent_type"] = "creds"
	cfg["model"] = map[string]any{"provider": "openaicompat", "name": "m", "base_url": srv.URL + "/v1", "credential": "api"}
	cfg["credentials"] = map[string]any{"env": map[string]any{"api": "API_KEY", "db": "DB"}}
	cfg["budgets"] = map[string]any{"turns": 5}
	path := writeConfig(t, dir, cfg)
	input := filepath.Join(dir, "in.txt")
	writeFile(t, input, "statement")
	o := runProgram(context.Background(), p, "run", "-config", path, "-input", input)
	if o.code != ExitComplete {
		t.Fatalf("exit %d: %s %s", o.code, o.stdout.String(), o.stderr.String())
	}
	if rep := o.report(t); rep.Status != store.StatusSucceeded {
		t.Fatalf("report %+v", rep)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 2 || auths[0] != "Bearer api-secret" || paths[0] != "/v1/chat/completions" {
		t.Fatalf("requests: auth %q, paths %q", auths, paths)
	}
	if len(got) != 1 || got[0] != "db-secret" {
		t.Fatalf("tool got %v", got)
	}
}

// TestBuiltinProviderRefusal: a built-in provider's constructor refusal
// (here, no API key) is a usage error before any run starts (ADR 0017,
// items 2 and 22).
func TestBuiltinProviderRefusal(t *testing.T) {
	dir := t.TempDir()
	r := agent.NewRegistry()
	if err := r.Register(credType(new([]string), new(sync.Mutex))); err != nil {
		t.Fatal(err)
	}
	p := Program{Registry: r, getenv: func(string) string { return "" }}
	cfg := oneShotConfig(dir)
	cfg["agent_type"] = "creds"
	cfg["model"] = map[string]any{"provider": "anthropic", "name": "claude-opus-5-5"}
	path := writeConfig(t, dir, cfg)
	if o := runProgram(context.Background(), p, "config", "validate", "-config", path); o.code != ExitComplete {
		t.Fatalf("validate: exit %d: %s", o.code, o.stderr.String())
	}
	input := filepath.Join(dir, "in.txt")
	writeFile(t, input, "x")
	o := runProgram(context.Background(), p, "run", "-config", path, "-input", input)
	if o.code != ExitUsage || !strings.Contains(o.stderr.String(), "anthropic") {
		t.Fatalf("exit %d: %s", o.code, o.stderr.String())
	}
	if o.stdout.Len() != 0 {
		t.Fatalf("a run started: %s", o.stdout.String())
	}
}

// TestProgramProviderReplacesBuiltin: Program.Providers overrides a
// built-in ID, for routes the configuration cannot express (ADR 0017,
// item 2).
func TestProgramProviderReplacesBuiltin(t *testing.T) {
	dir := t.TempDir()
	r := agent.NewRegistry()
	if err := r.Register(credType(new([]string), new(sync.Mutex))); err != nil {
		t.Fatal(err)
	}
	var key string
	p := Program{
		Registry:  r,
		Providers: map[llms.ProviderID]Provider{llms.ProviderOllama: fakeProvider(&key, toolCall("c1", "finish"))},
		getenv:    func(k string) string { return map[string]string{"K": "custom-key"}[k] },
	}
	cfg := oneShotConfig(dir)
	cfg["agent_type"] = "creds"
	// A credential with ollama is refused for the built-in builder, not
	// for the program's own.
	cfg["model"] = map[string]any{"provider": "ollama", "name": "m", "credential": "k"}
	cfg["credentials"] = map[string]any{"env": map[string]any{"k": "K"}}
	cfg["budgets"] = map[string]any{"turns": 5}
	path := writeConfig(t, dir, cfg)
	input := filepath.Join(dir, "in.txt")
	writeFile(t, input, "x")
	if o := runProgram(context.Background(), p, "run", "-config", path, "-input", input); o.code != ExitComplete {
		t.Fatalf("exit %d: %s", o.code, o.stderr.String())
	}
	if key != "custom-key" {
		t.Fatalf("the program's provider got %q", key)
	}
}
