package standalone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// jsonlSink appends each event to a file as one JSON line (ADR 0017, item
// 20). Emit is safe for concurrent use; a batch is written in one call, so
// batches never interleave. Events are in commit order within an
// activation only (ADR 0016, item 4): a reader that needs a run's events in
// order sorts them by seq.
type jsonlSink struct {
	w  io.WriteCloser
	mu sync.Mutex
}

// openJSONL opens path for appending, creating it owner-only.
func openJSONL(path string) (*jsonlSink, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) //nolint:gosec // G304: the operator names the file
	if err != nil {
		return nil, err //nolint:wrapcheck // the PathError names the file
	}
	return &jsonlSink{w: f}, nil
}

func (s *jsonlSink) Emit(_ context.Context, events []agent.Event) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := range events {
		if err := enc.Encode(&events[i]); err != nil {
			return fmt.Errorf("telemetry: encode event: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	return nil
}

func (s *jsonlSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Close() //nolint:wrapcheck // the PathError names the file
}

// openSink sets up the configured telemetry sink (MODE-7). It returns a
// nil sink for none; a closer to call at exit, possibly nil; and an error
// only when the sink cannot be set up, which the caller treats by
// telemetry.required.
func openSink(c *TelemetryConfig, st store.Store) (agent.Sink, io.Closer, error) {
	switch c.Sink {
	case SinkStore:
		t, ok := st.(store.Telemetry)
		if !ok {
			return nil, nil, errors.New("the store does not offer telemetry storage")
		}
		return agent.StoreSink(t), nil, nil
	case SinkJSONL:
		s, err := openJSONL(c.Path)
		if err != nil {
			return nil, nil, err
		}
		return s, s, nil
	}
	return nil, nil, nil
}
