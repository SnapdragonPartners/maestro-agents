package standalone

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"testing"

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

func TestOpenRefuses(t *testing.T) {
	p := probeProgram(t, newProbe())
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	folder := writeConfigFile(t, dir, folderConfig(dir, inbox, 1))
	if _, err := Open(context.Background(), p, folder, io.Discard); !errors.Is(err, ErrNotOneShot) {
		t.Fatalf("folder trigger: %v", err)
	}
	cfg := oneShotConfig(dir)
	cfg["agent_type"] = "nope"
	invalid := writeConfigFile(t, dir, cfg)
	if _, err := Open(context.Background(), p, invalid, io.Discard); err == nil {
		t.Fatal("an invalid configuration opened")
	}
}
