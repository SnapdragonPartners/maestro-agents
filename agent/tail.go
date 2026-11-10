package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// tailState is what recovery needs from the entries past the checkpoint
// (ADR 0004, item 10): a recorded turn whose calls are not all resolved.
type tailState struct {
	turn *tailTurn
}

// tailTurn is a recorded turn with its request and each call's entries.
type tailTurn struct {
	perms    map[int]PermissionEntry
	attempts map[int]AttemptEntry
	results  map[int]ResultEntry
	attempt  ModelAttemptEntry
	entry    TurnEntry
	step     uint64
}

// tailFold folds the tail's entries in order. Every step boundary commits a
// checkpoint, so the tail holds at most one step: activation starts and
// non-terminal endings, model attempts resolved by a failure or
// interrupted, and at most one turn with its calls' entries.
type tailFold struct {
	open *ModelAttemptEntry // an attempt with no turn or failure yet
	// used is the run's consumption, restored from the checkpoint; the
	// fold charges it the tail's attempts (ADR 0009).
	used *Consumption
	turn *tailTurn
	// states are the states the tail's model attempts name.
	states map[string]bool
	// interrupted counts attempts with neither a turn nor a failure after
	// them, each charged its reservation (ADR 0004, item 13).
	interrupted int
}

// interrupt charges the open attempt, which has neither a turn nor a
// failure after it, its reservation (ADR 0004, item 13).
func (f *tailFold) interrupt() {
	if f.open == nil {
		return
	}
	f.interrupted++
	f.used.charge(f.open.Model, nil, f.open.Reservation)
	f.open = nil
}

// active keeps the latest active time the tail records.
func (f *tailFold) active(d time.Duration) {
	f.used.ActiveTime = max(f.used.ActiveTime, d)
}

func (f *tailFold) add(e *store.Entry) error {
	switch e.Kind {
	case EntryActivationStarted:
		return nil
	case EntryOutcome:
		var o OutcomeEntry
		if err := decode(e, &o); err != nil {
			return err
		}
		if o.Status.Terminal() {
			return errors.New("a terminal outcome")
		}
		if o.Consumed != nil {
			f.active(o.Consumed.ActiveTime)
		}
		return nil
	case EntryModelAttempt:
		return f.addAttempt(e)
	case EntryModelFailure:
		if f.open == nil {
			return errors.New("a model failure with no attempt")
		}
		var mf ModelFailureEntry
		if err := decode(e, &mf); err != nil {
			return err
		}
		f.used.charge(f.open.Model, mf.Usage, f.open.Reservation)
		f.open = nil
		return nil
	case EntryTurn:
		return f.addTurn(e)
	case EntryPermission, EntryAttempt, EntryResult:
		if f.turn == nil {
			return errors.New("a call entry with no turn")
		}
		if err := f.turn.add(e); err != nil {
			return err
		}
		if e.Kind == EntryResult {
			// add decoded it: an executed result records the active time
			// spent through its call (ADR 0009, item 10).
			var r ResultEntry
			_ = decode(e, &r)
			f.active(r.Active)
		}
		return nil
	}
	return errors.New("a kind this version cannot recover")
}

func decode(e *store.Entry, v any) error {
	if err := json.Unmarshal(e.Payload, v); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	return nil
}

func (f *tailFold) addAttempt(e *store.Entry) error {
	if f.turn != nil {
		return errors.New("an attempt after a turn that has no checkpoint")
	}
	f.interrupt()
	var m ModelAttemptEntry
	if err := decode(e, &m); err != nil {
		return err
	}
	f.open = &m
	f.used.admitTurn(m.Model)
	f.active(m.Active)
	if f.states == nil {
		f.states = map[string]bool{}
	}
	f.states[m.State] = true
	return nil
}

func (f *tailFold) addTurn(e *store.Entry) error {
	if f.open == nil {
		return errors.New("a turn with no attempt")
	}
	var t TurnEntry
	if err := decode(e, &t); err != nil {
		return err
	}
	f.used.charge(f.open.Model, &t.Usage, f.open.Reservation)
	f.turn = &tailTurn{
		attempt: *f.open, entry: t, step: e.Step,
		perms: map[int]PermissionEntry{}, attempts: map[int]AttemptEntry{}, results: map[int]ResultEntry{},
	}
	f.open = nil
	return nil
}

