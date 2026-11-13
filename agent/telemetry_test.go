package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/memstore"
)

// recSink records what it is given.
type recSink struct {
	err     error
	batches [][]agent.Event
	mu      sync.Mutex
	panics  bool
}

func (s *recSink) Emit(_ context.Context, evs []agent.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.panics {
		panic("sink exploded")
	}
	s.batches = append(s.batches, append([]agent.Event(nil), evs...))
	return s.err
}

func (s *recSink) events() []agent.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []agent.Event
	for _, b := range s.batches {
		out = append(out, b...)
	}
	return out
}

// leakScript records an item and calls lookup, which leakPolicy denies,
// then finishes. Every free-text and content field it produces carries
// "LEAK", so telemetry can be searched for it.
var leakScript = []llms.ChatResponse{
	callsResp(tc("c1", "record", `{"item":"LEAK-ARG"}`), tc("c2", "lookup", `{"key":"LEAK-KEY"}`)),
	callsResp(tc("c3", "finish", `{}`)),
}

func leakPolicy(_ context.Context, req *agent.PolicyRequest) (agent.Decision, error) {
	if req.Tool == "lookup" {
		return agent.Decision{Verdict: agent.VerdictDeny, Reason: "LEAK-REASON"}, nil
	}
	return agent.Decision{Verdict: agent.VerdictAllow}, nil
}

