package standalone

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/internal/jsonschema"
)

// maxConfigBytes bounds a configuration or secrets file.
const maxConfigBytes = 1 << 20

// Defaults (ADR 0017).
const (
	DefaultMaxInputBytes = 64 << 20
	DefaultPollInterval  = 5 * time.Second
	MinPollInterval      = 100 * time.Millisecond
	DefaultPattern       = "*"
)

// The store kinds (ADR 0017, item 9).
const (
	StoreMemory = "memory"
	StoreFile   = "file"
)

// The telemetry sinks (ADR 0017, item 20).
const (
	SinkNone  = "none"
	SinkStore = "store"
	SinkJSONL = "jsonl"
)

// Config is the standalone host's configuration file (MODE-2, ADR 0017
// items 4 to 8). It is decoded strictly: an unknown or repeated key is an
// error.
type Config struct {
	Model *ModelConfig `json:"model,omitempty"`
	Log   LogConfig    `json:"log,omitzero"`
	Store StoreConfig  `json:"store"`
	// Credentials declares the credentials the host provides.
	Credentials CredentialsConfig `json:"credentials,omitzero"`
	// Principal is whose authority runs act under (ADR 0016, item 12);
	// empty is the host's own.
	Principal string `json:"principal,omitempty"`
	// AgentType names the registered agent type every run runs.
	AgentType string `json:"agent_type"`
	// Namespace is the object-store namespace every run is given (ADR
	// 0012).
	Namespace string          `json:"namespace"`
	Trigger   TriggerConfig   `json:"trigger"`
	Telemetry TelemetryConfig `json:"telemetry,omitzero"`
	// Config is the instance configuration, validated against the agent
	// type's schema; absent is JSON null.
	Config  json.RawMessage `json:"config,omitempty"`
	Budgets BudgetsConfig   `json:"budgets,omitzero"`
	// Concurrency is how many runs execute at once; zero is one.
	Concurrency int `json:"concurrency,omitempty"`
	// MaxInputBytes bounds an input file; zero is DefaultMaxInputBytes.
	MaxInputBytes int64 `json:"max_input_bytes,omitempty"`
}

// BudgetsConfig is a run's budgets (ADR 0017, item 6). A zero budget takes
// the agent type's default.
type BudgetsConfig struct {
	Tokens          int64    `json:"tokens,omitempty"`
	TokensEstimated bool     `json:"tokens_estimated,omitempty"`
	Turns           int      `json:"turns,omitempty"`
	ActiveTime      Duration `json:"active_time,omitempty"`
	// DeadlineAfter sets the run's deadline this long after its first
	// activation starts.
	DeadlineAfter Duration `json:"deadline_after,omitempty"`
}

// ModelConfig is the model route (LLM-3) and its client's tuning.
type ModelConfig struct {
	RateLimit *RateLimitConfig `json:"rate_limit,omitempty"`
	// Provider names a provider the program offers (Program.Providers).
	Provider string `json:"provider"`
	// Name is the provider's model name.
	Name string `json:"name"`
	// BaseURL overrides the provider's endpoint.
	BaseURL string `json:"base_url,omitempty"`
	// Credential names the credential passed to the provider, declared
	// in Credentials; empty passes none.
	Credential string `json:"credential,omitempty"`
	// Timeout bounds each provider attempt; zero sets none.
	Timeout Duration `json:"timeout,omitempty"`
}

// RateLimitConfig configures the process's in-memory rate limiter (LLM-4).
type RateLimitConfig struct {
	TokensPerMinute int      `json:"tokens_per_minute,omitempty"`
	MaxConcurrency  int      `json:"max_concurrency,omitempty"`
	MaxWait         Duration `json:"max_wait,omitempty"`
}

// StoreConfig chooses the store (MODE-3).
type StoreConfig struct {
	// Kind is StoreMemory or StoreFile.
	Kind string `json:"kind"`
	// Dir is the file store's directory.
	Dir string `json:"dir,omitempty"`
}

