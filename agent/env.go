package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Env is what a state, Init and Migrate see of the run (RUN-1): its
// identity, trigger and configuration, and read access to the object store.
// A code state writes only through the operations its transition carries.
type Env struct {
	reader store.Reader
	state  string
	config json.RawMessage
	run    store.Run
	step   uint64
}

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

// Object reads an object. Reads may be stale: a write conditional on the
// returned version is what makes a transition safe (ADR 0005, item 5). A
// store failure is returned as a *ReadError.
func (e *Env) Object(ctx context.Context, namespace, key string) (store.Object, bool, error) {
	obj, ok, err := e.reader.Object(ctx, namespace, key)
	return obj, ok, readErr(err)
}

// ListObjects lists a namespace's keys that start with prefix, with their
// versions. A store failure is returned as a *ReadError.
func (e *Env) ListObjects(ctx context.Context, namespace, prefix string) ([]store.ObjectInfo, error) {
	infos, err := e.reader.ListObjects(ctx, namespace, prefix)
	return infos, readErr(err)
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
