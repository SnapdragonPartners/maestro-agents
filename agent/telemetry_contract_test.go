package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/memstore"
)

// These tests check the Sink contract of ADR 0016 item 4: the bound on
// each call, a context that outlives the activation, ordering only within
// an activation, and concurrent calls.

func createCounterRun(t testing.TB, s store.Store, id, instance string, target int) {
	t.Helper()
	_, _, err := s.CreateRun(context.Background(), store.Run{
		ID: id, Instance: instance, AgentType: "counter", TriggerID: "trig-" + id, TriggerKind: agent.TriggerOneShot,
		Trigger: json.RawMessage(`{"target":` + strconv.Itoa(target) + `}`),
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
}

func claimInstance(t testing.TB, s store.Store, instance string) store.Fence {
	t.Helper()
	own, err := s.Claim(context.Background(), instance, "host", time.Minute)
	if err != nil {
		t.Fatalf("Claim %s: %v", instance, err)
	}
	return own.Fence()
}

func seqs(evs []agent.Event) []uint64 {
	out := make([]uint64, len(evs))
	for i := range evs {
		out[i] = evs[i].Seq
	}
	return out
}

func entrySeqs(es []store.Entry) []uint64 {
	out := make([]uint64, len(es))
	for i := range es {
		out[i] = es[i].Seq
	}
	return out
}

func equalSeqs(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// blockingSink waits for its context to end, as a hung sink would.
type blockingSink struct{}

func (blockingSink) Emit(ctx context.Context, _ []agent.Event) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestTelemetrySinkIsBounded(t *testing.T) {
	var logs bytes.Buffer
	s := memstore.New()
	c := agent.NewController(s, registry(t, counterType()), agent.WithTelemetry(blockingSink{}),
		agent.WithTelemetryTimeout(20*time.Millisecond), agent.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	createCounterRun(t, s, "r1", inst, 2)
	start := time.Now()
	out, err := activate(context.Background(), c, claim(t, s), "r1", 0)
	if err != nil || !out.Status.Terminal() {
		t.Fatalf("activation = %+v, %v", out, err)
	}
	// Every commit's sink call waited for its bound, never longer: the
	// activation finished well inside one default TelemetryTimeout.
	if elapsed := time.Since(start); elapsed >= agent.TelemetryTimeout {
		t.Fatalf("activation took %v", elapsed)
	}
	if !strings.Contains(logs.String(), `"failure":"sink_timeout"`) {
		t.Fatalf("no sink_timeout warning: %s", logs.String())
	}
}

// ctxSink records, per call, whether its context was live and bounded, and
// runs onEmit (outside its lock) before recording.
type ctxSink struct {
	onEmit  func()
	batches [][]agent.Event
	live    []bool
	mu      sync.Mutex
}

func (s *ctxSink) Emit(ctx context.Context, evs []agent.Event) error {
	if s.onEmit != nil {
		s.onEmit()
	}
	deadline, ok := ctx.Deadline()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live = append(s.live, ctx.Err() == nil && ok && time.Until(deadline) <= agent.TelemetryTimeout)
	s.batches = append(s.batches, append([]agent.Event(nil), evs...))
	return nil
}

func TestTelemetryOutlivesShutdown(t *testing.T) {
	s := memstore.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &ctxSink{}
	var once sync.Once
	// The first emission (activation_started's) shuts the activation down.
	sink.onEmit = func() { once.Do(cancel) }
	c := agent.NewController(s, registry(t, counterType()), agent.WithTelemetry(sink))
	createCounterRun(t, s, "r1", inst, 5)
	out, err := activate(ctx, c, claim(t, s), "r1", 0)
	if err != nil || out.Status != store.StatusRunnable || out.Reason.Code != agent.ReasonShutdown {
		t.Fatalf("activation = %+v, %v", out, err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for i, live := range sink.live {
		if !live {
			t.Fatalf("emission %d had a done or unbounded context", i)
		}
	}
	last := sink.batches[len(sink.batches)-1]
	if ev := last[len(last)-1]; ev.Kind != agent.EntryOutcome || ev.Reason.Code != agent.ReasonShutdown {
		t.Fatalf("final event = %+v, want the shutdown outcome", ev)
	}
}

// handoffSink holds the first batch carrying an outcome until released,
// and records each batch when its call returns, so batches appear in
// delivery-completion order.
type handoffSink struct {
	held    chan struct{}
	release chan struct{}
	batches [][]agent.Event
	mu      sync.Mutex
	holding atomic.Bool
}

func (s *handoffSink) Emit(_ context.Context, evs []agent.Event) error {
	if evs[len(evs)-1].Kind == agent.EntryOutcome {
		// Not sync.Once: its Do would make the next owner's outcome wait.
		if s.holding.CompareAndSwap(false, true) {
			close(s.held)
			<-s.release
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, append([]agent.Event(nil), evs...))
	return nil
}

// TestTelemetryAcrossAHandoff shows what ADR 0016 item 4 allows: the next
// owner emits while the previous owner's final batch is still in the
// sink, so batches arrive out of order, but identities are unique and
// cover every entry, so a sink can order them by sequence.
func TestTelemetryAcrossAHandoff(t *testing.T) {
	s := memstore.New()
	sink := &handoffSink{held: make(chan struct{}), release: make(chan struct{})}
	c := agent.NewController(s, registry(t, counterType()), agent.WithTelemetry(sink))
	createCounterRun(t, s, "r1", inst, 3)

	type result struct {
		err error
		out agent.Outcome
	}
	first := make(chan result, 1)
	go func() {
		out, err := activate(context.Background(), c, claim(t, s), "r1", 1)
		first <- result{out: out, err: err}
	}()
	<-sink.held // the first activation's final commit landed and released
	out, err := activate(context.Background(), c, claim(t, s), "r1", 0)
	if err != nil || !out.Status.Terminal() {
		t.Fatalf("second activation = %+v, %v", out, err)
	}
	close(sink.release)
	if r := <-first; r.err != nil || r.out.Status != store.StatusRunnable {
		t.Fatalf("first activation = %+v, %v", r.out, r.err)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	held := sink.batches[len(sink.batches)-1]
	if held[len(held)-1].Kind != agent.EntryOutcome || held[len(held)-1].Reason.Code != agent.ReasonStepLimit {
		t.Fatalf("last delivered batch = %+v, want the first activation's ending", held)
	}
	var all []agent.Event
	for _, b := range sink.batches {
		all = append(all, b...)
	}
	got := seqs(all)
	if equalSeqs(got, entrySeqs(entries(t, s, "r1"))) {
		t.Fatal("batches arrived in order; the handoff did not overlap")
	}
	byseq := map[uint64]bool{}
	for _, q := range got {
		if byseq[q] {
			t.Fatalf("sequence %d delivered twice", q)
		}
		byseq[q] = true
	}
	for _, e := range entries(t, s, "r1") {
		if !byseq[e.Seq] {
			t.Fatalf("entry %d never delivered", e.Seq)
		}
	}
}

// TestTelemetryConcurrentActivations runs two instances on one controller
// at once (run under -race): the sink, redactor and logger are shared, and
// each run's events arrive complete and, within its one activation, in
// order.
func TestTelemetryConcurrentActivations(t *testing.T) {
	s := memstore.New()
	sink := &recSink{}
	var redactMu sync.Mutex
	redacted := 0
	redact := func(ev *agent.Event) {
		redactMu.Lock()
		redacted++
		redactMu.Unlock()
		ev.Detail = ""
	}
	var logs syncBuffer
	c := agent.NewController(s, registry(t, counterType()), agent.WithTelemetry(sink), agent.WithRedactor(redact),
		agent.WithLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	runs := map[string]string{"ra": "inst-a", "rb": "inst-b"}
	for id, instance := range runs {
		createCounterRun(t, s, id, instance, 4)
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(runs))
	for id, instance := range runs {
		f := claimInstance(t, s, instance)
		wg.Go(func() {
			out, err := activate(context.Background(), c, f, id, 0)
			if err == nil && !out.Status.Terminal() {
				err = context.Canceled // any non-nil marks the failure
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("activation: %v", err)
		}
	}
	total := 0
	for id := range runs {
		var mine []agent.Event
		for _, ev := range sink.events() {
			if ev.Run == id {
				mine = append(mine, ev)
			}
		}
		want := entrySeqs(entries(t, s, id))
		if !equalSeqs(seqs(mine), want) {
			t.Fatalf("run %s events %v, want %v", id, seqs(mine), want)
		}
		total += len(mine)
	}
	if redacted != total {
		t.Fatalf("redactor saw %d events, sink %d", redacted, total)
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent writes.
type syncBuffer struct {
	b  bytes.Buffer
	mu sync.Mutex
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p) //nolint:wrapcheck // a test buffer
}
