package standalone

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/agent"
)

func TestDecodeStrict(t *testing.T) {
	type inner struct {
		B int `json:"b"`
	}
	type doc struct {
		A    string            `json:"a"`
		In   inner             `json:"in"`
		List []inner           `json:"list"`
		Map  map[string]inner  `json:"map"`
		Raw  []byte            `json:"-"`
		Any  any               `json:"any"`
		D    Duration          `json:"d"`
		Ptr  *inner            `json:"ptr"`
		Strs map[string]string `json:"strs"`
	}
	ok := `{"a":"x","in":{"b":1},"list":[{"b":2}],"map":{"k":{"b":3}},"any":{"z":1},"d":"5s","ptr":{"b":4},"strs":{"A":"v"}}`
	var d doc
	if err := decodeStrict([]byte(ok), &d); err != nil {
		t.Fatalf("valid document: %v", err)
	}
	if d.In.B != 1 || d.List[0].B != 2 || d.Map["k"].B != 3 || time.Duration(d.D) != 5*time.Second || d.Ptr.B != 4 {
		t.Fatalf("decoded %+v", d)
	}
	for name, tc := range map[string]struct{ in, want string }{
		"unknown field":         {`{"zz":1}`, `unknown field "zz"`},
		"case mismatch":         {`{"A":"x"}`, `unknown field "A"`},
		"nested unknown":        {`{"in":{"c":1}}`, `/in: unknown field "c"`},
		"in a slice":            {`{"list":[{"b":1},{"B":1}]}`, `/list/1: unknown field "B"`},
		"in a map":              {`{"map":{"k":{"x":1}}}`, `/map/k: unknown field "x"`},
		"behind a pointer":      {`{"ptr":{"q":1}}`, `/ptr: unknown field "q"`},
		"repeated key":          {`{"a":"x","a":"y"}`, `key "a" is repeated`},
		"repeated nested":       {`{"in":{"b":1,"b":2}}`, `key "b" is repeated`},
		"repeated in any":       {`{"any":{"z":1,"z":2}}`, `key "z" is repeated`},
		"repeated in a map":     {`{"strs":{"k":"1","k":"2"}}`, `key "k" is repeated`},
		"trailing data":         {`{"a":"x"} {}`, "data after the JSON value"},
		"trailing garbage":      {`{"a":"x"}x`, "data after the JSON value"},
		"syntax error":          {`{"a":`, "EOF"},
		"wrong type":            {`{"a":1}`, "cannot unmarshal"},
		"bad duration":          {`{"d":"5 parsecs"}`, "duration"},
		"duration not a string": {`{"d":5}`, "a duration is a string"},
	} {
		t.Run(name, func(t *testing.T) {
			var got doc
			err := decodeStrict([]byte(tc.in), &got)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
	if err := decodeStrict([]byte("{\"a\":\"\xff\"}"), &d); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8: %v", err)
	}
}

func TestLoadConfigResolvesRelativePaths(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, map[string]any{
		"agent_type": "probe", "namespace": "n",
		"store":       map[string]any{"kind": "file", "dir": "state"},
		"trigger":     map[string]any{"kind": "folder", "dir": "/abs/inbox", "poll_interval": "2s"},
		"credentials": map[string]any{"file": "secrets.json"},
		"telemetry":   map[string]any{"sink": "jsonl", "path": "events.jsonl"},
		"budgets":     map[string]any{"active_time": "10m", "deadline_after": "1h", "turns": 3},
	})
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(dir)
	for got, want := range map[string]string{
		c.Store.Dir:        filepath.Join(abs, "state"),
		c.Trigger.Dir:      "/abs/inbox",
		c.Credentials.File: filepath.Join(abs, "secrets.json"),
		c.Telemetry.Path:   filepath.Join(abs, "events.jsonl"),
	} {
		if got != want {
			t.Errorf("path %q, want %q", got, want)
		}
	}
	if c.Trigger.pollInterval() != 2*time.Second || time.Duration(c.Budgets.ActiveTime) != 10*time.Minute {
		t.Errorf("durations: %+v", c)
	}
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	b := c.Budgets.budgets(now)
	if !b.Deadline.Equal(now.Add(time.Hour)) || b.Turns != 3 || b.ActiveTime != 10*time.Minute {
		t.Errorf("budgets %+v", b)
	}
}

func TestLoadConfigRefusesStrictly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	for name, body := range map[string]string{
		"mcp servers are not built": `{"agent_type":"probe","mcp_servers":[]}`,
		"no cost budget":            `{"budgets":{"cost":5}}`,
		"repeated key":              `{"namespace":"a","namespace":"b"}`,
		"repeated in config":        `{"config":{"x":1,"x":2}}`,
		"case":                      `{"Agent_Type":"probe"}`,
	} {
		t.Run(name, func(t *testing.T) {
			writeFile(t, path, body)
			if _, err := LoadConfig(path); err == nil {
				t.Fatal("loaded")
			}
		})
	}
	if _, err := LoadConfig(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("loaded a missing file")
	}
	big := make([]byte, maxConfigBytes+1)
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized: %v", err)
	}
}