// add folds one call entry: a permission, an attempt or a result.
func (t *tailTurn) add(e *store.Entry) error {
	if e.Step != t.step {
		return fmt.Errorf("step %d, but the turn is step %d", e.Step, t.step)
	}
	switch e.Kind {
	case EntryPermission:
		var p PermissionEntry
		if err := decode(e, &p); err != nil {
			return err
		}
		return t.addPermission(&p)
	case EntryAttempt:
		var a AttemptEntry
		if err := decode(e, &a); err != nil {
			return err
		}
		return t.addAttempt(&a)
	}
	var r ResultEntry
	if err := decode(e, &r); err != nil {
		return err
	}
	return t.addResult(&r)
}

// open checks that a call entry names a call of the turn without a result.
func (t *tailTurn) open(i int) error {
	if i < 0 || i >= t.entry.Calls {
		return fmt.Errorf("call %d, but the turn has %d", i, t.entry.Calls)
	}
	if _, done := t.results[i]; done {
		return fmt.Errorf("call %d already has a result", i)
	}
	return nil
}

func (t *tailTurn) addPermission(p *PermissionEntry) error {
	if err := t.open(p.Call); err != nil {
		return err
	}
	if _, dup := t.perms[p.Call]; dup {
		return fmt.Errorf("call %d has two permissions", p.Call)
	}
	if !p.Permission.valid() {
		return fmt.Errorf("call %d has permission %q", p.Call, p.Permission)
	}
	t.perms[p.Call] = *p
	return nil
}

func (t *tailTurn) addAttempt(a *AttemptEntry) error {
	if err := t.open(a.Call); err != nil {
		return err
	}
	if p, ok := t.perms[a.Call]; !ok || !p.Permission.Sendable() {
		return fmt.Errorf("call %d was attempted without a permission to send it", a.Call)
	}
	t.attempts[a.Call] = *a
	return nil
}

func (t *tailTurn) addResult(r *ResultEntry) error {
	if err := t.open(r.Call); err != nil {
		return err
	}
	switch r.Status {
	case ResultExecuted:
		if _, ok := t.attempts[r.Call]; !ok {
			return fmt.Errorf("call %d executed with no attempt", r.Call)
		}
	case ResultRefused:
	default:
		// A not-executed result commits with its transition and an unknown
		// one with the run's failure, each with a checkpoint or a terminal
		// outcome, so neither is left in a tail.
		return fmt.Errorf("call %d has a %s result outside its commit", r.Call, r.Status)
	}
	t.results[r.Call] = *r
	return nil
}

// checkTail checks the fold against the checkpoint and keeps what recovery
// needs.
func (a *activation) checkTail(f *tailFold) error {
	if len(f.states) == 0 {
		return nil
	}
	var st *erasedState
	if a.own {
		st = a.m.states[a.cs.State]
	}
	if st == nil || st.model == nil {
		return errors.New("the record holds model attempts, but the checkpointed state is not a model state")
	}
	for s := range f.states {
		if s != a.cs.State {
			return fmt.Errorf("the record holds an attempt of state %q, but the checkpoint is in state %q", s, a.cs.State)
		}
	}
	t := f.turn
	if t == nil {
		return nil
	}
	if t.step != a.step1() {
		return fmt.Errorf("the recorded turn is step %d, but the checkpoint's next step is %d", t.step, a.step1())
	}
	base := 0
	if a.cs.Model != nil {
		base = len(a.cs.Model.Transcript)
	}
	if t.attempt.Base != base {
		return fmt.Errorf("the recorded attempt continues a transcript of %d messages; the checkpoint holds %d", t.attempt.Base, base)
	}
	resolved := len(t.results)
	for i, p := range t.perms {
		if _, ok := t.results[i]; !ok && !p.Permission.Sendable() {
			resolved++
		}
	}
	if resolved >= t.entry.Calls {
		return errors.New("the recorded turn is resolved but its checkpoint is missing")
	}
	a.tail.turn = t
	return nil
}
