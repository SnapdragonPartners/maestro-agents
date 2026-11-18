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
// harness or a test runs inputs one at a time, as `run -input` does, and
// reads what their runs wrote from the host's store. It is not safe for
// concurrent use.
type Host struct {
	h       *host
	cleanup func()
}

// ErrNotOneShot is Open's error for a configuration whose trigger is not
// one_shot.
var ErrNotOneShot = errors.New("standalone: the in-process host needs a one_shot trigger")

// Open loads and validates the configuration at configPath as `run` does,
// and builds the host. Only a one_shot trigger is accepted. The host logs
// to stderr. Close releases it.
func Open(ctx context.Context, p Program, configPath string, stderr io.Writer) (*Host, error) { //nolint:gocritic // hugeParam: called once per host
	cfg, err := loadAndValidate(&p, configPath)
	if err != nil {
		return nil, err
	}
	if cfg.Trigger.Kind != agent.TriggerOneShot {
		return nil, fmt.Errorf("%w (config %s has %q)", ErrNotOneShot, configPath, cfg.Trigger.Kind)
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
	rep, err := h.h.runInput(ctx, path)
	if err != nil {
		return Report{}, fmt.Errorf("standalone: input %s: %w", path, err)
	}
	return rep, nil
}

// Store is the host's store, for reading what the runs wrote.
func (h *Host) Store() store.Reader { return h.h.st }

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
