package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/ui"
	"github.com/SnapdragonPartners/maestro-agents/standalone"
)

// The serve command's defaults (ADR 0022, item 2).
const (
	defaultAddr      = "127.0.0.1:8741"
	tokenEnv         = "ADDEPAR_SESSION_TOKEN"
	workspaceName    = "Addepar Intake"
	shutdownGrace    = 5 * time.Second
	readHeaderLimit  = 10 * time.Second
	minSessionToken  = 16
	workspaceDirMode = 0o700
)

// serveEnv is what serve takes from its surroundings; tests replace it.
type serveEnv struct {
	// getenv reads the session token's variable.
	getenv func(string) string
	// home is the user's home folder, for the default workspace.
	home func() (string, error)
	// configDir is the user's configuration folder, for the secrets file
	// (ADR 0022, item 5).
	configDir func() (string, error)
	// open opens the link in a browser.
	open func(link string) error
	// ready is told the link once the server listens.
	ready func(link string)
	// wrap, when set, wraps the UI's handler: a test's hook.
	wrap func(http.Handler) http.Handler
	// goos is the operating system; empty is runtime.GOOS.
	goos string
}

func defaultServeEnv() serveEnv {
	return serveEnv{getenv: os.Getenv, home: os.UserHomeDir, configDir: os.UserConfigDir, open: openBrowser, ready: func(string) {}}
}

// workspace is the operator's folder (ADR 0021, item 12; ADR 0022, item
// 2): Inbox/ and Exports/ in sight, the store, configuration and
// manifests in the hidden .agent/.
type workspace struct {
	dir, inbox, exports, agent, manifests, config string
}

func newWorkspace(dir string) workspace {
	agentDir := filepath.Join(dir, ".agent")
	return workspace{
		dir: dir, inbox: filepath.Join(dir, "Inbox"), exports: filepath.Join(dir, "Exports"),
		agent: agentDir, manifests: filepath.Join(agentDir, "exports"), config: filepath.Join(agentDir, "config.json"),
	}
}

// credential is the workspace's model credential's name in the user's
// secrets file: one per workspace, so setting up another workspace never
// replaces this one's key.
func (w *workspace) credential() string {
	abs, err := filepath.Abs(w.dir)
	if err != nil {
		abs = w.dir
	}
	sum := sha256.Sum256([]byte(abs))
	return "workspace-" + hex.EncodeToString(sum[:8])
}

// create makes the workspace's folders, owner-only: they hold client data.
func (w *workspace) create() error {
	for _, d := range []string{w.dir, w.inbox, w.exports, w.agent, w.manifests} {
		if err := os.MkdirAll(d, workspaceDirMode); err != nil {
			return fmt.Errorf("workspace: %w", err)
		}
	}
	return nil
}

// serveCommand runs `addepar serve` (ADR 0022): the folder watch and the
// review UI in one process, until a signal.
func serveCommand(ctx context.Context, p standalone.Program, args []string, stdout, stderr io.Writer, env serveEnv) int { //nolint:gocritic // hugeParam: called once
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	wsDir := fs.String("workspace", "", "the workspace folder (default ~/Documents/"+workspaceName+")")
	addr := fs.String("addr", defaultAddr, "the loopback address to serve on")
	open := fs.Bool("open", true, "open the UI in the default browser")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		say(stderr, "serve: unexpected arguments\n")
		return exitUsage
	}
	// The workspace's store is the file store, which locks only on Unix
	// systems: serve would fail later on Windows, after setup.
	if cmp.Or(env.goos, runtime.GOOS) == "windows" {
		say(stderr, "serve: not supported on Windows: the workspace's file store needs a Unix system (macOS or Linux)\n")
		return exitUsage
	}
	if host, _, err := net.SplitHostPort(*addr); err != nil || !ui.Loopback(host) {
		say(stderr, "serve: -addr %s is not a loopback address (127.0.0.1, ::1 or localhost)\n", *addr)
		return exitUsage
	}
	if *wsDir == "" {
		home, err := env.home()
		if err != nil {
			say(stderr, "serve: no home folder for the default workspace: %v\n", err)
			return exitUsage
		}
		*wsDir = filepath.Join(home, "Documents", workspaceName)
	}
	ws := newWorkspace(*wsDir)
	if err := ws.create(); err != nil {
		say(stderr, "serve: %v\n", err)
		return exitUsage
	}
	token := env.getenv(tokenEnv)
	if token == "" {
		var b [32]byte
		_, _ = rand.Read(b[:])
		token = hex.EncodeToString(b[:])
	} else if len(token) < minSessionToken {
		say(stderr, "serve: %s must be at least %d characters\n", tokenEnv, minSessionToken)
		return exitUsage
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		say(stderr, "serve: %v (another copy may be running; choose another port with -addr)\n", err)
		return exitUsage
	}
	s := &server{p: p, ws: ws, stderr: stderr, env: env, watchDone: make(chan int, 1), grace: shutdownGrace}
	return s.run(ctx, ln, token, *open, stdout)
}

