package standalone

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Host is the standalone host opened in process (ADR 0020, item 2): a
// harness or a test runs inputs one at a time, as `run -input` does, a
// caller submits runs of the program's submitted types (Submit, ADR 0021),
// a caller watches a folder in process (Watch, ADR 0022), and any of
// them reads what the runs wrote from the host's store. Runs on one
// instance are serialized; Submit and RunInput are safe for concurrent
// use, and safe beside Watch, which shares their per-instance locks. One
// Watch runs at a time (a second returns ErrWatching); it returns run's
// exit code when its context ends (or the store must be reopened).
// Close comes after Watch, Submit and RunInput have returned.
type Host struct {
	h       *host
	cleanup func()
	// watching is set while Watch runs.
	watching atomic.Bool
}

// ErrNotOneShot is RunInput's error for a configuration whose trigger is
// not one_shot.
var ErrNotOneShot = errors.New("standalone: running an input needs a one_shot trigger")

// Open loads and validates the configuration at configPath as `run` does,
// and builds the host, whatever its trigger: RunInput needs a one_shot
// trigger (ADR 0021 amends ADR 0020, item 2), and Watch a folder one (ADR
// 0022, item 1). The host logs to stderr.
// Close releases it.
func Open(ctx context.Context, p Program, configPath string, stderr io.Writer) (*Host, error) { //nolint:gocritic // hugeParam: called once per host
	cfg, err := loadAndValidate(&p, configPath)
	if err != nil {
		return nil, err
	}
	h, cleanup, err := setup(ctx, &p, cfg, stderr)
	if err != nil {
		return nil, err
	}
	return &Host{h: h, cleanup: cleanup}, nil
}

// RunInput makes the input's run and activates it if it is runnable,
// exactly as `run -input` does, and returns its report. A run that did not
// succeed is a report; the error is what `run -input` reports as a usage
// error or fails on: the input cannot be read (wrapping the cause), or its
// run cannot be made. Cancelling ctx shuts the activation down at the next
// step boundary.
func (h *Host) RunInput(ctx context.Context, path string) (Report, error) {
	if k := h.h.cfg.Trigger.Kind; k != agent.TriggerOneShot {
		return Report{}, fmt.Errorf("%w (the configuration has %q)", ErrNotOneShot, k)
	}
	rep, err := h.h.runInput(ctx, path)
	if err != nil {
		return Report{}, fmt.Errorf("standalone: input %s: %w", path, err)
	}
	return rep, nil
}

// ErrNotFolder is Watch's error for a configuration whose trigger is not
// folder.
var ErrNotFolder = errors.New("standalone: watching needs a folder trigger")

// ErrWatching is Watch's error while another Watch runs on the host.
var ErrWatching = errors.New("standalone: the host is already watching")

// Watch runs the folder watch in process, as `run` does for a folder
// trigger, until ctx ends or the store must be reopened, and returns run's
// exit code (ADR 0022, item 1). Submit may be called while it runs: they
// share the per-instance locks (ADR 0021, item 3). It returns ErrNotFolder
// or ErrWatching at once, with ExitUsage.
func (h *Host) Watch(ctx context.Context) (int, error) {
	if k := h.h.cfg.Trigger.Kind; k != agent.TriggerFolder {
		return ExitUsage, fmt.Errorf("%w (the configuration has %q)", ErrNotFolder, k)
	}
	if !h.watching.CompareAndSwap(false, true) {
		return ExitUsage, ErrWatching
	}
	defer h.watching.Store(false)
	return h.h.watch(ctx), nil
}

// Store is the host's store, for reading what the runs wrote.
func (h *Host) Store() store.Reader { return h.h.st }

// TriggerKind is the configuration's trigger kind.
func (h *Host) TriggerKind() string { return h.h.cfg.Trigger.Kind }

// Namespace is the object-store namespace the host's runs write in.
func (h *Host) Namespace() string { return h.h.cfg.Namespace }

// Close releases the host's store and telemetry sink. It is idempotent.
func (h *Host) Close() error {
	if h.cleanup != nil {
		h.cleanup()
		h.cleanup = nil
	}
	return nil
}