func createLeakRun(t testing.TB, s store.Store) {
	t.Helper()
	_, _, err := s.CreateRun(context.Background(), store.Run{
		ID: "r1", Instance: inst, AgentType: "ledger", TriggerID: "trig-r1", TriggerKind: agent.TriggerOneShot,
		Trigger: json.RawMessage(`{"doc":"LEAK-TRIGGER"}`),
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
}

func activateLeak(c *agent.Controller, f store.Fence, maxSteps int) (agent.Outcome, error) {
	return c.Activate(context.Background(), agent.ActivationInput{
		Fence: f, Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}, Namespace: ns, Client: modelClient(scripted(leakScript)),
		Config: json.RawMessage(`{"secret":"LEAK-CONFIG"}`), Principal: "LEAK-PRINCIPAL", MaxSteps: maxSteps,
	})
}

// runLeak runs the leak script in two activations (the first stopped after
// one step) and returns the store.
func runLeak(t *testing.T, opts ...agent.Option) store.Store {
	t.Helper()
	s := memstore.New()
	var execs execLog
	opts = append([]agent.Option{agent.WithPolicy(leakPolicy)}, opts...)
	c := agent.NewController(s, registry(t, ledgerType(&execs)), opts...)
	createLeakRun(t, s)
	out, err := activateLeak(c, claim(t, s), 1)
	if err != nil || out.Status != store.StatusRunnable {
		t.Fatalf("first activation = %+v, %v", out, err)
	}
	out, err = activateLeak(c, claim(t, s), 0)
	if err != nil || !out.Status.Terminal() {
		t.Fatalf("second activation = %+v, %v", out, err)
	}
	return s
}

func TestTelemetryProjectsEveryCommittedEntry(t *testing.T) {
	sink := &recSink{}
	s := runLeak(t, agent.WithTelemetry(sink))
	entries, err := s.Entries(context.Background(), "r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	evs := sink.events()
	if len(evs) != len(entries) {
		t.Fatalf("%d events for %d entries", len(evs), len(entries))
	}
	kinds := map[string]bool{}
	for i, ev := range evs {
		e := entries[i]
		if ev.Seq != e.Seq || ev.Kind != e.Kind || ev.Step != e.Step || ev.Run != "r1" || ev.Instance != inst ||
			ev.AgentType != "ledger" || ev.At.IsZero() {
			t.Fatalf("event %d = %+v for entry seq %d kind %s step %d", i, ev, e.Seq, e.Kind, e.Step)
		}
		kinds[ev.Kind] = true
	}
	for _, k := range []string{
		agent.EntryActivationStarted, agent.EntryRunStarted, agent.EntryModelAttempt, agent.EntryTurn, agent.EntryPermission,
		agent.EntryAttempt, agent.EntryResult, agent.EntryTransition, agent.EntryOutcome,
	} {
		if !kinds[k] {
			t.Errorf("no %s event", k)
		}
	}
	// Each batch is one commit's events, in sequence order.
	for _, b := range sink.batches {
		for i := 1; i < len(b); i++ {
			if b[i].Seq != b[i-1].Seq+1 {
				t.Fatalf("batch out of order: %+v", b)
			}
		}
	}
	// Projections carry their entry's metadata.
	var sawTurn, sawDenied, sawOutcome, sawStart bool
	for _, ev := range evs {
		switch ev.Kind {
		case agent.EntryTurn:
			sawTurn = true
			if ev.Model == nil || ev.Model.Name != "scripted" || ev.Usage == nil || ev.Usage.InputTokens != 10 || ev.Calls == 0 {
				t.Errorf("turn event = %+v", ev)
			}
		case agent.EntryPermission:
			if ev.Tool == "lookup" {
				sawDenied = true
				if ev.Permission != agent.PermissionDenied || ev.Call != 1 || ev.Effect != agent.EffectRead {
					t.Errorf("denied permission event = %+v", ev)
				}
			}
		case agent.EntryRunStarted:
			sawStart = true
			if ev.TriggerID != "trig-r1" || ev.TriggerKind != agent.TriggerOneShot || ev.Namespace != ns {
				t.Errorf("run_started event = %+v", ev)
			}
		case agent.EntryOutcome:
			if ev.Status.Terminal() {
				sawOutcome = true
				if ev.Consumed == nil || ev.Consumed.Turns != 2 {
					t.Errorf("outcome event = %+v", ev)
				}
			}
		case agent.EntryActivationStarted:
			if ev.Owner != "host" || ev.Epoch == 0 {
				t.Errorf("activation_started event = %+v", ev)
			}
		}
	}
	if !sawTurn || !sawDenied || !sawOutcome || !sawStart {
		t.Fatalf("turn %v, denied %v, outcome %v, start %v", sawTurn, sawDenied, sawOutcome, sawStart)
	}
}

// TestTelemetryCarriesNoContent checks ADR 0016 items 2 and 3: no event
// carries arguments, results, trigger, configuration or principal, and free
// text leaves only through a redactor.
func TestTelemetryCarriesNoContent(t *testing.T) {
	sink := &recSink{}
	s := runLeak(t, agent.WithTelemetry(sink))
	// The run did produce the content: the record tool stored its argument.
	if items := ledgerItems(t, s); len(items) != 1 || items[0] != "LEAK-ARG" {
		t.Fatalf("ledger = %v; the leak run did not execute record", items)
	}
	raw, err := json.Marshal(sink.events())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("LEAK")) {
		t.Fatalf("telemetry leaks content: %s", raw)
	}

	// A redactor sees, and may keep, the free text; content is never
	// projected at all.
	kept := &recSink{}
	var seen []string
	runLeak(t, agent.WithTelemetry(kept), agent.WithRedactor(func(ev *agent.Event) {
		if ev.Detail != "" {
			seen = append(seen, ev.Detail)
		}
	}))
	raw, err = json.Marshal(kept.events())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("LEAK-REASON")) || len(seen) == 0 {
		t.Fatalf("redactor did not keep the policy reason: %s", raw)
	}
	for _, leak := range []string{"LEAK-ARG", "LEAK-KEY", "LEAK-TRIGGER", "LEAK-CONFIG", "LEAK-PRINCIPAL"} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Errorf("telemetry carries %s: %s", leak, raw)
		}
	}

	// A redactor that strips the detail is obeyed.
	stripped := &recSink{}
	runLeak(t, agent.WithTelemetry(stripped), agent.WithRedactor(func(ev *agent.Event) { ev.Detail = "redacted" }))
	raw, _ = json.Marshal(stripped.events())
	if bytes.Contains(raw, []byte("LEAK")) {
		t.Fatalf("redacted telemetry leaks: %s", raw)
	}
}

