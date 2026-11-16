package standalone

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/memstore"
)

func TestUsage(t *testing.T) {
	p := probeProgram(t, newProbe())
	dir := t.TempDir()
	path := writeConfig(t, dir, oneShotConfig(dir))
	folder := oneShotConfig(dir)
	folder["trigger"] = map[string]any{"kind": "folder", "dir": dir}
	folderPath := filepath.Join(dir, "folder.json")
	b, _ := json.Marshal(folder)
	writeFile(t, folderPath, string(b))
	for name, args := range map[string][]string{
		"no command":            nil,
		"unknown command":       {"serve"},
		"config alone":          {"config"},
		"run without config":    {"run"},
		"run with extra args":   {"run", "-config", path, "extra"},
		"bad flag":              {"run", "-nope"},
		"one-shot needs input":  {"run", "-config", path},
		"folder takes no input": {"run", "-config", folderPath, "-input", path},
		"missing input":         {"run", "-config", path, "-input", filepath.Join(dir, "absent")},
		"validate no config":    {"config", "validate"},
		"validate missing file": {"config", "validate", "-config", filepath.Join(dir, "absent.json")},
		"validate bad flag":     {"config", "validate", "-x"},
	} {
		t.Run(name, func(t *testing.T) {
			if o := runProgram(context.Background(), p, args...); o.code != ExitUsage {
				t.Fatalf("exit %d: %s", o.code, o.stderr.String())
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	p := probeProgram(t, newProbe())
	dir := t.TempDir()
	cfg := oneShotConfig(dir)
	path := writeConfig(t, dir, cfg)
	o := runProgram(context.Background(), p, "config", "validate", "-config", path)
	if o.code != ExitComplete || !strings.Contains(o.stdout.String(), "valid") {
		t.Fatalf("exit %d: %s %s", o.code, o.stdout.String(), o.stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
		t.Fatalf("validate opened the store: %v", err)
	}
	cfg["config"] = map[string]any{"greeting": 5}
	cfg["budgets"] = map[string]any{"turns": -1}
	path = writeConfig(t, dir, cfg)
	o = runProgram(context.Background(), p, "config", "validate", "-config", path)
	if o.code != ExitUsage || !strings.Contains(o.stderr.String(), "budgets.turns") || !strings.Contains(o.stderr.String(), "config: ") {
		t.Fatalf("exit %d: %s", o.code, o.stderr.String())
	}
}

// TestOneShot covers the one-shot trigger (ADR 0017, items 11 to 13, 15
// and 22): a success, a failure, and running a file again.
func TestOneShot(t *testing.T) {
	pr := newProbe()
	p := probeProgram(t, pr)
	dir := t.TempDir()
	cfg := oneShotConfig(dir)
	cfg["config"] = map[string]any{"greeting": "hi"}
	path := writeConfig(t, dir, cfg)
	good := filepath.Join(dir, "good.txt")
	writeFile(t, good, "fine")
	o := runProgram(context.Background(), p, "run", "-config", path, "-input", good)
	if o.code != ExitComplete {
		t.Fatalf("exit %d: %s %s", o.code, o.stdout.String(), o.stderr.String())
	}
	rep := o.report(t)
	if !rep.Activated || rep.Status != store.StatusSucceeded || rep.Reason.Code != agent.ReasonChanged || rep.Steps != 1 || !strings.HasPrefix(rep.Run, "sha256-") {
		t.Fatalf("report %+v", rep)
	}

	// The same bytes under another name are the same run: reported, not
	// run again.
	copyPath := filepath.Join(dir, "copy.txt")
	writeFile(t, copyPath, "fine")
	o = runProgram(context.Background(), p, "run", "-config", path, "-input", copyPath)
	again := o.report(t)
	if o.code != ExitComplete || again.Activated || again.Run != rep.Run || again.Status != store.StatusSucceeded {
		t.Fatalf("second run: exit %d %+v", o.code, again)
	}
	if n := len(pr.started); n != 1 {
		t.Fatalf("the state ran %d times", n)
	}

	bad := filepath.Join(dir, "bad.txt")
	writeFile(t, bad, "fail please")
	o = runProgram(context.Background(), p, "run", "-config", path, "-input", bad)
	rep = o.report(t)
	if o.code != ExitIncomplete || rep.Status != store.StatusFailed || rep.Reason.Code != agent.ReasonInvalidOutput {
		t.Fatalf("failure: exit %d %+v", o.code, rep)
	}

	s := openState(t, dir)
	run, err := s.Run(context.Background(), rep.Run)
	if err != nil {
		t.Fatal(err)
	}
	if run.TriggerID != "sha256:"+strings.TrimPrefix(rep.Run, "sha256-") || run.TriggerKind != agent.TriggerOneShot {
		t.Fatalf("run %+v", run)
	}
	if trig, err := s.Triggers(context.Background(), run.Instance); err != nil || len(trig) != 0 {
		t.Fatalf("queued triggers left: %v, %v", trig, err)
	}
}

// TestOneShotShutdown: a shutdown leaves the run runnable with reason
// shutdown, and the exit code is 1 (MODE-6, ADR 0017 items 21 and 22).
func TestOneShotShutdown(t *testing.T) {
	pr := newProbe()
	p := probeProgram(t, pr)
	dir := t.TempDir()
	path := writeConfig(t, dir, oneShotConfig(dir))
	input := filepath.Join(dir, "in.txt")
	writeFile(t, input, "block until shutdown")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *runOutput)
	go func() { done <- runProgram(ctx, p, "run", "-config", path, "-input", input) }()
	waitFor(t, pr.started, 1)
	cancel()
	o := <-done
	rep := o.report(t)
	if o.code != ExitIncomplete || rep.Status != store.StatusRunnable || rep.Reason.Code != agent.ReasonShutdown || rep.Error != "" {
		t.Fatalf("shutdown: exit %d %+v %s", o.code, rep, o.stderr.String())
	}
}

// TestOneShotResumesTheSameRun: running a file again continues its
// runnable run (ADR 0017, item 13).
func TestOneShotResumesTheSameRun(t *testing.T) {
	pr := newProbe()
	p := probeProgram(t, pr)
	dir := t.TempDir()
	path := writeConfig(t, dir, oneShotConfig(dir))
	input := filepath.Join(dir, "in.txt")
	writeFile(t, input, "slow")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *runOutput)
	go func() { done <- runProgram(ctx, p, "run", "-config", path, "-input", input) }()
	waitFor(t, pr.started, 1)
	cancel()
	o := <-done
	first := o.report(t)
	if first.Status != store.StatusRunnable {
		t.Fatalf("shutdown: %+v", first)
	}
	o = runProgram(context.Background(), p, "run", "-config", path, "-input", input)
	second := o.report(t)
	if o.code != ExitComplete || !second.Activated || second.Run != first.Run || second.Status != store.StatusSucceeded {
		t.Fatalf("resume: exit %d %+v", o.code, second)
	}
}

// TestOneShotInputChanged: a file that changes between hashing and the
// state's read fails its run rather than processing other bytes (ADR
// 0017, item 11).
func TestOneShotInputChanged(t *testing.T) {
	pr := newProbe()
	r := agent.NewRegistry()
	typ := pr.typ()
	read := typ.States[0].Code
	var input string
	typ.States[0].Code = func(ctx context.Context, env *agent.Env, d probeData) (agent.Transition[probeData], error) {
		writeFile(t, input, "tampered")
		return read(ctx, env, d)
	}
	if err := r.Register(typ); err != nil {
		t.Fatal(err)
	}
	p := Program{Registry: r, getenv: func(string) string { return "" }}
	dir := t.TempDir()
	path := writeConfig(t, dir, oneShotConfig(dir))
	input = filepath.Join(dir, "in.txt")
	writeFile(t, input, "original")
	o := runProgram(context.Background(), p, "run", "-config", path, "-input", input)
	rep := o.report(t)
	if o.code != ExitIncomplete || rep.Status != store.StatusFailed || rep.Reason.Code != agent.ReasonStateError || pr.readErrs != 1 || rep.Detail == "" {
		t.Fatalf("exit %d %+v", o.code, rep)
	}

	// Running the input again reports the recorded outcome, detail and
	// consumption included, without running it (Codex round 1 on 651edbc).
	writeFile(t, input, "original")
	o = runProgram(context.Background(), p, "run", "-config", path, "-input", input)
	again := o.report(t)
	if o.code != ExitIncomplete || again.Activated || again.Detail != rep.Detail || again.Consumed == nil || again.Status != store.StatusFailed {
		t.Fatalf("again: exit %d %+v, first %+v", o.code, again, rep)
	}
}

func TestOneShotAnotherConfigurationsRun(t *testing.T) {
	pr := newProbe()
	p := probeProgram(t, pr)
	other := pr.typ()
	other.Name = "other"
	if err := p.Registry.Register(other); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := writeConfig(t, dir, oneShotConfig(dir))
	input := filepath.Join(dir, "in.txt")
	writeFile(t, input, "fine")
	if o := runProgram(context.Background(), p, "run", "-config", path, "-input", input); o.code != ExitComplete {
		t.Fatalf("exit %d", o.code)
	}
	cfg := oneShotConfig(dir)
	cfg["agent_type"] = "other"
	path = writeConfig(t, dir, cfg)
	o := runProgram(context.Background(), p, "run", "-config", path, "-input", input)
	if o.code != ExitIncomplete || !strings.Contains(o.stderr.String(), "another configuration") {
		t.Fatalf("exit %d: %s", o.code, o.stderr.String())
	}
}

func TestTelemetrySinks(t *testing.T) {
	pr := newProbe()
	p := probeProgram(t, pr)
	dir := t.TempDir()
	input := filepath.Join(dir, "in.txt")
	writeFile(t, input, "fine")

	cfg := oneShotConfig(dir)
	cfg["telemetry"] = map[string]any{"sink": "jsonl", "path": "events.jsonl", "required": true}
	path := writeConfig(t, dir, cfg)
	if o := runProgram(context.Background(), p, "run", "-config", path, "-input", input); o.code != ExitComplete {
		t.Fatalf("exit %d: %s", o.code, o.stderr.String())
	}
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	for sc := bufio.NewScanner(f); sc.Scan(); n++ {
		var ev agent.Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil || !strings.HasPrefix(ev.Run, "sha256-") {
			t.Fatalf("event %s: %v", sc.Text(), err)
		}
	}
	if n == 0 {
		t.Fatal("no events")
	}

	// A required sink that cannot be opened fails before any run (exit
	// 2); an optional one is a warning.
	writeFile(t, input, "fine again")
	cfg["telemetry"] = map[string]any{"sink": "jsonl", "path": "no/such/dir/events.jsonl", "required": true}
	path = writeConfig(t, dir, cfg)
	if o := runProgram(context.Background(), p, "run", "-config", path, "-input", input); o.code != ExitUsage {
		t.Fatalf("required: exit %d", o.code)
	}
	if len(pr.started) != 1 {
		t.Fatal("a run started without its required sink")
	}
	cfg["telemetry"] = map[string]any{"sink": "jsonl", "path": "no/such/dir/events.jsonl"}
	cfg["log"] = map[string]any{"level": "warn", "format": "json"}
	path = writeConfig(t, dir, cfg)
	o := runProgram(context.Background(), p, "run", "-config", path, "-input", input)
	if o.code != ExitComplete || !strings.Contains(o.stderr.String(), "optional telemetry sink unavailable") {
		t.Fatalf("optional: exit %d: %s", o.code, o.stderr.String())
	}
}

func TestStoreLockedIsAUsageError(t *testing.T) {
	p := probeProgram(t, newProbe())
	dir := t.TempDir()
	path := writeConfig(t, dir, oneShotConfig(dir))
	_ = openState(t, dir) // holds the directory's lock
	input := filepath.Join(dir, "in.txt")
	writeFile(t, input, "fine")
	if o := runProgram(context.Background(), p, "run", "-config", path, "-input", input); o.code != ExitUsage || !strings.Contains(o.stderr.String(), "store") {
		t.Fatalf("exit %d: %s", o.code, o.stderr.String())
	}
}

func folderConfig(dir, inbox string, concurrency int) map[string]any {
	cfg := oneShotConfig(dir)
	cfg["trigger"] = map[string]any{"kind": "folder", "dir": inbox, "pattern": "*.txt", "poll_interval": "100ms"}
	cfg["concurrency"] = concurrency
	return cfg
}

// TestFolderWatch: each new stable file becomes one run, a copy of a file
// already seen does not, runs execute at most concurrency at once, and a
// stop with nothing in flight exits 0 (ADR 0017, items 12, 14, 16, 22).
func TestFolderWatch(t *testing.T) {
	pr := newProbe()
	p := probeProgram(t, pr)
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	if err := os.Mkdir(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, dir, folderConfig(dir, inbox, 2))
	for i := range 5 {
		writeFile(t, filepath.Join(inbox, fmt.Sprintf("s%d.txt", i)), fmt.Sprintf("slow %d", i))
	}
	writeFile(t, filepath.Join(inbox, "ignored.pdf"), "not matched")
	writeFile(t, filepath.Join(inbox, "fail.txt"), "fail")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *runOutput)
	go func() { done <- runProgram(ctx, p, "run", "-config", path) }()
	ids := waitFor(t, pr.started, 6)
	// A copy of a file already seen is the same run.
	writeFile(t, filepath.Join(inbox, "dup.txt"), "slow 0")
	waitIdle(t, dir, pr, 6)
	cancel()
	o := <-done
	if o.code != ExitComplete {
		t.Fatalf("exit %d: %s", o.code, o.stderr.String())
	}
	select {
	case id := <-pr.started:
		t.Fatalf("an extra run started: %s (of %v)", id, ids)
	default:
	}
	pr.mu.Lock()
	maxRunning := pr.maxRunning
	pr.mu.Unlock()
	if maxRunning > 2 || maxRunning < 1 {
		t.Fatalf("%d ran at once with concurrency 2", maxRunning)
	}
	s := openState(t, dir)
	failed := 0
	for _, id := range ids {
		run, err := s.Run(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case run.Status == store.StatusFailed:
			failed++
		case run.Status != store.StatusSucceeded:
			t.Fatalf("run %s is %s", id, run.Status)
		}
		if run.TriggerKind != agent.TriggerFolder {
			t.Fatalf("run %s trigger kind %q", id, run.TriggerKind)
		}
	}
	if failed != 1 {
		t.Fatalf("%d failed runs", failed)
	}
	if strings.Contains(o.stderr.String(), "s0.txt") || strings.Contains(o.stderr.String(), inbox) {
		t.Fatalf("a log line names an input: %s", o.stderr.String())
	}
}

// waitIdle waits until the store's file has n terminal runs, by polling a
// second view: the file store is locked, so it polls the probe instead.
func waitIdle(t *testing.T, _ string, pr *probe, _ int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pr.mu.Lock()
		running := pr.running
		pr.mu.Unlock()
		if running == 0 {
			// Two poll intervals more for a duplicate to be seen.
			time.Sleep(400 * time.Millisecond)
			pr.mu.Lock()
			running = pr.running
			pr.mu.Unlock()
			if running == 0 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the probe never went idle")
}

// TestFolderWatchShutdownAndRestart: a shutdown with a run in flight exits
// 1 and leaves it runnable; the next watch resumes it at start-up, before
// any new file (ADR 0017, items 16, 21 and 22).
func TestFolderWatchShutdownAndRestart(t *testing.T) {
	pr := newProbe()
	p := probeProgram(t, pr)
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	if err := os.Mkdir(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, dir, folderConfig(dir, inbox, 1))
	writeFile(t, filepath.Join(inbox, "a.txt"), "slow")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *runOutput)
	go func() { done <- runProgram(ctx, p, "run", "-config", path) }()
	id := waitFor(t, pr.started, 1)[0]
	cancel()
	if o := <-done; o.code != ExitIncomplete {
		t.Fatalf("shutdown with a run in flight: exit %d: %s", o.code, o.stderr.String())
	}
	s := openState(t, dir)
	if run, err := s.Run(context.Background(), id); err != nil || run.Status != store.StatusRunnable {
		t.Fatalf("after shutdown: %+v, %v", run, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart: the run resumes from the store at start-up (the scan
	// would need two polls to see the file as stable, and then finds the
	// existing run).
	ctx, cancel = context.WithCancel(context.Background())
	go func() { done <- runProgram(ctx, p, "run", "-config", path) }()
	if got := waitFor(t, pr.started, 1)[0]; got != id {
		t.Fatalf("resumed %s, want %s", got, id)
	}
	waitIdle(t, dir, pr, 1)
	cancel()
	if o := <-done; o.code != ExitComplete {
		t.Fatalf("restart: exit %d: %s", o.code, o.stderr.String())
	}
	s = openState(t, dir)
	if run, err := s.Run(context.Background(), id); err != nil || run.Status != store.StatusSucceeded {
		t.Fatalf("after restart: %+v, %v", run, err)
	}
	if n := len(pr.started); n != 0 {
		t.Fatalf("%d more runs started", n)
	}
}

func TestFolderMissingIsAUsageError(t *testing.T) {
	p := probeProgram(t, newProbe())
	dir := t.TempDir()
	path := writeConfig(t, dir, folderConfig(dir, filepath.Join(dir, "absent"), 1))
	if o := runProgram(context.Background(), p, "run", "-config", path); o.code != ExitUsage {
		t.Fatalf("exit %d: %s", o.code, o.stderr.String())
	}
	file := filepath.Join(dir, "file")
	writeFile(t, file, "x")
	path = writeConfig(t, dir, folderConfig(dir, file, 1))
	if o := runProgram(context.Background(), p, "run", "-config", path); o.code != ExitUsage {
		t.Fatalf("not a directory: exit %d", o.code)
	}
	// A folder that exists but cannot be listed would be polled forever
	// and never seen into.
	if os.Geteuid() == 0 {
		return // root lists any folder
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	path = writeConfig(t, dir, folderConfig(dir, locked, 1))
	if o := runProgram(context.Background(), p, "run", "-config", path); o.code != ExitUsage || !strings.Contains(o.stderr.String(), "trigger.dir") {
		t.Fatalf("unlistable: exit %d: %s", o.code, o.stderr.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// TestOneShotOutcomeUnwritten: a succeeded run whose outcome cannot be
// written to standard output exits 1, since the caller never learns it
// succeeded (ADR 0017, item 22).
func TestOneShotOutcomeUnwritten(t *testing.T) {
	p := probeProgram(t, newProbe())
	dir := t.TempDir()
	path := writeConfig(t, dir, oneShotConfig(dir))
	input := filepath.Join(dir, "in.txt")
	writeFile(t, input, "statement")
	var stderr bytes.Buffer
	if code := Run(context.Background(), p, []string{"run", "-config", path, "-input", input}, failingWriter{}, &stderr); code != ExitIncomplete {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "cannot write the outcome") {
		t.Fatalf("stderr %s", stderr.String())
	}
	// The run itself succeeded and is recorded.
	o := runProgram(context.Background(), p, "run", "-config", path, "-input", input)
	if rep := o.report(t); o.code != ExitComplete || rep.Status != store.StatusSucceeded {
		t.Fatalf("exit %d, report %+v", o.code, rep)
	}
}

// TestMainSignals runs Main in a child process and sends it SIGTERM while
// a run is in flight: the run is left runnable and the exit code is 1
// (MODE-6, ADR 0017 items 21 and 22).
func TestMainSignals(t *testing.T) {
	if os.Getenv("STANDALONE_TEST_MAIN") != "" {
		pr := newProbe()
		go func() {
			<-pr.started
			fmt.Println("STARTED")
		}()
		for i, a := range os.Args {
			if a == "--" {
				os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
				break
			}
		}
		Main(probeProgram(t, pr))
		return
	}
	dir := t.TempDir()
	path := writeConfig(t, dir, oneShotConfig(dir))
	input := filepath.Join(dir, "in.txt")
	writeFile(t, input, "block until shutdown")
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainSignals$", "--", "run", "-config", path, "-input", input) //nolint:gosec // the test binary
	cmd.Env = append(os.Environ(), "STANDALONE_TEST_MAIN=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(stdout)
	var out strings.Builder
	for sc.Scan() {
		out.WriteString(sc.Text() + "\n")
		if sc.Text() == "STARTED" {
			if err = cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		}
	}
	err = cmd.Wait()
	code := cmd.ProcessState.ExitCode()
	if code != ExitIncomplete || !strings.Contains(out.String(), `"status": "runnable"`) {
		t.Fatalf("exit %d (%v): %s", code, err, out.String())
	}
}

// TestHeartbeat: the heartbeat renews the lease, stops quietly when the
// activation's own final commit released the claim, and cancels the
// activation when someone else claimed (ADR 0017, item 18).
func TestHeartbeat(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	var logs bytes.Buffer
	h := &host{st: st, heartbeat: 5 * time.Millisecond, logger: slog.New(slog.NewTextHandler(&logs, nil))}

	own, err := st.Claim(ctx, "i1", "me", leaseDuration)
	if err != nil {
		t.Fatal(err)
	}
	actx, cancel := context.WithCancelCause(ctx)
	beat := h.startHeartbeat(actx, cancel, own.Fence())
	time.Sleep(20 * time.Millisecond)
	if err = st.Release(ctx, own.Fence()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-beat:
	case <-time.After(5 * time.Second):
		t.Fatal("the heartbeat did not stop after the release")
	}
	if actx.Err() != nil || strings.Contains(logs.String(), "ownership lost") {
		t.Fatalf("our own release cancelled or logged: %v %s", context.Cause(actx), logs.String())
	}
	cancel(nil)

	own, err = st.Claim(ctx, "i2", "me", leaseDuration)
	if err != nil {
		t.Fatal(err)
	}
	actx, cancel = context.WithCancelCause(ctx)
	defer cancel(nil)
	beat = h.startHeartbeat(actx, cancel, own.Fence())
	if err := st.Release(ctx, own.Fence()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Claim(ctx, "i2", "thief", leaseDuration); err != nil {
		t.Fatal(err)
	}
	<-beat
	if !errors.Is(context.Cause(actx), errHeartbeatLost) || !strings.Contains(logs.String(), "ownership lost") {
		t.Fatalf("a stolen claim: cause %v, logs %s", context.Cause(actx), logs.String())
	}
}

// TestFolderWatchLeavesAnotherConfigurationsRuns: a claimable run of
// another agent type is not activated, and a file whose run is another
// configuration's is refused once (ADR 0017, item 9).
func TestFolderWatchLeavesAnotherConfigurationsRuns(t *testing.T) {
	pr := newProbe()
	p := probeProgram(t, pr)
	other := pr.typ()
	other.Name = "other"
	if err := p.Registry.Register(other); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	if err := os.Mkdir(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	// Leave a runnable run of "other": shut it down mid-run.
	cfg := oneShotConfig(dir)
	cfg["agent_type"] = "other"
	path := writeConfig(t, dir, cfg)
	input := filepath.Join(inbox, "a.txt")
	writeFile(t, input, "slow")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *runOutput)
	go func() { done <- runProgram(ctx, p, "run", "-config", path, "-input", input) }()
	waitFor(t, pr.started, 1)
	cancel()
	if rep := (<-done).report(t); rep.Status != store.StatusRunnable {
		t.Fatalf("other's run: %+v", rep)
	}

	path = writeConfig(t, dir, folderConfig(dir, inbox, 1))
	ctx, cancel = context.WithCancel(context.Background())
	go func() { done <- runProgram(ctx, p, "run", "-config", path) }()
	time.Sleep(600 * time.Millisecond) // several polls
	cancel()
	o := <-done
	if o.code != ExitComplete {
		t.Fatalf("exit %d: %s", o.code, o.stderr.String())
	}
	if n := len(pr.started); n != 0 {
		t.Fatalf("another configuration's run was activated (%d)", n)
	}
	if n := strings.Count(o.stderr.String(), "another_configuration"); n != 1 {
		t.Fatalf("refused %d times, want once: %s", n, o.stderr.String())
	}
}

// faultyStore injects failures into a store.
type faultyStore struct {
	store.Store
	mu             sync.Mutex
	claimFailures  int  // Claim calls to fail with a generic error
	claimableFatal bool // Claimable returns store.ErrUnusable
	heartbeatFatal bool // Heartbeat returns store.ErrUnusable
	claims         int
}

func (f *faultyStore) Heartbeat(ctx context.Context, fence store.Fence, lease time.Duration) (store.Ownership, error) {
	f.mu.Lock()
	fatal := f.heartbeatFatal
	f.mu.Unlock()
	if fatal {
		return store.Ownership{}, fmt.Errorf("poisoned: %w", store.ErrUnusable)
	}
	return f.Store.Heartbeat(ctx, fence, lease) //nolint:wrapcheck // a test double
}

func (f *faultyStore) Claim(ctx context.Context, instance, owner string, lease time.Duration) (store.Ownership, error) {
	f.mu.Lock()
	f.claims++
	fail := f.claimFailures > 0
	if fail {
		f.claimFailures--
	}
	f.mu.Unlock()
	if fail {
		return store.Ownership{}, errors.New("transient")
	}
	return f.Store.Claim(ctx, instance, owner, lease) //nolint:wrapcheck // a test double
}

func (f *faultyStore) Claimable(ctx context.Context) ([]store.Claimable, error) {
	f.mu.Lock()
	fatal := f.claimableFatal
	f.mu.Unlock()
	if fatal {
		return nil, fmt.Errorf("poisoned: %w", store.ErrUnusable)
	}
	return f.Store.Claimable(ctx) //nolint:wrapcheck // a test double
}

func watchHost(t *testing.T, pr *probe, st store.Store, inbox string) *host {
	t.Helper()
	p := probeProgram(t, pr)
	cfg := &Config{
		AgentType: "probe", Namespace: "n", Config: []byte(`{}`), Store: StoreConfig{Kind: StoreMemory},
		Trigger: TriggerConfig{Kind: agent.TriggerFolder, Dir: inbox, PollInterval: Duration(50 * time.Millisecond)},
	}
	return &host{
		cfg: cfg, st: st, ctrl: agent.NewController(st, p.Registry), logger: slog.New(slog.DiscardHandler),
		owner: "test", maxInput: DefaultMaxInputBytes, heartbeat: time.Second, now: time.Now,
	}
}

// TestWatchRetriesAfterAFailure: an activation without a confirmed
// outcome is retried after a delay (ADR 0017, item 17).
func TestWatchRetriesAfterAFailure(t *testing.T) {
	pr := newProbe()
	inbox := t.TempDir()
	st := &faultyStore{Store: memstore.New(), claimFailures: 1}
	h := watchHost(t, pr, st, inbox)
	writeFile(t, filepath.Join(inbox, "a.txt"), "fine")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() { done <- h.watch(ctx) }()
	start := time.Now()
	waitFor(t, pr.started, 1)
	if d := time.Since(start); d < minRetryDelay {
		t.Fatalf("retried after %s, before the %s delay", d, minRetryDelay)
	}
	waitIdle(t, "", pr, 1)
	cancel()
	if code := <-done; code != ExitComplete {
		t.Fatalf("exit %d", code)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.claims != 2 {
		t.Fatalf("%d claims", st.claims)
	}
}

// TestWatchStopsWhenAHeartbeatFindsTheStoreUnusable: the heartbeat
// cancels its activation and the watch stops with exit 1, even with every
// slot busy so that dispatch never lists Claimable (Codex round 1 on
// 651edbc; ADR 0017, item 10).
func TestWatchStopsWhenAHeartbeatFindsTheStoreUnusable(t *testing.T) {
	pr := newProbe()
	inbox := t.TempDir()
	st := &faultyStore{Store: memstore.New()}
	h := watchHost(t, pr, st, inbox)
	h.heartbeat = 20 * time.Millisecond
	writeFile(t, filepath.Join(inbox, "a.txt"), "block until shutdown")
	done := make(chan int)
	go func() { done <- h.watch(context.Background()) }()
	waitFor(t, pr.started, 1)
	st.mu.Lock()
	st.heartbeatFatal = true
	st.mu.Unlock()
	select {
	case code := <-done:
		if code != ExitIncomplete {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watch kept going after a heartbeat found the store unusable")
	}
}

// TestWatchStopsOnAnUnusableStore: a store that must be reopened ends the
// watch with exit 1 (ADR 0017, item 10).
func TestWatchStopsOnAnUnusableStore(t *testing.T) {
	st := &faultyStore{Store: memstore.New(), claimableFatal: true}
	h := watchHost(t, newProbe(), st, t.TempDir())
	done := make(chan int)
	go func() { done <- h.watch(context.Background()) }()
	select {
	case code := <-done:
		if code != ExitIncomplete {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watch kept going on an unusable store")
	}
}