// TriggerConfig chooses the trigger (MODE-4, ADR 0017 items 15 and 16).
type TriggerConfig struct {
	// Kind is agent.TriggerOneShot or agent.TriggerFolder.
	Kind string `json:"kind"`
	// Dir is the watched folder.
	Dir string `json:"dir,omitempty"`
	// Pattern selects the folder's files by name (filepath.Match);
	// empty is DefaultPattern.
	Pattern string `json:"pattern,omitempty"`
	// PollInterval is how often the folder is scanned; zero is
	// DefaultPollInterval.
	PollInterval Duration `json:"poll_interval,omitempty"`
}

// CredentialsConfig declares the credentials the host provides (MODE-5,
// ADR 0017 item 19).
type CredentialsConfig struct {
	// Env maps a credential's name to the environment variable holding
	// it.
	Env map[string]string `json:"env,omitempty"`
	// File names a secrets file: a JSON object from name to value.
	File string `json:"file,omitempty"`
}

// TelemetryConfig chooses the telemetry sink (ADR 0017, item 20).
type TelemetryConfig struct {
	// Sink is SinkNone (empty), SinkStore or SinkJSONL.
	Sink string `json:"sink,omitempty"`
	// Path is the SinkJSONL file.
	Path string `json:"path,omitempty"`
	// Required fails the start when the sink cannot be set up.
	Required bool `json:"required,omitempty"`
}

// LogConfig chooses the log handler.
type LogConfig struct {
	// Level is debug, info (empty), warn or error.
	Level string `json:"level,omitempty"`
	// Format is text (empty) or json.
	Format string `json:"format,omitempty"`
}

// Duration is a time.Duration written as a string time.ParseDuration
// reads, such as "10m".
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("a duration is a string such as \"10m\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err //nolint:wrapcheck // ParseDuration's message names the value
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String()) //nolint:wrapcheck // a string always marshals
}

// LoadConfig reads and strictly decodes a configuration file, resolving its
// relative paths against the file's directory (ADR 0017, item 4). It does
// not validate the result: Validate does.
func LoadConfig(path string) (*Config, error) {
	data, err := readBounded(path, maxConfigBytes)
	if err != nil {
		return nil, err
	}
	var c Config
	if err = decodeStrict(data, &c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	base := filepath.Dir(abs)
	for _, p := range []*string{&c.Store.Dir, &c.Trigger.Dir, &c.Credentials.File, &c.Telemetry.Path} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
	}
	return &c, nil
}

// readBounded reads a file of at most limit bytes.
func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the operator names the file
	if err != nil {
		return nil, err //nolint:wrapcheck // the PathError names the file
	}
	defer func() { _ = f.Close() }()
	data, err := readLimited(f, limit)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return data, nil
}

// Validate checks the configuration against the program (ADR 0017, item
// 7), reporting every problem. It reads no credential and opens nothing.
func (c *Config) Validate(p *Program) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	var m agent.Manifest
	switch {
	case c.AgentType == "":
		add("agent_type is required")
	case p.Registry == nil:
		add("the program has no registry")
	default:
		var ok bool
		if m, ok = p.Registry.Manifest(c.AgentType); !ok {
			add("agent_type %q is not registered", c.AgentType)
		}
	}
	if c.Namespace == "" {
		add("namespace is required")
	}
	if m.Name != "" {
		errs = append(errs, c.validateAgainst(&m)...)
	}
	errs = append(errs, c.Budgets.validate()...)
	errs = append(errs, c.validateModel(p, &m)...)
	errs = append(errs, c.Store.validate()...)
	errs = append(errs, c.Trigger.validate()...)
	errs = append(errs, c.Credentials.validate()...)
	errs = append(errs, c.Telemetry.validate()...)
	errs = append(errs, c.Log.validate()...)
	if c.Concurrency < 0 {
		add("concurrency %d is negative", c.Concurrency)
	}
	if c.MaxInputBytes < 0 {
		add("max_input_bytes %d is negative", c.MaxInputBytes)
	}
	return errors.Join(errs...) //nolint:wrapcheck // the caller names the file
}

