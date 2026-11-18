package standalone

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Host is the standalone host opened in process (ADR 0020, item 2): a
// harness or a test runs inputs one at a time, as `run -input` does, a
// caller submits runs of the program's submitted types (Submit, ADR 0021),
// and either reads what the runs wrote from the host's store. Runs on one
// instance are serialized; Submit and RunInput are safe for concurrent
// use.
type Host struct {
	h       *host
	cleanup func()
}

// ErrNotOneShot is RunInput's error for a configuration whose trigger is
// not one_shot.
var ErrNotOneShot = errors.New("standalone: running an input needs a one_shot trigger")

// Open loads and validates the configuration at configPath as `run` does,
// and builds the host, whatever its trigger: the in-process host never
// watches a folder, and RunInput needs a one_shot trigger (ADR 0021
// amends ADR 0020, item 2), so a folder configuration's store can be
// opened to submit runs while no watch holds it. The host logs to stderr.
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
