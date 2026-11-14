package standalone

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/ratelimit"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/filestore"
	"github.com/SnapdragonPartners/maestro-agents/store/memstore"
)

// Exit codes (MODE-6, ADR 0017 item 22), after buggrit's.
const (
	// ExitComplete: the one-shot run succeeded, or a watch stopped with
	// no activation interrupted.
	ExitComplete = 0
	// ExitIncomplete: the one-shot run did not succeed, or a watch's
	// shutdown interrupted an activation, or the store must be reopened.
	ExitIncomplete = 1
	// ExitUsage: bad arguments, an invalid configuration, or a credential,
	// store, required sink or watched folder that cannot be set up.
	ExitUsage = 2
)

// ModelRoute is the configured model route a Provider builds a client for.
type ModelRoute struct {
	Provider llms.ProviderID
	Name     string
	// BaseURL overrides the provider's endpoint; empty is its default.
	BaseURL string
}

// Provider builds a maestro-llms provider client for a route, with the
// route's credential (zero if the route names none). The host wraps it
// with agent.NewModelClient (ADR 0017, item 2).
type Provider func(route ModelRoute, credential agent.Secret) (llms.ChatClient, error)

// Program is an agent executable: what standalone.Main runs (ADR 0017,
// item 1).
type Program struct {
	// Registry holds the program's agent types.
	Registry *agent.Registry
	// Providers are the model providers the program offers, by ID. The
	// executable imports their SDKs; this package imports none.
	Providers map[llms.ProviderID]Provider
	// Policy is the policy hook every tool call passes (TOOL-3); nil
	// allows every call.
	Policy agent.Policy
	// Redactor edits each telemetry event before it leaves (HOST-2); nil
	// clears each event's Detail.
	Redactor agent.Redactor
	// getenv reads the environment; nil is os.Getenv. Tests set it.
	getenv func(string) string
	// Name is the executable's name, for usage messages.
	Name string
	// heartbeat overrides heartbeatInterval in tests.
	heartbeat time.Duration
}

func (p *Program) providerNames() []string {
	names := make([]string, 0, len(p.Providers))
	for id := range p.Providers {
		names = append(names, string(id))
	}
	sort.Strings(names)
	if len(names) == 0 {
		return []string{"none"}
	}
	return names
}

// Main runs the program with the process's arguments and exits with its
// exit code. The first SIGINT or SIGTERM shuts down gracefully; a second
// ends the process at once (ADR 0017, item 21).
func Main(p Program) { //nolint:gocritic // hugeParam: called once, by main
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop() // restore the default handling: a second signal ends the process
	}()
	code := Run(ctx, p, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// Run runs the program with the given arguments and returns its exit code.
// Cancelling ctx shuts it down gracefully (MODE-6).
func Run(ctx context.Context, p Program, args []string, stdout, stderr io.Writer) int { //nolint:gocritic // hugeParam: called once
	name := p.Name
	if name == "" {
		name = "agent"
	}
	usage := func() int {
		say(stderr, "usage:\n  %s run -config FILE [-input FILE]\n  %s config validate -config FILE\n", name, name)
		return ExitUsage
	}
	switch {
	case len(args) >= 1 && args[0] == "run":
		return runCommand(ctx, &p, args[1:], stdout, stderr)
	case len(args) >= 2 && args[0] == "config" && args[1] == "validate":
		return validateCommand(&p, args[2:], stdout, stderr)
	}
	return usage()
}

func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

func validateCommand(p *Program, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "the configuration file")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *path == "" || fs.NArg() != 0 {
		say(stderr, "config validate: -config is required\n")
		return ExitUsage
	}
	if _, err := loadAndValidate(p, *path); err != nil {
		say(stderr, "%v\n", err)
		return ExitUsage
	}
	say(stdout, "%s: valid\n", *path)
	return ExitComplete
}

func loadAndValidate(p *Program, path string) (*Config, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(p); err != nil {
		return nil, fmt.Errorf("config %s is invalid:\n%w", path, err)
	}
	return cfg, nil
}