// validateAgainst checks what the agent type's manifest decides: the
// trigger kind and the instance configuration.
func (c *Config) validateAgainst(m *agent.Manifest) []error {
	var errs []error
	if c.Trigger.Kind != "" && !contains(m.Triggers, c.Trigger.Kind) {
		errs = append(errs, fmt.Errorf("trigger.kind %q is not one agent type %q declares (%s)", c.Trigger.Kind, m.Name, strings.Join(m.Triggers, ", ")))
	}
	if m.Config != nil {
		s, err := jsonschema.Compile(m.Config)
		if err != nil {
			// Registration compiled it; this cannot happen.
			return append(errs, fmt.Errorf("agent type %q: its configuration schema: %w", m.Name, err))
		}
		if err := s.Validate(c.instanceConfig()); err != nil {
			errs = append(errs, fmt.Errorf("config: %w", err))
		}
	}
	return errs
}

// instanceConfig is the instance configuration, JSON null if absent.
func (c *Config) instanceConfig() json.RawMessage {
	if len(c.Config) == 0 {
		return json.RawMessage("null")
	}
	return c.Config
}

func (b *BudgetsConfig) validate() []error {
	var errs []error
	if b.Tokens < 0 {
		errs = append(errs, fmt.Errorf("budgets.tokens %d is negative", b.Tokens))
	}
	if b.TokensEstimated && b.Tokens == 0 {
		errs = append(errs, errors.New("budgets.tokens_estimated is set but budgets.tokens is not"))
	}
	if b.Turns < 0 {
		errs = append(errs, fmt.Errorf("budgets.turns %d is negative", b.Turns))
	}
	if b.ActiveTime < 0 {
		errs = append(errs, fmt.Errorf("budgets.active_time %s is negative", time.Duration(b.ActiveTime)))
	}
	if b.DeadlineAfter < 0 {
		errs = append(errs, fmt.Errorf("budgets.deadline_after %s is negative", time.Duration(b.DeadlineAfter)))
	}
	return errs
}

// budgets returns the run's budgets as of now.
func (b *BudgetsConfig) budgets(now time.Time) agent.Budgets {
	out := agent.Budgets{
		Tokens:     agent.TokenBudget{Limit: b.Tokens, Estimated: b.TokensEstimated},
		Turns:      b.Turns,
		ActiveTime: time.Duration(b.ActiveTime),
	}
	if b.DeadlineAfter > 0 {
		out.Deadline = now.Add(time.Duration(b.DeadlineAfter))
	}
	return out
}

func (c *Config) validateModel(p *Program, m *agent.Manifest) []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	mc := c.Model
	if mc == nil {
		for i := range m.States {
			if s := &m.States[i]; s.Kind == agent.StateKindModel {
				add("agent type %q has a model state (%q), so model is required", m.Name, s.Name)
				break
			}
		}
		return errs
	}
	switch {
	case mc.Provider == "":
		add("model.provider is required")
	case p.Providers[llms.ProviderID(mc.Provider)] == nil:
		add("model.provider %q is not one this program offers (%s)", mc.Provider, strings.Join(p.providerNames(), ", "))
	}
	if mc.Name == "" {
		add("model.name is required")
	}
	if mc.Credential != "" && !c.Credentials.declares(mc.Credential) {
		add("model.credential %q is not declared in credentials", mc.Credential)
	}
	if mc.Timeout < 0 {
		add("model.timeout %s is negative", time.Duration(mc.Timeout))
	}
	if r := mc.RateLimit; r != nil {
		if r.TokensPerMinute < 0 {
			add("model.rate_limit.tokens_per_minute %d is negative", r.TokensPerMinute)
		}
		if r.MaxConcurrency < 0 {
			add("model.rate_limit.max_concurrency %d is negative", r.MaxConcurrency)
		}
		if r.MaxWait < 0 {
			add("model.rate_limit.max_wait %s is negative", time.Duration(r.MaxWait))
		}
	}
	return errs
}