// modelType is a type with a model state, to check that a model route is
// required.
func modelType() *agent.Type[probeData] {
	return &agent.Type[probeData]{
		Name: "modeled", Version: "1", Triggers: []string{agent.TriggerOneShot}, Initial: "think",
		States: []agent.State[probeData]{
			{Name: "think", Next: []string{"done"}, Model: &agent.ModelState[probeData]{
				Prompt: func(context.Context, *agent.Env, probeData) (llms.ChatRequest, error) { return llms.ChatRequest{}, nil },
				Tools:  []agent.Tool[probeData]{finishTool()},
				WrapUp: agent.WrapUp{Tools: []string{"finish"}},
			}},
			{Name: "done", End: agent.EndChanged},
		},
	}
}

func TestValidate(t *testing.T) {
	p := probeProgram(t, newProbe())
	if err := p.Registry.Register(modelType()); err != nil {
		t.Fatal(err)
	}
	p.Providers = map[llms.ProviderID]Provider{"fake": fakeProvider(new(string))}
	base := func() *Config {
		return &Config{
			AgentType: "probe", Namespace: "n", Config: []byte(`{}`),
			Store:   StoreConfig{Kind: StoreMemory},
			Trigger: TriggerConfig{Kind: agent.TriggerOneShot},
		}
	}
	if err := base().Validate(&p); err != nil {
		t.Fatalf("valid: %v", err)
	}
	// Windows file modes say nothing of who can read a file, so a secrets
	// file is refused there (ADR 0017, item 19).
	withFile := base()
	withFile.Credentials.File = "/s.json"
	if err := withFile.Validate(&p); err != nil {
		t.Fatalf("secrets file here: %v", err)
	}
	win := p
	win.goos = "windows"
	if err := withFile.Validate(&win); err == nil || !strings.Contains(err.Error(), "credentials.file is not supported on windows") {
		t.Fatalf("secrets file on windows: %v", err)
	}
	if err := base().Validate(&win); err != nil {
		t.Fatalf("no secrets file on windows: %v", err)
	}
	cases := map[string]struct {
		edit func(*Config)
		want string
	}{
		"no agent type":      {func(c *Config) { c.AgentType = "" }, "agent_type is required"},
		"unregistered":       {func(c *Config) { c.AgentType = "nope" }, `"nope" is not registered`},
		"no namespace":       {func(c *Config) { c.Namespace = "" }, "namespace is required"},
		"namespace too long": {func(c *Config) { c.Namespace = strings.Repeat("N", 81) }, "namespace: "},
		"undeclared trigger": {func(c *Config) {
			c.AgentType = "modeled"
			c.Trigger = TriggerConfig{Kind: agent.TriggerFolder, Dir: "/x"}
		}, `"folder" is not one agent type "modeled" declares`},
		"unbuilt trigger":    {func(c *Config) { c.Trigger.Kind = agent.TriggerSchedule }, "is not one this host builds"},
		"no trigger":         {func(c *Config) { c.Trigger.Kind = "" }, "trigger.kind is required"},
		"schema rejects":     {func(c *Config) { c.Config = []byte(`{"greeting":5}`) }, "config: "},
		"schema null":        {func(c *Config) { c.Config = nil }, "must be object, not null"},
		"schema extra":       {func(c *Config) { c.Config = []byte(`{"other":"x"}`) }, "config: "},
		"negative tokens":    {func(c *Config) { c.Budgets.Tokens = -1 }, "budgets.tokens -1 is negative"},
		"estimated no limit": {func(c *Config) { c.Budgets.TokensEstimated = true }, "tokens_estimated is set"},
		"negative turns":     {func(c *Config) { c.Budgets.Turns = -1 }, "budgets.turns"},
		"negative active":    {func(c *Config) { c.Budgets.ActiveTime = -1 }, "budgets.active_time"},
		"negative deadline":  {func(c *Config) { c.Budgets.DeadlineAfter = -1 }, "budgets.deadline_after"},
		"model state, none":  {func(c *Config) { c.AgentType = "modeled" }, "has a model state"},
		"no provider":        {func(c *Config) { c.Model = &ModelConfig{Name: "m"} }, "model.provider is required"},
		"unknown provider": {func(c *Config) { c.Model = &ModelConfig{Provider: "acme", Name: "m"} },
			`"acme" is not one this program offers (anthropic, fake, google, ollama, openai, openaicompat)`},
		"ollama with a key": {func(c *Config) {
			c.Model = &ModelConfig{Provider: "ollama", Name: "m", Credential: "k"}
			c.Credentials.Env = map[string]string{"k": "K"}
		}, `provider "ollama" takes no API key`},
		"openaicompat, no url": {func(c *Config) { c.Model = &ModelConfig{Provider: "openaicompat", Name: "m"} },
			`model.base_url is required for provider "openaicompat"`},
		"no model name":    {func(c *Config) { c.Model = &ModelConfig{Provider: "fake"} }, "model.name is required"},
		"undeclared cred":  {func(c *Config) { c.Model = &ModelConfig{Provider: "fake", Name: "m", Credential: "k"} }, `model.credential "k" is not declared`},
		"negative timeout": {func(c *Config) { c.Model = &ModelConfig{Provider: "fake", Name: "m", Timeout: -1} }, "model.timeout"},
		"negative rate": {func(c *Config) {
			c.Model = &ModelConfig{Provider: "fake", Name: "m", RateLimit: &RateLimitConfig{TokensPerMinute: -1, MaxConcurrency: -1, MaxWait: -1}}
		}, "max_wait"},
		"no store kind":      {func(c *Config) { c.Store.Kind = "" }, "store.kind is required"},
		"bad store kind":     {func(c *Config) { c.Store.Kind = "postgres" }, `store.kind "postgres"`},
		"memory with dir":    {func(c *Config) { c.Store.Dir = "/x" }, "store.dir is only for a file store"},
		"file without dir":   {func(c *Config) { c.Store.Kind = StoreFile }, "store.dir is required"},
		"one-shot with dir":  {func(c *Config) { c.Trigger.Dir = "/x" }, "only for a folder trigger"},
		"folder without dir": {func(c *Config) { c.Trigger = TriggerConfig{Kind: agent.TriggerFolder} }, "trigger.dir is required"},
		"bad pattern":        {func(c *Config) { c.Trigger = TriggerConfig{Kind: agent.TriggerFolder, Dir: "/x", Pattern: "["} }, "trigger.pattern"},
		"pattern with path":  {func(c *Config) { c.Trigger = TriggerConfig{Kind: agent.TriggerFolder, Dir: "/x", Pattern: "a/*.pdf"} }, "names a path"},
		"poll too fast": {func(c *Config) {
			c.Trigger = TriggerConfig{Kind: agent.TriggerFolder, Dir: "/x", PollInterval: Duration(time.Millisecond)}
		}, "is below"},
		"empty env var":        {func(c *Config) { c.Credentials.Env = map[string]string{"k": ""} }, "is not an environment variable name"},
		"env var with =":       {func(c *Config) { c.Credentials.Env = map[string]string{"k": "A=B"} }, "is not an environment variable name"},
		"empty cred name":      {func(c *Config) { c.Credentials.Env = map[string]string{"": "A"} }, "empty credential name"},
		"bad sink":             {func(c *Config) { c.Telemetry.Sink = "kafka" }, `telemetry.sink "kafka"`},
		"jsonl without path":   {func(c *Config) { c.Telemetry.Sink = SinkJSONL }, "telemetry.path is required"},
		"store sink with path": {func(c *Config) { c.Telemetry = TelemetryConfig{Sink: SinkStore, Path: "/x"} }, "only for a jsonl sink"},
		"none with path":       {func(c *Config) { c.Telemetry.Path = "/x" }, "only for a jsonl sink"},
		"required none":        {func(c *Config) { c.Telemetry.Required = true }, "telemetry.required is set"},
		"bad level":            {func(c *Config) { c.Log.Level = "loud" }, "log.level"},
		"bad format":           {func(c *Config) { c.Log.Format = "xml" }, "log.format"},
		"negative concurrency": {func(c *Config) { c.Concurrency = -1 }, "concurrency -1"},
		"negative max input":   {func(c *Config) { c.MaxInputBytes = -1 }, "max_input_bytes -1"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := base()
			tc.edit(c)
			err := c.Validate(&p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
	// Every problem is reported at once.
	c := base()
	c.AgentType, c.Store.Kind, c.Log.Level = "", "", "loud"
	if err := c.Validate(&p); err == nil || strings.Count(err.Error(), "\n") < 2 {
		t.Fatalf("not every problem: %v", err)
	}
	// A model route with a declared credential, and a folder trigger, are
	// valid.
	c = base()
	c.AgentType = "modeled"
	c.Model = &ModelConfig{Provider: "fake", Name: "m", Credential: "k"}
	c.Credentials.Env = map[string]string{"k": "K"}
	if err := c.Validate(&p); err != nil {
		t.Fatalf("model route: %v", err)
	}
	c = base()
	c.Trigger = TriggerConfig{Kind: agent.TriggerFolder, Dir: "/x", Pattern: "*.pdf"}
	c.Config = []byte(`{"greeting":"hi"}`)
	if err := c.Validate(&p); err != nil {
		t.Fatalf("folder: %v", err)
	}
}