func TestTelemetryFailureNeverFailsTheRun(t *testing.T) {
	for name, sink := range map[string]*recSink{
		"error": {err: errors.New("sink down")},
		"panic": {panics: true},
	} {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			s := runLeak(t, agent.WithTelemetry(sink), agent.WithLogger(logger))
			run, err := s.Run(context.Background(), "r1")
			if err != nil || !run.Status.Terminal() {
				t.Fatalf("run = %+v, %v", run, err)
			}
			if !strings.Contains(logs.String(), "telemetry not delivered") {
				t.Fatalf("no warning logged: %s", logs.String())
			}
		})
	}
	// A panicking redactor drops the batch, with a warning.
	var logs bytes.Buffer
	sink := &recSink{}
	runLeak(t, agent.WithTelemetry(sink), agent.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))),
		agent.WithRedactor(func(*agent.Event) { panic("redactor exploded") }))
	if len(sink.events()) != 0 || !strings.Contains(logs.String(), "redactor_panic") {
		t.Fatalf("events %d, logs %s", len(sink.events()), logs.String())
	}
}

// TestTelemetryFailureLogsNoContent checks ADR 0016 items 5 and 10: a
// failure is logged by class, never by its text, which may quote content.
func TestTelemetryFailureLogsNoContent(t *testing.T) {
	cases := map[string]struct {
		opts  []agent.Option
		class string
	}{
		"redactor panics with content": {
			opts:  []agent.Option{agent.WithTelemetry(&recSink{}), agent.WithRedactor(func(ev *agent.Event) { panic("LEAK " + ev.Detail) })},
			class: "redactor_panic",
		},
		"sink errs with content": {
			opts:  []agent.Option{agent.WithTelemetry(contentSink{err: errors.New("LEAK-SINK")})},
			class: "sink_error",
		},
		"sink panics with content": {
			opts:  []agent.Option{agent.WithTelemetry(contentSink{panics: true})},
			class: "sink_panic",
		},
		"sink times out": {
			opts:  []agent.Option{agent.WithTelemetry(contentSink{err: fmt.Errorf("LEAK-SINK: %w", context.DeadlineExceeded)})},
			class: "sink_timeout",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			runLeak(t, append(c.opts, agent.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))...)
			if !strings.Contains(logs.String(), `"failure":"`+c.class+`"`) {
				t.Fatalf("no %s warning: %s", c.class, logs.String())
			}
			if strings.Contains(logs.String(), "LEAK") {
				t.Fatalf("log leaks failure text: %s", logs.String())
			}
		})
	}
}

// contentSink fails with content-bearing text.
type contentSink struct {
	err    error
	panics bool
}

func (s contentSink) Emit(context.Context, []agent.Event) error {
	if s.panics {
		panic("LEAK-PANIC")
	}
	return s.err
}

// leakyStore fails reads of the execution record with content-bearing
// text.
type leakyStore struct{ store.Store }

func (leakyStore) Entries(context.Context, string, uint64) ([]store.Entry, error) {
	return nil, errors.New("LEAK-STORE key statement-amount-12345")
}