// server is one serve process: the host, once configured, its watch, and
// the UI.
type server struct {
	stderr    io.Writer
	host      *standalone.Host
	watchDone chan int
	env       serveEnv
	ws        workspace
	p         standalone.Program
	// grace is how long shutdown waits before cancelling requests.
	grace time.Duration
}

func (s *server) run(ctx context.Context, ln net.Listener, token string, open bool, stdout io.Writer) int {
	defer s.close()
	// The watch stops at the signal; requests have their own context,
	// cancelled only after the grace period (ADR 0022, item 2).
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rctx, rcancel := context.WithCancel(context.WithoutCancel(ctx))
	defer rcancel()
	srv, err := s.newUI(wctx, token, ln.Addr().String())
	if err != nil {
		say(s.stderr, "serve: %v\n", err)
		return exitUsage
	}
	var h http.Handler = srv
	if s.env.wrap != nil {
		h = s.env.wrap(h)
	}
	inflight := newGate()
	hs := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !inflight.enter() {
				http.Error(w, "shutting down", http.StatusServiceUnavailable)
				return
			}
			defer inflight.leave()
			h.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: readHeaderLimit,
		// Requests outlive the signal by the grace period: shutdown then
		// cancels rctx, stopping a submission at its next step boundary.
		BaseContext: func(net.Listener) context.Context { return rctx },
	}
	served := make(chan error, 1)
	go func() { served <- hs.Serve(ln) }()
	link := fmt.Sprintf("http://%s/auth/session?token=%s", ln.Addr().String(), url.QueryEscape(token))
	say(stdout, "Addepar Intake is running.\nworkspace: %s\nopen: %s\n", s.ws.dir, link)
	s.env.ready(link)
	if open {
		if err := s.env.open(link); err != nil {
			say(s.stderr, "serve: cannot open a browser (%v); open the link above\n", err)
		}
	}
	code := exitOK
	select {
	case <-ctx.Done():
	case err := <-served:
		say(s.stderr, "serve: the server stopped: %v\n", err)
		code = exitFail
	case c := <-s.watchDone:
		s.watchDone <- c
		// A signal can end the watch before this select sees it: only a
		// watch that stopped on its own is a failure.
		if ctx.Err() == nil {
			say(s.stderr, "serve: the folder watch stopped (exit %d): the store must be reopened\n", c)
			code = exitFail
		}
	}
	// Stop the watch on every path, not only a signal's: after a server
	// failure ctx is still live, and the watch would never end.
	cancel()
	s.shutdown(ctx, hs, rcancel, inflight)
	if s.host != nil {
		if c := <-s.watchDone; c != standalone.ExitComplete && code == exitOK {
			code = c
		}
	}
	return code
}

// newUI starts the configured workspace's host and watch and builds the
// UI on it, or, with no configuration yet, builds the UI in setup mode,
// whose setup starts them (ADR 0022, item 4).
func (s *server) newUI(wctx context.Context, token, addr string) (*ui.Server, error) {
	opts := ui.Options{
		Token: token, Addr: addr, Exports: s.ws.exports, Manifests: s.ws.manifests,
		Logger: slog.New(slog.NewTextHandler(s.stderr, nil)),
	}
	if _, err := os.Stat(s.ws.config); err == nil {
		if opts.Inbox, err = s.start(wctx); err != nil {
			return nil, err
		}
		return ui.New(opts, s.host) //nolint:wrapcheck // ui names it
	}
	opts.Setup = func(_ context.Context, in ui.SetupInput) (ui.Backend, ui.Inbox, error) {
		if err := s.setup(in); err != nil {
			return nil, ui.Inbox{}, err
		}
		inbox, err := s.start(wctx)
		if err != nil {
			_ = os.Remove(s.ws.config) // setup runs again with the person's next try
			return nil, ui.Inbox{}, err
		}
		return s.host, inbox, nil
	}
	return ui.New(opts, nil) //nolint:wrapcheck // ui names it
}

// shutdown stops the server: requests in flight get the grace period,
// then their context is cancelled (a submission stops at its next step
// boundary) and every handler is joined, so none outlives the host.
func (s *server) shutdown(ctx context.Context, hs *http.Server, cancel context.CancelFunc, inflight *gate) {
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), s.grace)
	defer scancel()
	err := hs.Shutdown(sctx)
	cancel()
	if err != nil {
		_ = hs.Close()
	}
	inflight.closeAndWait()
}

