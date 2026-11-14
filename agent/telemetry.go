package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// TelemetryTimeout bounds each call to a Sink (ADR 0016, item 4).
const TelemetryTimeout = 5 * time.Second

// Event is a telemetry event (HOST-1, ADR 0016): the projection of one
// execution-record entry, emitted after the commit that appended it. Its
// identity is (Run, Seq), Seq being the entry's sequence, so it is unbroken
// across activations and a redelivered event has the same identity.
//
// An event carries metadata only: identities, names, counts and codes. It
// never carries state data, object values, the trigger's payload, the
// configuration, the principal, tool arguments or results, or model
// content. Free text that may quote content is in Detail, which leaves the
// run only if the host's Redactor keeps it.
type Event struct {
	At time.Time `json:"at"`
	// Model is the route of a model_attempt or turn (LLM-3).
	Model *ModelRef `json:"model,omitempty"`
	// Usage is a turn's usage, or a model_failure's when it reported one.
	Usage *Usage `json:"usage,omitempty"`
	// Consumed is an outcome's consumption (BUD-1).
	Consumed  *Consumption `json:"consumed,omitempty"`
	Run       string       `json:"run"`
	Instance  string       `json:"instance"`
	AgentType string       `json:"agent_type"`
	// Kind is the kind of the entry the event projects (EntryTurn, say).
	Kind string `json:"kind"`

	// State is the model state of a model_attempt.
	State string `json:"state,omitempty"`
	// From and To are a transition's states.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// ErrorKind classifies a model_failure.
	ErrorKind string `json:"error_kind,omitempty"`

	// Tool, Effect, Permission and Result describe a permission, attempt
	// or result entry's call.
	Tool       string       `json:"tool,omitempty"`
	Effect     Effect       `json:"effect,omitempty"`
	Permission Permission   `json:"permission,omitempty"`
	Result     ResultStatus `json:"result,omitempty"`

	// Status and Reason are an outcome's (RUN-3).
	Status store.Status `json:"status,omitempty"`
	Reason store.Reason `json:"reason,omitzero"`

	// Owner is an activation_started's claimant.
	Owner string `json:"owner,omitempty"`

	// TriggerID, TriggerKind and Namespace are a run_started's.
	TriggerID   string `json:"trigger_id,omitempty"`
	TriggerKind string `json:"trigger_kind,omitempty"`
	Namespace   string `json:"namespace,omitempty"`

	// Detail is free text that may quote content: an outcome's detail, a
	// model failure's message or a permission's reason. It is cleared
	// before the event leaves unless the host's Redactor keeps it (ADR
	// 0016, item 3).
	Detail string `json:"detail,omitempty"`

	Seq  uint64 `json:"seq"`
	Step uint64 `json:"step"`
	// Epoch is an activation_started's claim epoch.
	Epoch uint64 `json:"epoch,omitempty"`
	// Attempt numbers a model_attempt's provider attempts (ADR 0014).
	Attempt int `json:"attempt,omitempty"`
	// Calls is how many tool calls a turn made.
	Calls int `json:"calls,omitempty"`
	// HTTPStatus is a model_failure's provider status.
	HTTPStatus int `json:"http_status,omitempty"`
	// Call is a permission, attempt or result entry's call index.
	Call int `json:"call,omitempty"`
	// WrapUp marks a wrap-up turn's model_attempt (ADR 0010).
	WrapUp bool `json:"wrap_up,omitempty"`
	// IsError marks a result the model should correct.
	IsError bool `json:"is_error,omitempty"`
}

// Sink receives telemetry (HOST-1, ADR 0016 item 4). Emit is called once
// per commit with that commit's events in sequence order, synchronously,
// with a context that ends after TelemetryTimeout and is not cancelled with
// the activation. An error or a panic is logged as a warning and never
// fails the run (MODE-7); a sink that needs longer buffers on its own.
//
// Emit is called concurrently: for different runs, since one controller
// serves many instances, and for one run across an ownership handoff. An
// implementation must be safe for concurrent use. Within one activation a
// run's batches arrive in order; across activations they may not (an
// activation's final commit releases ownership before its events are
// emitted), so a sink that needs a run's events in order sorts them by
// Seq.
type Sink interface {
	Emit(ctx context.Context, events []Event) error
}

// Redactor edits each event before it leaves the run (HOST-2, ADR 0016
// item 3). It receives the full event, Detail included; what it leaves is
// emitted. Without one, Detail is cleared. It is called concurrently, from
// every activation the controller runs, and must be safe for concurrent
// use.
type Redactor func(*Event)

// WithTelemetry sets the telemetry sink. Nil, the default, emits nothing.
func WithTelemetry(s Sink) Option {
	return func(c *Controller) { c.sink = s }
}

// WithRedactor sets the redactor every event passes through before the
// sink. Nil, the default, clears each event's Detail.
func WithRedactor(r Redactor) Option {
	return func(c *Controller) { c.redact = r }
}

