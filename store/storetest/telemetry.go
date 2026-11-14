package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// testTelemetry checks store.Telemetry for a store that offers it (ADR
// 0016, item 7): idempotent appends on (run, sequence), ordered reads,
// validation before any write and, for a durable store, survival across a
// reopen.
func testTelemetry(t *testing.T, h Harness) {
	s := h.Open(t, Options{Now: NewClock().Now})
	tel, ok := s.(store.Telemetry)
	if !ok {
		_ = s.Close()
		t.Skip("store does not implement store.Telemetry")
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	at := time.Date(2026, 11, 13, 12, 0, 0, 0, time.FixedZone("x", 3600))
	ev := func(run string, seq uint64, kind, payload string) store.Event {
		return store.Event{Run: run, Seq: seq, Kind: kind, At: at, Payload: json.RawMessage(payload)}
	}
	if got, err := tel.Events(ctx, "run/1", 0); err != nil || len(got) != 0 {
		t.Fatalf("Events with none = %+v, %v", got, err)
	}
	// Out of order, with a repeat inside the batch: the first is kept.
	if err := tel.AppendEvents(ctx, []store.Event{
		ev("run/1", 2, "turn", `{"n":2}`), ev("run/1", 1, "run_started", `{"n":1}`), ev("run/1", 2, "turn", `{"n":"dup"}`),
		ev("run/2", 1, "run_started", `{}`),
	}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}
	// A redelivered event keeps the stored one; a new one is added.
	if err := tel.AppendEvents(ctx, []store.Event{ev("run/1", 1, "other", `{"n":"again"}`), ev("run/1", 3, "outcome", `{"n":3}`)}); err != nil {
		t.Fatalf("AppendEvents again: %v", err)
	}
	check := func(s store.Telemetry) {
		t.Helper()
		got, err := s.Events(ctx, "run/1", 0)
		if err != nil || len(got) != 3 {
			t.Fatalf("Events = %+v, %v", got, err)
		}
		want := []struct {
			kind, payload string
		}{{"run_started", `{"n":1}`}, {"turn", `{"n":2}`}, {"outcome", `{"n":3}`}}
		for i, w := range want {
			g := got[i]
			if g.Run != "run/1" || g.Seq != uint64(i+1) || g.Kind != w.kind || !bytes.Equal(g.Payload, []byte(w.payload)) || !g.At.Equal(at) {
				t.Fatalf("event %d = %+v, want kind %s payload %s", i, g, w.kind, w.payload)
			}
		}
		after, err := s.Events(ctx, "run/1", 2)
		if err != nil || len(after) != 1 || after[0].Seq != 3 {
			t.Fatalf("Events(after 2) = %+v, %v", after, err)
		}
		other, err := s.Events(ctx, "run/2", 0)
		if err != nil || len(other) != 1 {
			t.Fatalf("Events(run/2) = %+v, %v", other, err)
		}
	}
	check(tel)
	// Validation refuses the whole batch.
	for name, bad := range map[string]store.Event{
		"empty run":    ev("", 1, "k", `{}`),
		"zero seq":     ev("run/1", 0, "k", `{}`),
		"invalid kind": ev("run/1", 9, "k\xff", `{}`),
		"invalid json": ev("run/1", 9, "k", `{`),
		"invalid utf8": ev("run/1", 9, "k", "\"\xff\""),
	} {
		err := tel.AppendEvents(ctx, []store.Event{ev("run/1", 8, "k", `{}`), bad})
		wantErr(t, err, store.ErrInvalid)
		if got, _ := tel.Events(ctx, "run/1", 7); len(got) != 0 {
			t.Fatalf("%s: AppendEvents wrote %+v despite refusing the batch", name, got)
		}
	}
	if _, err := tel.Events(ctx, "", 0); err == nil {
		t.Fatal("Events with an empty run succeeded")
	}
	if h.Reopen == nil {
		return
	}
	s = h.Reopen(t, s, Options{Now: NewClock().Now})
	reopened, ok := s.(store.Telemetry)
	if !ok {
		t.Fatal("reopened store does not implement store.Telemetry")
	}
	check(reopened)
}
