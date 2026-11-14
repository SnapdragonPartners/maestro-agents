package storecore

import (
	"context"
	"fmt"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

func telemetryDir(run string) string { return "telemetry/" + run }

// AppendEvents implements store.Telemetry (ADR 0016, item 8). Telemetry is
// not fenced and does not go through Commit; it shares the engine's single
// writer so its files are written atomically like every other.
func (e *Engine) AppendEvents(ctx context.Context, events []store.Event) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("store: append events not started: %w", err)
	}
	paths := make([]string, len(events))
	for i := range events {
		ev := &events[i]
		run, err := component("event run", ev.Run)
		if err != nil {
			return fmt.Errorf("event %d: %w", i, err)
		}
		if ev.Seq == 0 {
			return fmt.Errorf("event %d: %w: sequence numbers start at 1", i, store.ErrInvalid)
		}
		if err = text("event kind", ev.Kind); err != nil {
			return fmt.Errorf("event %d: %w", i, err)
		}
		if err = rawJSON("event payload", ev.Payload); err != nil {
			return fmt.Errorf("event %d: %w", i, err)
		}
		paths[i] = telemetryDir(run) + "/" + seqName(ev.Seq)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.usable(); err != nil {
		return err
	}
	t := newTx(e.b)
	for i := range events {
		_, found, err := t.Read(paths[i])
		if err != nil {
			return err
		}
		if found {
			continue // the first event written with an identity is kept
		}
		ev := events[i]
		ev.At = ev.At.UTC()
		if err = t.putJSON(paths[i], ev); err != nil {
			return err
		}
	}
	if len(t.order) == 0 {
		return nil
	}
	return e.apply(t.order)
}

// Events implements store.Telemetry.
func (e *Engine) Events(ctx context.Context, run string, after uint64) ([]store.Event, error) {
	enc, err := component("run", run)
	if err != nil {
		return nil, err
	}
	if err = e.rlock(ctx); err != nil {
		return nil, err
	}
	defer e.mu.RUnlock()
	return readSeqDir[store.Event](e, telemetryDir(enc), after)
}

var _ store.Telemetry = (*Engine)(nil)