func runCommand(ctx context.Context, p *Program, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "the configuration file")
	inputPath := fs.String("input", "", "the input file (one_shot trigger)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *path == "" || fs.NArg() != 0 {
		say(stderr, "run: -config is required\n")
		return ExitUsage
	}
	cfg, err := loadAndValidate(p, *path)
	if err != nil {
		say(stderr, "%v\n", err)
		return ExitUsage
	}
	oneShot := cfg.Trigger.Kind == agent.TriggerOneShot
	switch {
	case oneShot && *inputPath == "":
		say(stderr, "run: a one_shot trigger needs -input\n")
		return ExitUsage
	case !oneShot && *inputPath != "":
		say(stderr, "run: -input is only for a one_shot trigger\n")
		return ExitUsage
	}
	h, cleanup, err := setup(ctx, p, cfg, stderr)
	if err != nil {
		say(stderr, "run: %v\n", err)
		return ExitUsage
	}
	defer cleanup()
	if oneShot {
		return h.oneShot(ctx, *inputPath, stdout, stderr)
	}
	return h.watch(ctx)
}

// setup builds the host: logger, credentials, model client, store,
// telemetry and controller (ADR 0017, items 7, 9, 19 and 20). An error is
// a usage error; nothing has run.
func setup(_ context.Context, p *Program, cfg *Config, stderr io.Writer) (h *host, cleanup func(), err error) {
	logger := newLogger(&cfg.Log, stderr)
	getenv := p.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	creds, err := loadCredentials(&cfg.Credentials, cfg.Principal, getenv)
	if err != nil {
		return nil, nil, fmt.Errorf("credentials: %w", err)
	}
	client, err := newModelClient(p, cfg.Model, creds)
	if err != nil {
		return nil, nil, fmt.Errorf("model: %w", err)
	}
	if err = checkFolder(&cfg.Trigger); err != nil {
		return nil, nil, err
	}
	var closers []io.Closer
	cleanup = func() {
		for i := len(closers) - 1; i >= 0; i-- {
			if cerr := closers[i].Close(); cerr != nil {
				logger.Warn("standalone: close failed", "error_class", errorClass(cerr))
			}
		}
	}
	st, err := openStore(&cfg.Store)
	if err != nil {
		return nil, nil, fmt.Errorf("store: %w", err)
	}
	closers = append(closers, st)
	if cfg.Store.Kind == StoreMemory && cfg.Trigger.Kind == agent.TriggerFolder {
		logger.Warn("standalone: the memory store keeps nothing across a restart; unfinished runs are lost with the process")
	}
	sink, closer, err := openSink(&cfg.Telemetry, st)
	switch {
	case err != nil && cfg.Telemetry.Required:
		cleanup()
		return nil, nil, fmt.Errorf("telemetry (required): %w", err)
	case err != nil:
		logger.Warn("standalone: optional telemetry sink unavailable; running without telemetry", "sink", cfg.Telemetry.Sink)
	case closer != nil:
		closers = append(closers, closer)
	}
	h = &host{
		cfg: cfg, st: st, ctrl: agent.NewController(st, p.Registry, controllerOptions(p, logger, creds, sink)...), client: client,
		logger: logger, owner: newOwnerID(), maxInput: cmp.Or(cfg.MaxInputBytes, DefaultMaxInputBytes),
		heartbeat: cmp.Or(p.heartbeat, heartbeatInterval), now: time.Now,
	}
	return h, cleanup, nil
}

