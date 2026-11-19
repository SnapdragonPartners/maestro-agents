package standalone

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// TestOpenRunsInputsInProcess: the in-process host runs inputs as
// `run -input` does, and its store, the memory store included, holds what
// they wrote (ADR 0020, item 2).
func TestOpenRunsInputsInProcess(t *testing.T) {
	pr := newProbe()
	p := probeProgram(t, pr)
	dir := t.TempDir()
	cfg := oneShotConfig(dir)
	cfg["store"] = map[string]any{"kind": "memory"}
	path := writeConfig(t, dir, cfg)
	ctx := context.Background()
	h, err := Open(ctx, p, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close() //nolint:errcheck // test
	if h.Namespace() != "intake" {
		t.Fatalf("namespace %q", h.Namespace())
	}

	good := filepath.Join(dir, "good.txt")
	writeFile(t, good, "fine")
	rep, err := h.RunInput(ctx, good)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Activated || rep.Status != store.StatusSucceeded || rep.Consumed == nil || rep.SHA256 == "" {
		t.Fatalf("report %+v", rep)
	}
	run, err := h.Store().Run(ctx, rep.Run)
	if err != nil || run.Status != store.StatusSucceeded || run.TriggerKind != agent.TriggerOneShot {
		t.Fatalf("run %+v, %v", run, err)
	}

	// The same input again is its recorded outcome.
	again, err := h.RunInput(ctx, good)
	if err != nil || again.Activated || again.Run != rep.Run || again.Status != store.StatusSucceeded {
		t.Fatalf("again %+v, %v", again, err)
	}
	if n := len(pr.started); n != 1 {
		t.Fatalf("the state ran %d times", n)
	}

	bad := filepath.Join(dir, "bad.txt")
	writeFile(t, bad, "fail")
	if rep, err = h.RunInput(ctx, bad); err != nil || rep.Status != store.StatusFailed {
		t.Fatalf("a failed run is a report: %+v, %v", rep, err)
	}

	var ie *inputError
	if _, err = h.RunInput(ctx, filepath.Join(dir, "missing.txt")); !errors.As(err, &ie) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing input: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestOpenFolderConfiguration: a folder configuration opens in process,
// to submit runs on its store while no watch holds it, but runs no input
// (ADR 0021 amends ADR 0020, item 2).
func TestOpenFolderConfiguration(t *testing.T) {
	n := &note{}
	p := noteProgram(t, newProbe(), n)
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	if err := os.Mkdir(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	folder := writeConfigFile(t, dir, folderConfig(dir, inbox, 1))
	h, err := Open(context.Background(), p, folder, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close() //nolint:errcheck // test
	writeFile(t, filepath.Join(inbox, "a.txt"), "fine")
	if _, err = h.RunInput(context.Background(), filepath.Join(inbox, "a.txt")); !errors.Is(err, ErrNotOneShot) {
		t.Fatalf("RunInput on a folder configuration: %v", err)
	}
	rep, err := h.Submit(context.Background(), Submission{AgentType: "note", Instance: "i", Payload: payload("x")})
	if err != nil || rep.Status != store.StatusSucceeded {
		t.Fatalf("Submit: %+v %v", rep, err)
	}
}

func TestOpenRefuses(t *testing.T) {
	p := probeProgram(t, newProbe())
	dir := t.TempDir()
	cfg := oneShotConfig(dir)
	cfg["agent_type"] = "nope"
	invalid := writeConfigFile(t, dir, cfg)
	if _, err := Open(context.Background(), p, invalid, io.Discard); err == nil {
		t.Fatal("an invalid configuration opened")
	}
}

// TestHostWatch: the in-process host watches a folder as `run` does, a
// submission runs while it watches, a second watch is refused, and a
// one_shot configuration does not watch (ADR 0022, item 1).
func TestHostWatch(t *testing.T) {
	pr := newProbe()
	n := &note{}
	p := noteProgram(t, pr, n)
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	if err := os.Mkdir(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	h, err := Open(context.Background(), p, writeConfigFile(t, dir, folderConfig(dir, inbox, 1)), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close() //nolint:errcheck // test
	ctx, cancel := context.WithCancel(context.Background())
	type ret struct {
		err  error
		code int
	}
	done := make(chan ret, 1)
	go func() {
		code, werr := h.Watch(ctx)
		done <- ret{code: code, err: werr}
	}()
	writeFile(t, filepath.Join(inbox, "a.txt"), "fine")
	waitFor(t, pr.started, 1)
	rep, err := h.Submit(context.Background(), Submission{AgentType: "note", Instance: "i", Payload: payload("x")})
	if err != nil || rep.Status != store.StatusSucceeded {
		t.Fatalf("Submit while watching: %+v %v", rep, err)
	}
	// A second watch is refused while the first runs.
	deadline := time.Now().Add(5 * time.Second)
	for !h.watching.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if code, werr := h.Watch(ctx); !errors.Is(werr, ErrWatching) || code != ExitUsage {
		t.Fatalf("second watch: %d %v", code, werr)
	}
	cancel()
	if r := <-done; r.err != nil || r.code != ExitComplete {
		t.Fatalf("watch ended %d %v", r.code, r.err)
	}

	one, err := Open(context.Background(), probeProgram(t, newProbe()), writeConfigFile(t, t.TempDir(), oneShotConfig(t.TempDir())), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close() //nolint:errcheck // test
	if code, err := one.Watch(context.Background()); !errors.Is(err, ErrNotFolder) || code != ExitUsage {
		t.Fatalf("one_shot watch: %d %v", code, err)
	}
}