// gate counts the requests in flight; once closed it admits none, and
// closeAndWait returns when the last one has left.
type gate struct {
	cond   *sync.Cond
	mu     sync.Mutex
	n      int
	closed bool
}

func newGate() *gate {
	g := &gate{}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *gate) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.n++
	return true
}

func (g *gate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n--
	g.cond.Broadcast()
}

func (g *gate) closeAndWait() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	for g.n > 0 {
		g.cond.Wait()
	}
}

// start opens the host on the workspace's configuration and starts its
// watch, and returns the watched folder as the watch sees it.
func (s *server) start(ctx context.Context) (ui.Inbox, error) {
	cfg, err := standalone.LoadConfig(s.ws.config)
	if err != nil {
		return ui.Inbox{}, err //nolint:wrapcheck // names the file
	}
	if cfg.Trigger.Kind != "folder" {
		return ui.Inbox{}, fmt.Errorf("config %s: serve needs a folder trigger, not %q", s.ws.config, cfg.Trigger.Kind)
	}
	h, err := standalone.Open(ctx, s.p, s.ws.config, s.stderr)
	if err != nil {
		return ui.Inbox{}, err //nolint:wrapcheck // standalone names it
	}
	s.host = h
	go func() {
		code, err := h.Watch(ctx)
		if err != nil {
			say(s.stderr, "serve: %v\n", err)
		}
		s.watchDone <- code
	}()
	return ui.Inbox{Dir: cfg.Trigger.Dir, Pattern: cfg.Trigger.Pattern, MaxBytes: cmp.Or(cfg.MaxInputBytes, standalone.DefaultMaxInputBytes)}, nil
}

func (s *server) close() {
	if s.host != nil {
		_ = s.host.Close()
	}
}

// setup writes the API key to the secrets file and the workspace's
// configuration (ADR 0022, items 3 to 5). It never overwrites a
// configuration.
func (s *server) setup(in ui.SetupInput) error {
	// Reserve the configuration first: a second setup of this workspace
	// fails here, before it can touch the key this one writes.
	f, err := os.OpenFile(s.ws.config, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the workspace's own file
	if errors.Is(err, fs.ErrExist) {
		return errors.New("the workspace is already configured")
	}
	if err != nil {
		return fmt.Errorf("write the configuration: %w", err)
	}
	werr := s.writeConfig(f, in)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(s.ws.config)
		return werr
	}
	return nil
}

// writeConfig writes the workspace's key to its secrets file and its
// configuration to f.
func (s *server) writeConfig(f *os.File, in ui.SetupInput) error {
	cred := s.ws.credential()
	secrets, err := s.writeSecret(cred, in.Key)
	if err != nil {
		return err
	}
	cfg := map[string]any{
		"agent_type": "addepar-intake",
		"namespace":  "intake",
		"store":      map[string]any{"kind": "file", "dir": "store"},
		"trigger":    map[string]any{"kind": "folder", "dir": "../Inbox"},
		"model": map[string]any{
			"provider": in.Provider, "name": in.Model, "credential": cred, "timeout": "5m",
		},
		"credentials": map[string]any{"file": secrets},
		"budgets":     map[string]any{"turns": 16, "active_time": "10m", "tokens": 600000, "tokens_estimated": true},
		"telemetry":   map[string]any{"sink": "store"},
		"log":         map[string]any{"level": "info"},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the configuration: %w", err)
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write the configuration: %w", err)
	}
	return nil
}

// writeSecret writes the workspace's own secrets file,
// <user config dir>/addepar/<credential>.json, owner-only, holding only
// its key (ADR 0022, item 5): no file is shared between workspaces, so
// setting up two at once never loses either key. It returns the path.
func (s *server) writeSecret(name, key string) (string, error) {
	base, err := s.env.configDir()
	if err != nil {
		return "", fmt.Errorf("no user configuration folder for the API key: %w", err)
	}
	dir := filepath.Join(base, "addepar")
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("secrets: %w", err)
	}
	path := filepath.Join(dir, name+".json")
	b, err := json.MarshalIndent(map[string]string{name: key}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("secrets: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".secrets-*")
	if err != nil {
		return "", fmt.Errorf("secrets: %w", err)
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	if err = tmp.Chmod(0o600); err == nil {
		if _, err = tmp.Write(b); err == nil {
			err = tmp.Sync()
		}
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return "", fmt.Errorf("secrets: %w", err)
	}
	return path, nil
}

// openBrowser opens the link in the default browser: macOS's open, or
// xdg-open elsewhere (serve refuses Windows, ADR 0022 item 2).
func openBrowser(link string) error {
	cmd := exec.Command("xdg-open", link)
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("open", link)
	}
	if err := cmd.Start(); err != nil {
		return err //nolint:wrapcheck // the caller says what failed
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
