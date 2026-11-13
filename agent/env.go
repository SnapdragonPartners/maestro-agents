package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"text/template"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Env is what a state, Init and Migrate see of the run (RUN-1): its
// identity, trigger and configuration, and read access to the run's
// namespace of the object store (STORE-2, ADR 0012). A code state writes
// only through the operations its transition carries, and an atomic local
// transaction only through its result's; PutJSON, PutBytes and Delete build
// them in the run's namespace. Agent code never names a namespace: the host
// assigns one per run, and the runtime refuses an operation outside it.
type Env struct {
	reader    store.Reader
	namespace string
	state     string
	config    json.RawMessage
	run       store.Run
	templates *template.Template
	logger    *slog.Logger
	creds     CredentialProvider
	rc        RunContext
	budget    BudgetStatus
	step      uint64
}

// Budget returns the run's budgets and consumption as of when the Env was
// made, with the running model state's wrap-up point (ADR 0010). Its
// String is the visible clock each of the state's tool results ends with,
// so a Prompt can state the budget before the first result.
func (e *Env) Budget() BudgetStatus {
	s := e.budget
	s.Consumed = s.Consumed.clone()
	if s.WrapUp != nil {
		w := *s.WrapUp
		s.WrapUp = &w
	}
	return s
}

// Logger returns the host's logger (HOST-2, ADR 0016 item 11), with the
// run, instance and state as attributes. Agent code logs through it, so one
// host policy governs every line; what its own lines carry is the agent
// type's responsibility.
func (e *Env) Logger() *slog.Logger {
	if e.logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return e.logger
}

// RunContext returns the run's context (TOOL-8, ADR 0016 item 12).
func (e *Env) RunContext() RunContext { return e.rc }

// RunID returns the run's ID.
func (e *Env) RunID() string { return e.run.ID }

// Instance returns the agent instance the run belongs to.
func (e *Env) Instance() string { return e.run.Instance }

// Scope returns the run's scope (ADR 0003, item 1).
func (e *Env) Scope() store.Scope { return store.Scope{Kind: store.ScopeRun, ID: e.run.ID} }

// TriggerID returns the identity of the trigger that started the run.
func (e *Env) TriggerID() string { return e.run.TriggerID }

// Trigger returns the trigger's payload.
func (e *Env) Trigger() json.RawMessage { return clone(e.run.Trigger) }

// Config returns the run's resolved configuration, fixed when the run
// started (RUN-1).
func (e *Env) Config() json.RawMessage { return clone(e.config) }

// State returns the current state's name.
func (e *Env) State() string { return e.state }

// Step returns the current step's number, counted from 1 (ADR 0003,
// item 3).
func (e *Env) Step() uint64 { return e.step }

// Object reads an object in the run's namespace. Reads may be stale: a
// write conditional on the returned version is what makes a transition safe
// (ADR 0005, item 5). A store failure is returned as a *ReadError.
func (e *Env) Object(ctx context.Context, key string) (store.Object, bool, error) {
	obj, ok, err := e.reader.Object(ctx, e.namespace, key)
	return obj, ok, readErr(err)
}

// ListObjects lists the keys in the run's namespace that start with prefix,
// with their versions. A store failure is returned as a *ReadError.
func (e *Env) ListObjects(ctx context.Context, prefix string) ([]store.ObjectInfo, error) {
	infos, err := e.reader.ListObjects(ctx, e.namespace, prefix)
	return infos, readErr(err)
}

// PutJSON returns an operation that puts a JSON value at key in the run's
// namespace, conditional on the object's version (zero: the key must not
// exist; ADR 0005 item 3).
func (e *Env) PutJSON(key string, expectVersion uint64, value json.RawMessage) store.Op {
	return store.PutObjectJSON(e.namespace, key, expectVersion, value)
}

// PutBytes returns an operation that puts bytes at key in the run's
// namespace, conditional on the object's version.
func (e *Env) PutBytes(key string, expectVersion uint64, value []byte) store.Op {
	return store.PutObjectBytes(e.namespace, key, expectVersion, value)
}

// Delete returns an operation that deletes key in the run's namespace,
// conditional on the object's version.
func (e *Env) Delete(key string, expectVersion uint64) store.Op {
	return store.DeleteObjectOp(e.namespace, key, expectVersion)
}

// readErr wraps a store failure as a *ReadError. An invalid request (a
// malformed key, say) is the caller's defect, not the store's, and is
// returned as is.
func readErr(err error) error {
	if err == nil || errors.Is(err, store.ErrInvalid) {
		return err
	}
	return &ReadError{Err: err}
}

func clone(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}
