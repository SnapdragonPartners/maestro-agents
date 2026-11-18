package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/ui"
	"github.com/SnapdragonPartners/maestro-agents/internal/strictjson"
	"github.com/SnapdragonPartners/maestro-agents/standalone"
)

// The serve command's defaults (ADR 0022, item 2).
const (
	defaultAddr      = "127.0.0.1:8741"
	tokenEnv         = "ADDEPAR_SESSION_TOKEN"
	workspaceName    = "Addepar Intake"
	secretsName      = "secrets.json"
	modelCredential  = "model"
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
	s := &server{p: p, ws: ws, stderr: stderr, env: env, watchDone: make(chan int, 1)}
	return s.run(ctx, ln, token, *open, stdout)
}

// server is one serve process: the host, once configured, its watch, and
// the UI.
type server struct {
	stderr    io.Writer
	host      *standalone.Host
	watchDone chan int
	cancel    context.CancelFunc
	env       serveEnv
	ws        workspace
	p         standalone.Program
}

func (s *server) run(ctx context.Context, ln net.Listener, token string, open bool, stdout io.Writer) int {
	defer s.close()
	wctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	defer cancel()
	inbox := s.ws.inbox
	var backend ui.Backend
	var setup func(context.Context, ui.SetupInput) (ui.Backend, error)
	if _, err := os.Stat(s.ws.config); err == nil {
		if inbox, err = s.start(wctx); err != nil {
			say(s.stderr, "serve: %v\n", err)
			return exitUsage
		}
		backend = s.host
	} else {
		setup = func(_ context.Context, in ui.SetupInput) (ui.Backend, error) {
			if err := s.setup(in); err != nil {
				return nil, err
			}
			if _, err := s.start(wctx); err != nil {
				_ = os.Remove(s.ws.config) // setup runs again with the person's next try
				return nil, err
			}
			return s.host, nil
		}
	}
	srv, err := ui.New(ui.Options{
		Token: token, Addr: ln.Addr().String(), Inbox: inbox, Exports: s.ws.exports, Manifests: s.ws.manifests, Setup: setup,
	}, backend)
	if err != nil {
		say(s.stderr, "serve: %v\n", err)
		return exitUsage
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: readHeaderLimit}
	served := make(chan error, 1)
	go func() { served <- hs.Serve(ln) }()
	link := fmt.Sprintf("http://%s/auth/session?token=%s", ln.Addr().String(), token)
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
		say(s.stderr, "serve: the folder watch stopped (exit %d): the store must be reopened\n", c)
		s.watchDone <- c
		code = exitFail
	}
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer scancel()
	_ = hs.Shutdown(sctx)
	cancel()
	if s.host != nil {
		if c := <-s.watchDone; c != standalone.ExitComplete && code == exitOK {
			code = c
		}
	}
	return code
}

// start opens the host on the workspace's configuration and starts its
// watch, and returns the watched folder.
func (s *server) start(ctx context.Context) (string, error) {
	cfg, err := standalone.LoadConfig(s.ws.config)
	if err != nil {
		return "", err //nolint:wrapcheck // names the file
	}
	if cfg.Trigger.Kind != "folder" {
		return "", fmt.Errorf("config %s: serve needs a folder trigger, not %q", s.ws.config, cfg.Trigger.Kind)
	}
	h, err := standalone.Open(ctx, s.p, s.ws.config, s.stderr)
	if err != nil {
		return "", err //nolint:wrapcheck // standalone names it
	}
	s.host = h
	go func() {
		code, err := h.Watch(ctx)
		if err != nil {
			say(s.stderr, "serve: %v\n", err)
		}
		s.watchDone <- code
	}()
	return cfg.Trigger.Dir, nil
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
	if _, err := os.Stat(s.ws.config); err == nil {
		return errors.New("the workspace is already configured")
	}
	secrets, err := s.writeSecret(in.Key)
	if err != nil {
		return err
	}
	cfg := map[string]any{
		"agent_type": "addepar-intake",
		"namespace":  "intake",
		"store":      map[string]any{"kind": "file", "dir": "store"},
		"trigger":    map[string]any{"kind": "folder", "dir": "../Inbox"},
		"model": map[string]any{
			"provider": in.Provider, "name": in.Model, "credential": modelCredential, "timeout": "5m",
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
	f, err := os.OpenFile(s.ws.config, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the workspace's own file
	if err != nil {
		return fmt.Errorf("write the configuration: %w", err)
	}
	_, werr := f.Write(append(b, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(s.ws.config)
		return fmt.Errorf("write the configuration: %w", werr)
	}
	return nil
}

// writeSecret stores the key as the model credential in the user's
// secrets file, outside the workspace, owner-only, keeping any other
// credential in it, and returns its path.
func (s *server) writeSecret(key string) (string, error) {
	base, err := s.env.configDir()
	if err != nil {
		return "", fmt.Errorf("no user configuration folder for the API key: %w", err)
	}
	dir := filepath.Join(base, "addepar")
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("secrets: %w", err)
	}
	path := filepath.Join(dir, secretsName)
	values := map[string]string{}
	switch b, rerr := os.ReadFile(path); { //nolint:gosec // G304: the user's own secrets file
	case rerr == nil:
		if err = strictjson.Decode(b, &values); err != nil {
			return "", fmt.Errorf("secrets file %s: %w", path, err)
		}
	case !errors.Is(rerr, fs.ErrNotExist):
		return "", fmt.Errorf("secrets: %w", rerr)
	}
	values[modelCredential] = key
	b, err := json.MarshalIndent(values, "", "  ")
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

// openBrowser opens the link in the default browser.
func openBrowser(link string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", link)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", link)
	default:
		cmd = exec.Command("xdg-open", link)
	}
	if err := cmd.Start(); err != nil {
		return err //nolint:wrapcheck // the caller says what failed
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