func (s *StoreConfig) validate() []error {
	switch s.Kind {
	case StoreMemory:
		if s.Dir != "" {
			return []error{errors.New("store.dir is only for a file store")}
		}
	case StoreFile:
		if s.Dir == "" {
			return []error{errors.New("store.dir is required for a file store")}
		}
	case "":
		return []error{errors.New("store.kind is required (memory or file)")}
	default:
		return []error{fmt.Errorf("store.kind %q is not memory or file", s.Kind)}
	}
	return nil
}

func (t *TriggerConfig) validate() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	switch t.Kind {
	case agent.TriggerOneShot:
		if t.Dir != "" || t.Pattern != "" || t.PollInterval != 0 {
			add("trigger.dir, trigger.pattern and trigger.poll_interval are only for a folder trigger")
		}
	case agent.TriggerFolder:
		if t.Dir == "" {
			add("trigger.dir is required for a folder trigger")
		}
		if _, err := filepath.Match(t.pattern(), ""); err != nil {
			add("trigger.pattern %q: %v", t.Pattern, err)
		}
		if strings.ContainsRune(t.Pattern, filepath.Separator) {
			add("trigger.pattern %q names a path; it matches names in trigger.dir", t.Pattern)
		}
		if t.PollInterval != 0 && time.Duration(t.PollInterval) < MinPollInterval {
			add("trigger.poll_interval %s is below %s", time.Duration(t.PollInterval), MinPollInterval)
		}
	case "":
		add("trigger.kind is required (%s or %s)", agent.TriggerOneShot, agent.TriggerFolder)
	default:
		add("trigger.kind %q is not one this host builds (%s or %s)", t.Kind, agent.TriggerOneShot, agent.TriggerFolder)
	}
	return errs
}

func (t *TriggerConfig) pattern() string {
	if t.Pattern == "" {
		return DefaultPattern
	}
	return t.Pattern
}

func (t *TriggerConfig) pollInterval() time.Duration {
	if t.PollInterval == 0 {
		return DefaultPollInterval
	}
	return time.Duration(t.PollInterval)
}

func (c *CredentialsConfig) validate() []error {
	var errs []error
	for _, name := range sortedKeys(c.Env) {
		v := c.Env[name]
		if name == "" {
			errs = append(errs, errors.New("credentials.env has an empty credential name"))
		}
		if v == "" || strings.ContainsAny(v, "=\x00") {
			errs = append(errs, fmt.Errorf("credentials.env[%q]: %q is not an environment variable name", name, v))
		}
	}
	return errs
}

// declares reports whether the configuration may provide name: it is in
// Env, or a secrets file may hold it.
func (c *CredentialsConfig) declares(name string) bool {
	if _, ok := c.Env[name]; ok {
		return true
	}
	return c.File != ""
}

func (t *TelemetryConfig) validate() []error {
	var errs []error
	switch t.Sink {
	case "", SinkNone:
		if t.Required {
			errs = append(errs, errors.New("telemetry.required is set but telemetry.sink is none"))
		}
		if t.Path != "" {
			errs = append(errs, errors.New("telemetry.path is only for a jsonl sink"))
		}
	case SinkStore:
		if t.Path != "" {
			errs = append(errs, errors.New("telemetry.path is only for a jsonl sink"))
		}
	case SinkJSONL:
		if t.Path == "" {
			errs = append(errs, errors.New("telemetry.path is required for a jsonl sink"))
		}
	default:
		errs = append(errs, fmt.Errorf("telemetry.sink %q is not none, store or jsonl", t.Sink))
	}
	return errs
}

func (l *LogConfig) validate() []error {
	var errs []error
	switch l.Level {
	case "", "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level %q is not debug, info, warn or error", l.Level))
	}
	switch l.Format {
	case "", "text", "json":
	default:
		errs = append(errs, fmt.Errorf("log.format %q is not text or json", l.Format))
	}
	return errs
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
