package standalone

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// SubmittedKind is the trigger kind of a submitted run (ADR 0021, item 1):
// a submitted agent type declares it in its manifest.
const SubmittedKind = "submitted"

// Submitted runs' identities (ADR 0021, item 2).
const (
	submittedTriggerPrefix = "submitted:"
	maxInstanceID          = 128
)

// ErrBusy is Submit's error for an instance that has a run still
// unfinished, here or in another host's live claim: retry later (ADR 0021,
// item 3).
var ErrBusy = errors.New("standalone: the instance has an unfinished run")

// ErrInvalid is Submit's error for a submission it refuses before making a
// run: an agent type the program does not submit, or a malformed instance.
var ErrInvalid = errors.New("standalone: invalid submission")

// Submission is a trigger submitted in process (ADR 0021, item 2): a run
// of a submitted agent type on an instance, with a payload the type reads
// from agent.Env.Trigger.
type Submission struct {
	// AgentType is one of Program.Submitted.
	AgentType string
	// Instance is the run's instance: letters, digits, '.', '_' and '-',
	// at most 128 bytes.
	Instance string
	// Payload is the trigger's payload, JSON.
	Payload json.RawMessage
}

// Submit makes a run of the submission and activates it to its end, and
// returns its report (ADR 0021, items 2 and 3). It first finishes any
// runnable run the instance has, of a type the host serves, so a run left
// by a crash or a shutdown does not hold the instance. It returns ErrBusy
// when the instance still has an unfinished run, ErrInvalid for a
// submission it refuses, and otherwise an error only when the run cannot
// be made; a run that does not succeed is a report. Cancelling ctx shuts
// the activation down at its next step boundary, leaving the run runnable.
// Submit is safe for concurrent use; runs on one instance are serialized.
func (h *Host) Submit(ctx context.Context, s Submission) (Report, error) {
	return h.h.submit(ctx, s)
}

func (h *host) submit(ctx context.Context, s Submission) (Report, error) {
	if !h.submitted[s.AgentType] {
		return Report{}, fmt.Errorf("%w: agent type %q is not one the program submits", ErrInvalid, s.AgentType)
	}
	if err := checkInstanceID(s.Instance); err != nil {
		return Report{}, err
	}
	if !json.Valid(s.Payload) {
		return Report{}, fmt.Errorf("%w: the payload is not JSON", ErrInvalid)
	}
	if err := h.locks.lock(ctx, s.Instance); err != nil {
		return Report{}, fmt.Errorf("standalone: waiting for instance %s: %w", s.Instance, err)
	}
	defer h.locks.unlock(s.Instance)
	dctx := context.WithoutCancel(ctx)
	if err := h.drain(ctx, s.Instance); err != nil {
		return Report{}, err
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	run, err := h.makeRun(dctx, store.Trigger{
		Instance: s.Instance, ID: submittedTriggerPrefix + id, Kind: SubmittedKind, Payload: s.Payload,
	}, s.Instance+"."+id, s.AgentType)
	switch {
	case errors.Is(err, store.ErrInstanceBusy):
		return Report{}, fmt.Errorf("%w: %s", ErrBusy, s.Instance)
	case err != nil:
		return Report{}, err
	}
	rep := Report{Run: run.ID, Status: run.Status, Reason: run.Reason}
	if ctx.Err() != nil {
		return rep, nil // shut down before the run started: it stays runnable
	}
	h.fill(&rep, h.activate(ctx, &run))
	return rep, nil
}

// drain activates the instance's runnable run, if it has one of a type the
// host serves, under the caller's instance lock. A run still unfinished
// afterwards, or one this host cannot activate, is ErrBusy.
func (h *host) drain(ctx context.Context, instance string) error {
	run, ok, err := h.st.ActiveRun(ctx, instance)
	switch {
	case err != nil:
		return fmt.Errorf("standalone: read instance %s: %w", instance, err)
	case !ok:
		return nil
	case run.Status != store.StatusRunnable || !h.serves(run.AgentType) || ctx.Err() != nil:
		return fmt.Errorf("%w: %s", ErrBusy, instance)
	}
	if r := h.activate(ctx, &run); r.interrupted() {
		return fmt.Errorf("%w: %s", ErrBusy, instance)
	}
	return nil
}

// fill copies an activation's result into a report.
func (h *host) fill(rep *Report, r *result) {
	rep.Activated = true
	rep.Status, rep.Reason = r.Status, r.Reason
	if r.Err != nil {
		rep.Error = r.Err.Error()
		return
	}
	rep.Detail, rep.Steps = r.Outcome.Detail, r.Outcome.Steps
	c := r.Outcome.Consumed
	rep.Consumed = &c
}

// serves reports whether the host runs agent type t: the configured type
// or a submitted one (ADR 0021, item 4).
func (h *host) serves(t string) bool { return t == h.cfg.AgentType || h.submitted[t] }

func checkInstanceID(s string) error {
	if s == "" || len(s) > maxInstanceID {
		return fmt.Errorf("%w: instance %q: 1 to %d bytes", ErrInvalid, s, maxInstanceID)
	}
	for _, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
		if !ok {
			return fmt.Errorf("%w: instance %q: letters, digits, '.', '_' and '-' only", ErrInvalid, s)
		}
	}
	return nil
}

// instanceLocks serializes activations per instance within the process
// (ADR 0002; ADR 0021, item 3): the scheduler takes an instance's lock
// without waiting, Submit and RunInput wait for it. The zero value is
// ready to use.
type instanceLocks struct {
	mu   sync.Mutex
	held map[string]chan struct{}
}

// take marks the instance held; l.mu is held by the caller.
func (l *instanceLocks) take(instance string) {
	if l.held == nil {
		l.held = map[string]chan struct{}{}
	}
	l.held[instance] = make(chan struct{})
}

// tryLock takes the instance's lock if it is free.
func (l *instanceLocks) tryLock(instance string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.held[instance]; ok {
		return false
	}
	l.take(instance)
	return true
}

// lock waits for the instance's lock, or for ctx to end.
func (l *instanceLocks) lock(ctx context.Context, instance string) error {
	for {
		l.mu.Lock()
		ch, ok := l.held[instance]
		if !ok {
			l.take(instance)
			l.mu.Unlock()
			return nil
		}
		l.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return context.Cause(ctx) //nolint:wrapcheck // the caller names the wait
		}
	}
}

// unlock releases the instance's lock, waking its waiters.
func (l *instanceLocks) unlock(instance string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ch, ok := l.held[instance]; ok {
		close(ch)
		delete(l.held, instance)
	}
}