// checkFolder checks that a folder trigger's folder exists.
func checkFolder(t *TriggerConfig) error {
	if t.Kind != agent.TriggerFolder {
		return nil
	}
	fi, err := os.Stat(t.Dir)
	if err != nil {
		return fmt.Errorf("trigger.dir: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("trigger.dir %s is not a directory", t.Dir)
	}
	return nil
}

func controllerOptions(p *Program, logger *slog.Logger, creds *credentials, sink agent.Sink) []agent.Option {
	opts := []agent.Option{
		agent.WithLogger(logger), agent.WithCredentials(creds.provider),
		agent.WithPolicy(p.Policy), agent.WithRedactor(p.Redactor),
	}
	if sink != nil {
		opts = append(opts, agent.WithTelemetry(sink))
	}
	return opts
}

func newLogger(c *LogConfig, w io.Writer) *slog.Logger {
	var level slog.Level
	switch c.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	o := &slog.HandlerOptions{Level: level}
	if c.Format == "json" {
		return slog.New(slog.NewJSONHandler(w, o))
	}
	return slog.New(slog.NewTextHandler(w, o))
}

func openStore(c *StoreConfig) (store.Store, error) {
	if c.Kind == StoreFile {
		s, err := filestore.Open(c.Dir)
		if err != nil {
			return nil, err //nolint:wrapcheck // the caller says it was the store
		}
		return s, nil
	}
	return memstore.New(), nil
}

// newModelClient builds the process's one model client (ADR 0017, item 2),
// nil when the configuration has no model route.
func newModelClient(p *Program, mc *ModelConfig, creds *credentials) (*agent.ModelClient, error) {
	if mc == nil {
		return nil, nil //nolint:nilnil // no route is no client: a model state then fails with model_error (ADR 0008)
	}
	var key agent.Secret
	if mc.Credential != "" {
		var ok bool
		if key, ok = creds.get(mc.Credential); !ok {
			return nil, fmt.Errorf("credential %q is not provided", mc.Credential)
		}
	}
	route := ModelRoute{Provider: llms.ProviderID(mc.Provider), Name: mc.Name, BaseURL: mc.BaseURL}
	base, err := p.Providers[route.Provider](route, key)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", mc.Provider, err)
	}
	cfg := agent.ModelClientConfig{Timeout: time.Duration(mc.Timeout)}
	if r := mc.RateLimit; r != nil {
		lim, err := ratelimit.NewInMemoryLimiter(ratelimit.Config{
			TokensPerMinute: r.TokensPerMinute, MaxConcurrency: r.MaxConcurrency, MaxWait: time.Duration(r.MaxWait),
		})
		if err != nil {
			return nil, fmt.Errorf("rate_limit: %w", err)
		}
		cfg.Limiter = lim
	}
	return agent.NewModelClient(base, cfg) //nolint:wrapcheck // the caller says it was the model
}

// report is the one-shot command's output: the run's outcome.
type report struct {
	Consumed *agent.Consumption `json:"consumed,omitempty"`
	Run      string             `json:"run"`
	Input    string             `json:"input"`
	SHA256   string             `json:"sha256"`
	Status   store.Status       `json:"status,omitempty"`
	Reason   store.Reason       `json:"reason,omitzero"`
	Detail   string             `json:"detail,omitempty"`
	// Error is set when the activation had no confirmed outcome; Status
	// is then the run's as reread, if the reread succeeded.
	Error string `json:"error,omitempty"`
	Steps uint64 `json:"steps,omitempty"`
	// Activated is false when the run was already terminal.
	Activated bool `json:"activated"`
}

// oneShot makes the input's run, activates it if it is runnable, and
// prints its outcome (ADR 0017, items 13 and 15).
func (h *host) oneShot(ctx context.Context, path string, stdout, stderr io.Writer) int {
	in, err := hashInput(path, h.maxInput)
	if err != nil {
		say(stderr, "run: input %s: %v\n", path, err)
		return ExitUsage
	}
	dctx := context.WithoutCancel(ctx)
	run, err := h.makeRun(dctx, in, agent.TriggerOneShot)
	if err != nil {
		say(stderr, "run: %v\n", err)
		return ExitIncomplete
	}
	rep := report{Run: run.ID, Input: in.trigger.Path, SHA256: in.trigger.SHA256, Status: run.Status, Reason: run.Reason}
	switch run.Status {
	case store.StatusRunnable:
		if ctx.Err() != nil {
			break // shut down before the run started: it stays runnable
		}
		r := h.activate(ctx, &run)
		rep.Activated = true
		rep.Status, rep.Reason = r.Status, r.Reason
		if r.Err != nil {
			rep.Error = r.Err.Error()
		} else {
			rep.Detail, rep.Steps = r.Outcome.Detail, r.Outcome.Steps
			c := r.Outcome.Consumed
			rep.Consumed = &c
		}
	case store.StatusWaiting:
		rep.Error = "the run is waiting for input, which this host cannot deliver"
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		say(stderr, "run: %v\n", err)
	}
	if rep.Status == store.StatusSucceeded {
		return ExitComplete
	}
	return ExitIncomplete
}