func TestActivationErrorLogsNoContent(t *testing.T) {
	var logs bytes.Buffer
	mem := memstore.New()
	var execs execLog
	c := agent.NewController(leakyStore{mem}, registry(t, ledgerType(&execs)), agent.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	createLeakRun(t, mem)
	_, err := activateLeak(c, claim(t, mem), 0)
	if !errors.Is(err, agent.ErrStoreFailure) || !strings.Contains(err.Error(), "LEAK-STORE") {
		t.Fatalf("Activate = %v; the host gets the full error", err)
	}
	if !strings.Contains(logs.String(), `"error_class":"store_failure"`) || strings.Contains(logs.String(), "LEAK") {
		t.Fatalf("log = %s", logs.String())
	}
}

func TestStoreSink(t *testing.T) {
	s := memstore.New()
	tel, ok := s.(store.Telemetry)
	if !ok {
		t.Fatal("memstore does not implement store.Telemetry")
	}
	var execs execLog
	c := agent.NewController(s, registry(t, ledgerType(&execs)), agent.WithPolicy(leakPolicy), agent.WithTelemetry(agent.StoreSink(tel)))
	createLeakRun(t, s)
	if _, err := activateLeak(c, claim(t, s), 0); err != nil {
		t.Fatal(err)
	}
	entries, _ := s.Entries(context.Background(), "r1", 0)
	stored, err := tel.Events(context.Background(), "r1", 0)
	if err != nil || len(stored) != len(entries) {
		t.Fatalf("stored %d events for %d entries, %v", len(stored), len(entries), err)
	}
	for i, e := range stored {
		var ev agent.Event
		if err := json.Unmarshal(e.Payload, &ev); err != nil {
			t.Fatal(err)
		}
		if e.Seq != entries[i].Seq || e.Kind != entries[i].Kind || ev.Seq != e.Seq || ev.Run != "r1" {
			t.Fatalf("stored event %d = %+v (%+v)", i, e, ev)
		}
	}
	// Redelivery keeps the stored events.
	again := make([]agent.Event, 1)
	again[0] = agent.Event{Run: "r1", Seq: 1, Kind: "other"}
	if err := agent.StoreSink(tel).Emit(context.Background(), again); err != nil {
		t.Fatal(err)
	}
	if got, _ := tel.Events(context.Background(), "r1", 0); got[0].Kind != entries[0].Kind {
		t.Fatalf("redelivered event replaced the stored one: %+v", got[0])
	}
}

func TestRuntimeLogsCarryNoContent(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	runLeak(t, agent.WithLogger(logger))
	out := logs.String()
	if !strings.Contains(out, "activation started") || !strings.Contains(out, "activation ended") {
		t.Fatalf("expected activation lines: %s", out)
	}
	if strings.Contains(out, "LEAK") {
		t.Fatalf("runtime logs leak content: %s", out)
	}
}

func TestEnvLogger(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	typ := counterType()
	typ.States[0].Code = func(ctx context.Context, env *agent.Env, d counterData) (agent.Transition[counterData], error) {
		env.Logger().Info("counting", "n", d.N)
		return countStep(ctx, env, d)
	}
	s := memstore.New()
	c := agent.NewController(s, registry(t, typ), agent.WithLogger(logger))
	createRun(t, s, "r1", `{"target":1}`)
	if _, err := activate(context.Background(), c, claim(t, s), "r1", 0); err != nil {
		t.Fatal(err)
	}
	var line map[string]any
	for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(l, `"counting"`) {
			if err := json.Unmarshal([]byte(l), &line); err != nil {
				t.Fatal(err)
			}
		}
	}
	if line["run"] != "r1" || line["instance"] != inst || line["state"] != "count" {
		t.Fatalf("agent log line = %v", line)
	}
	// An Env made outside the runtime logs nothing rather than panicking.
	var zero agent.Env
	zero.Logger().Info("dropped")
}

func TestSecretNeverRenders(t *testing.T) {
	s := agent.NewSecret("hunter2")
	if s.Reveal() != "hunter2" || s.IsZero() || !agent.NewSecret("").IsZero() {
		t.Fatal("Reveal or IsZero wrong")
	}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("x", "secret", s)
	slog.New(slog.NewTextHandler(&logs, nil)).Info("x", "secret", s)
	j, _ := json.Marshal(map[string]any{"s": s, "p": &s})
	outputs := []string{
		fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s %q %x %d", s, s, s, s, s, s, s),
		fmt.Sprintf("%v", struct{ S agent.Secret }{s}), fmt.Sprintf("%+v", &s), string(j), logs.String(),
	}
	// fmt prints an unexported field by reflection, bypassing the
	// methods; slog's text handler does the same for a struct value.
	type wrapper struct {
		s     agent.Secret
		inner struct{ s agent.Secret }
	}
	w := wrapper{s: s}
	w.inner.s = s
	logs.Reset()
	slog.New(slog.NewTextHandler(&logs, nil)).Info("x", "w", w, "pw", &w)
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("x", "w", w)
	outputs = append(outputs,
		fmt.Sprintf("%v %+v %#v", w, w, w), fmt.Sprintf("%v %+v %#v", &w, &w, &w),
		fmt.Sprintf("%+v", []any{w, map[string]wrapper{"k": w}}), logs.String())
	for _, o := range outputs {
		if strings.Contains(o, "hunter2") {
			t.Fatalf("secret rendered: %s", o)
		}
	}
	var zero agent.Secret
	if zero.Reveal() != "" || !zero.IsZero() {
		t.Fatal("zero Secret not empty")
	}
}