// emit sends the events of a landed commit's entries to the sink (ADR
// 0016, items 1 and 5). It never fails the activation.
func (a *activation) emit(ctx context.Context, ops []store.Op) {
	if a.c.sink == nil {
		return
	}
	var evs []Event
	at := time.Now().UTC()
	for _, op := range ops {
		if op.Kind == store.OpAppendEntry && op.AppendEntry != nil {
			evs = append(evs, a.event(op.AppendEntry, at))
		}
	}
	if len(evs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(durable(ctx), a.c.telemetryTimeout)
	defer cancel()
	if failure := a.deliver(ctx, evs); failure != "" {
		// The failure is logged by class only: a sink's error or a
		// panic's value may quote what the redactor was meant to remove
		// (ADR 0016, item 10).
		a.c.logger.Warn("agent: telemetry not delivered",
			"run", a.run.ID, "instance", a.run.Instance, "first_seq", evs[0].Seq, "last_seq", evs[len(evs)-1].Seq, "failure", failure)
	}
}

// Telemetry failure classes, the only thing logged of a failure (ADR 0016,
// item 5).
const (
	telemetryRedactorPanic = "redactor_panic"
	telemetrySinkPanic     = "sink_panic"
	telemetrySinkTimeout   = "sink_timeout"
	telemetrySinkError     = "sink_error"
)

// deliver redacts the events and emits them, returning the failure's class,
// or empty on success.
func (a *activation) deliver(ctx context.Context, evs []Event) (failure string) {
	stage := telemetryRedactorPanic
	defer func() {
		if recover() != nil {
			failure = stage
		}
	}()
	for i := range evs {
		if a.c.redact == nil {
			evs[i].Detail = ""
			continue
		}
		a.c.redact(&evs[i])
	}
	stage = telemetrySinkPanic
	err := a.c.sink.Emit(ctx, evs)
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return telemetrySinkTimeout
	default:
		return telemetrySinkError
	}
}

// event projects an entry (ADR 0016, item 2). An entry whose payload does
// not decode, or a kind with no projection, keeps the common fields.
func (a *activation) event(e *store.Entry, at time.Time) Event {
	ev := Event{At: at, Run: e.Run, Instance: a.run.Instance, AgentType: a.run.AgentType, Kind: e.Kind, Seq: e.Seq, Step: e.Step}
	if p, ok := projections[e.Kind]; ok {
		p(&ev, e.Payload)
	}
	return ev
}

// projection copies the fields of an entry's payload an event may carry.
type projection func(ev *Event, payload json.RawMessage)

func project[P any](f func(ev *Event, p *P)) projection {
	return func(ev *Event, payload json.RawMessage) {
		var p P
		if json.Unmarshal(payload, &p) == nil {
			f(ev, &p)
		}
	}
}

// projections names, per entry kind, what its event carries. A field not
// copied here never leaves the run.
var projections = map[string]projection{ //nolint:gochecknoglobals // a fixed table, never written
	EntryActivationStarted: project(func(ev *Event, p *ActivationStarted) {
		ev.Owner, ev.Epoch = p.Owner, p.Epoch
	}),
	EntryRunStarted: project(func(ev *Event, p *RunStarted) {
		ev.TriggerID, ev.TriggerKind, ev.Namespace = p.TriggerID, p.TriggerKind, p.Namespace
	}),
	EntryModelAttempt: project(func(ev *Event, p *ModelAttemptEntry) {
		ev.Model, ev.State, ev.Attempt, ev.WrapUp = &p.Model, p.State, p.Attempt, p.WrapUp
	}),
	EntryTurn: project(func(ev *Event, p *TurnEntry) {
		ev.Model, ev.Usage, ev.Calls = &p.Model, &p.Usage, p.Calls
	}),
	EntryModelFailure: project(func(ev *Event, p *ModelFailureEntry) {
		ev.Usage, ev.ErrorKind, ev.HTTPStatus, ev.Detail = p.Usage, p.Kind, p.Status, p.Message
	}),
	EntryPermission: project(func(ev *Event, p *PermissionEntry) {
		ev.Tool, ev.Call, ev.Effect, ev.Permission, ev.Detail = p.Tool, p.Call, p.Effect, p.Permission, p.Reason
	}),
	EntryAttempt: project(func(ev *Event, p *AttemptEntry) {
		ev.Tool, ev.Call, ev.Effect = p.Tool, p.Call, p.Effect
	}),
	EntryResult: project(func(ev *Event, p *ResultEntry) {
		ev.Tool, ev.Call, ev.Result, ev.IsError = p.Tool, p.Call, p.Status, p.IsError
	}),
	EntryTransition: project(func(ev *Event, p *TransitionEntry) {
		ev.From, ev.To = p.From, p.To
	}),
	EntryOutcome: project(func(ev *Event, p *OutcomeEntry) {
		ev.Status, ev.Reason, ev.Consumed, ev.Detail = p.Status, p.Reason, p.Consumed, p.Detail
	}),
}

// StoreSink returns a Sink that writes events to a store's telemetry
// (ADR 0016, item 9), each encoded as its record's payload.
func StoreSink(t store.Telemetry) Sink { return storeSink{t} }

type storeSink struct{ t store.Telemetry }

func (s storeSink) Emit(ctx context.Context, events []Event) error {
	recs := make([]store.Event, len(events))
	for i := range events {
		raw, err := json.Marshal(&events[i])
		if err != nil {
			return fmt.Errorf("agent: encode event %d: %w", events[i].Seq, err)
		}
		recs[i] = store.Event{Run: events[i].Run, Seq: events[i].Seq, Kind: events[i].Kind, At: events[i].At, Payload: raw}
	}
	if err := s.t.AppendEvents(ctx, recs); err != nil {
		return fmt.Errorf("agent: store telemetry: %w", err)
	}
	return nil
}
