package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Store is what the controller needs of a store: the fenced commit and the
// reads.
type Store interface {
	store.Committer
	store.Reader
}

// DefaultConflictRetries is how many times a code state is run again when
// its object operations conflict with another instance's commit (ADR 0005,
// item 10).
const DefaultConflictRetries = 3

// Controller runs activations (ADR 0003, item 12). It holds no per-run
// state, so one controller serves every instance a host runs, concurrently.
type Controller struct {
	store    Store
	registry *Registry
	retries  int
}

// Option configures a Controller.
type Option func(*Controller)

// WithConflictRetries sets how many times a code state is re-run on an
// object conflict before the run fails with ReasonObjectConflict. Negative
// values count as zero.
func WithConflictRetries(n int) Option {
	return func(c *Controller) { c.retries = max(n, 0) }
}

// NewController returns a controller that runs the registry's agent types
// against s.
func NewController(s Store, r *Registry, opts ...Option) *Controller {
	c := &Controller{store: s, registry: r, retries: DefaultConflictRetries}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ActivationInput is what the host passes to an activation (RUN-1). The
// controller reads the run record, the latest checkpoint and the execution
// record's tail through the store, so the input never duplicates durable
// state.
type ActivationInput struct {
	// Fence is the host's claim on the run's instance (ADR 0003, item 8).
	Fence store.Fence
	// Scope is the run's scope: kind run, with the run's ID.
	Scope store.Scope
	// Config is the resolved configuration, recorded when the run starts
	// and used by every later activation in its place (RUN-1). A later
	// activation ignores this field.
	Config json.RawMessage
	// MaxSteps is how many steps this activation may complete before it
	// stops with the run runnable (STM-7). Zero is no limit.
	MaxSteps int
}

// Activate runs the run named by in.Scope from its last step boundary
// until it ends, its context ends or it reaches in.MaxSteps.
//
// A nil error means the returned Outcome is what this activation durably
// committed, in the commit that released the instance (ADR 0003, item 13).
// A non-nil error (ErrOwnershipLost, ErrStoreFailure, ErrNotActivatable)
// means there is no confirmed outcome and the Outcome is zero; the host
// rereads the run record before acting (item 14). Cancellation, budget
// expiry and shutdown are outcomes, told apart by the context's cause
// (item 17).
//
// The controller's own reads and writes use a context that is not
// cancelled with ctx, so a cancelled activation still commits its outcome.
func (c *Controller) Activate(ctx context.Context, in ActivationInput) (Outcome, error) { //nolint:gocritic // hugeParam: the input is passed by value, as ADR 0003 item 12 shows
	a, err := c.begin(ctx, &in)
	if err != nil {
		return Outcome{}, err
	}
	e := a.start(ctx, in.Config)
	for e == nil {
		switch {
		case ctx.Err() != nil:
			e = a.endByCause(ctx)
		case in.MaxSteps > 0 && a.steps >= in.MaxSteps:
			e = a.end(ctx, OutcomeEntry{Status: store.StatusRunnable, Reason: store.Reason{Code: ReasonStepLimit}})
		default:
			e = a.step(ctx)
			a.steps++
		}
	}
	return e.out, e.err
}

// activation is one activation's view of its run. Only the activation
// writes the run's records, so the view stays exact (ADR 0005, item 5).
type activation struct {
	// start1 is the run's start while it is not yet committed.
	start1 *runStart
	m      *machine
	c      *Controller
	scope  store.Scope
	fence  store.Fence
	// config is the run's resolved configuration; cs.Config its digest.
	config json.RawMessage
	run    store.Run
	cs     checkpointState
	// ckSeq is the run's latest checkpoint, nextCk the instance's next
	// checkpoint sequence.
	ckSeq  uint64
	nextCk uint64
	// nextEntry is the run's next execution-record sequence.
	nextEntry uint64
	steps     int
	// own reports whether the run has a checkpoint.
	own bool
}

// ending is an activation's result: an outcome or an error.
type ending struct {
	err error
	out Outcome
}

// durable returns the context for the controller's own store calls: not
// cancelled with the activation (ADR 0003, item 17).
func durable(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotActivatable, fmt.Sprintf(format, args...))
}

func storeFailure(err error) error { return fmt.Errorf("%w: %w", ErrStoreFailure, err) }

// commitErr classifies a commit that did not land, or may not have.
func commitErr(err error) *ending {
	if errors.Is(err, store.ErrOwnership) {
		return &ending{err: fmt.Errorf("%w: %w", ErrOwnershipLost, err)}
	}
	return &ending{err: storeFailure(err)}
}

// begin reads the run, its latest checkpoint and the record's tail, and
// refuses an activation it cannot run before writing anything.
func (c *Controller) begin(ctx context.Context, in *ActivationInput) (*activation, error) {
	if in.Scope.Kind != store.ScopeRun || in.Scope.ID == "" {
		return nil, refuse("scope %+v is not a run", in.Scope)
	}
	if in.Fence.Instance == "" || in.Fence.Owner == "" {
		return nil, refuse("fence %+v names no instance or owner", in.Fence)
	}
	run, err := c.activatableRun(ctx, in)
	if err != nil {
		return nil, err
	}
	m, ok := c.registry.lookup(run.AgentType)
	if !ok {
		return nil, refuse("run %q names agent type %q, which is not registered", run.ID, run.AgentType)
	}
	a := &activation{c: c, m: m, fence: in.Fence, run: run, scope: in.Scope, nextCk: 1}
	latest, ok, err := c.store.LatestCheckpoint(durable(ctx), run.Instance)
	if err != nil {
		return nil, storeFailure(err)
	}
	var after uint64
	if ok {
		a.nextCk = latest.Seq + 1
		// Checkpoint sequences are per instance: an earlier run's
		// checkpoint is never restored (ADR 0004, item 3).
		if latest.Scope == a.scope {
			if err := a.restore(ctx, &latest); err != nil {
				return nil, err
			}
			after = latest.RecordSeq
		}
	}
	if err := a.readTail(ctx, after); err != nil {
		return nil, err
	}
	return a, nil
}

// activatableRun reads the run record and refuses a run this activation
// cannot run.
func (c *Controller) activatableRun(ctx context.Context, in *ActivationInput) (store.Run, error) {
	run, err := c.store.Run(durable(ctx), in.Scope.ID)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrInvalid):
		return run, refuse("run %q: %v", in.Scope.ID, err)
	case err != nil:
		return run, storeFailure(err)
	case run.Instance != in.Fence.Instance:
		return run, refuse("run %q belongs to instance %q, not %q", run.ID, run.Instance, in.Fence.Instance)
	case run.Status == store.StatusWaiting:
		return run, refuse("run %q is waiting; delivering input is not built yet (STM-8)", run.ID)
	case run.Status != store.StatusRunnable:
		return run, refuse("run %q is %s", run.ID, run.Status)
	}
	return run, nil
}

// readTail reads the entries after the checkpoint. Every step boundary
// commits a checkpoint, so the tail holds only what an activation commits
// without one: its start, and a non-terminal ending.
func (a *activation) readTail(ctx context.Context, after uint64) error {
	tail, err := a.c.store.Entries(durable(ctx), a.run.ID, after)
	if err != nil {
		return storeFailure(err)
	}
	next := after + 1
	for _, e := range tail {
		if e.Seq != next {
			return storeFailure(fmt.Errorf("agent: run %q record has entry %d where %d belongs", a.run.ID, e.Seq, next))
		}
		if e.Kind != EntryActivationStarted && e.Kind != EntryOutcome {
			return refuse("run %q record has a %q entry past its checkpoint, which this version cannot recover", a.run.ID, e.Kind)
		}
		next++
	}
	a.nextEntry = next
	return nil
}

// restore loads the run's checkpoint.
func (a *activation) restore(ctx context.Context, c *store.Checkpoint) error {
	if c.Format != checkpointFormat {
		return refuse("run %q checkpoint %d has format %d, want %d", a.run.ID, c.Seq, c.Format, checkpointFormat)
	}
	if err := json.Unmarshal(c.State, &a.cs); err != nil {
		return storeFailure(fmt.Errorf("agent: decode checkpoint %d: %w", c.Seq, err))
	}
	if a.cs.AgentType != a.m.name {
		return refuse("run %q checkpoint %d is of agent type %q, not %q", a.run.ID, c.Seq, a.cs.AgentType, a.m.name)
	}
	if a.cs.Config != "" {
		_, cfg, err := a.c.store.Artifact(durable(ctx), a.cs.Config)
		if err != nil {
			return storeFailure(fmt.Errorf("agent: read configuration %s: %w", a.cs.Config, err))
		}
		a.config = cfg
	}
	a.own, a.ckSeq = true, c.Seq
	return nil
}

func (a *activation) env() *Env {
	state := a.cs.State
	if !a.own {
		state = a.m.initial
	}
	return &Env{reader: a.c.store, config: a.config, run: a.run, state: state, step: a.step1()}
}

// step1 is the current step: the checkpoint's next step, or 1 before the
// run has a checkpoint.
func (a *activation) step1() uint64 { return max(a.cs.Step, 1) }

// commit makes one fenced commit. The entries in it were numbered from
// a.nextEntry up to next; they are the run's only once the commit lands.
func (a *activation) commit(ctx context.Context, ops []store.Op, next uint64) error {
	if err := a.c.store.Commit(durable(ctx), a.fence, ops); err != nil {
		return err //nolint:wrapcheck // the caller classifies the store's error (ADR 0005, item 3)
	}
	a.nextEntry = next
	return nil
}

// entries numbers execution-record entries from a.nextEntry.
type entries struct {
	err  error
	a    *activation
	ops  []store.Op
	next uint64
}

func (a *activation) entries() *entries { return &entries{a: a, next: a.nextEntry} }

func (b *entries) add(step uint64, kind string, payload any) {
	if b.err != nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		b.err = storeFailure(fmt.Errorf("agent: encode %s entry: %w", kind, err))
		return
	}
	b.ops = append(b.ops, store.AppendEntryOp(store.Entry{Run: b.a.run.ID, Seq: b.next, Step: step, Kind: kind, Payload: raw}))
	b.next++
}

// checkpoint adds the checkpoint covering every entry added so far.
func (b *entries) checkpoint(cs *checkpointState) {
	if b.err != nil {
		return
	}
	raw, err := json.Marshal(cs)
	if err != nil {
		b.err = storeFailure(fmt.Errorf("agent: encode checkpoint: %w", err))
		return
	}
	b.ops = append(b.ops, store.PutCheckpointOp(store.Checkpoint{
		Instance: b.a.run.Instance, Scope: b.a.scope, Seq: b.a.nextCk, RecordSeq: b.next - 1,
		Format: checkpointFormat, State: raw,
	}))
}

// start commits activation_started, then either starts the run (Init, the
// run_started entry and the first checkpoint) or migrates a checkpoint
// from another version of the agent type.
func (a *activation) start(ctx context.Context, config json.RawMessage) *ending {
	b := a.entries()
	b.add(a.step1(), EntryActivationStarted, ActivationStarted{
		Owner: a.fence.Owner, Epoch: a.fence.Epoch, CheckpointSeq: a.ckSeq, RecordSeq: a.nextEntry - 1,
	})
	if b.err != nil {
		return &ending{err: b.err}
	}
	if err := a.commit(ctx, b.ops, b.next); err != nil {
		return commitErr(err)
	}
	if !a.own {
		return a.startRun(ctx, config)
	}
	if a.cs.AgentVersion == a.m.version {
		return nil
	}
	if a.m.migrate == nil {
		return a.fail(ctx, ReasonStateError, fmt.Sprintf(
			"checkpoint was written by version %q of agent type %q, which is version %q and has no Migrate",
			a.cs.AgentVersion, a.m.name, a.m.version))
	}
	if ctx.Err() != nil {
		return a.endByCause(ctx)
	}
	var data json.RawMessage
	err := guard(func() (err error) {
		data, err = a.m.migrate(ctx, a.cs.AgentVersion, a.cs.Data)
		return err
	})
	if err != nil {
		return a.codeFailed(ctx, fmt.Errorf("migrate from version %q: %w", a.cs.AgentVersion, err))
	}
	// The migrated data is checkpointed by the next step boundary.
	a.cs.Data, a.cs.AgentVersion = data, a.m.version
	return nil
}

// startRun runs Init and commits the run's start with its first checkpoint,
// so Init never runs again once the run's start has landed (it may run
// again if this commit does not land). Until that commit lands, a terminal
// ending carries the run's start with it (REC-1).
func (a *activation) startRun(ctx context.Context, config json.RawMessage) *ending {
	start := &runStart{entry: RunStarted{
		AgentType: a.m.name, AgentVersion: a.m.version, TriggerID: a.run.TriggerID, Trigger: a.run.Trigger,
	}}
	a.start1 = start
	if err := store.ValidJSON(config); err != nil {
		return a.fail(ctx, ReasonStateError, "configuration: "+err.Error())
	}
	if config != nil {
		sum := sha256.Sum256(config)
		start.entry.Config = hex.EncodeToString(sum[:])
		start.config = &store.Artifact{Digest: start.entry.Config, MediaType: "application/json", Content: config}
	}
	a.config = config
	if ctx.Err() != nil {
		// The start is prepared, so a terminal ending records it.
		return a.endByCause(ctx)
	}
	var data json.RawMessage
	err := guard(func() (err error) {
		data, err = a.m.init(ctx, a.env())
		return err
	})
	if err != nil {
		return a.codeFailed(ctx, fmt.Errorf("init: %w", err))
	}
	cs := checkpointState{
		AgentType: a.m.name, AgentVersion: a.m.version, State: a.m.initial, Data: data, Step: 1, Config: start.entry.Config,
	}
	b := a.entries()
	start.add(b)
	b.checkpoint(&cs)
	if b.err != nil {
		return &ending{err: b.err}
	}
	if err := a.commit(ctx, b.ops, b.next); err != nil {
		return commitErr(err)
	}
	a.start1 = nil
	a.checkpointed(&cs)
	return nil
}

// runStart is a run's start, not yet committed: the configuration
// artifact, if any, and the run_started entry.
type runStart struct {
	config *store.Artifact
	entry  RunStarted
}

func (r *runStart) add(b *entries) {
	if r.config != nil {
		b.ops = append(b.ops, store.PutArtifactOp(*r.config))
	}
	b.add(1, EntryRunStarted, r.entry)
}

// checkpointed records that a commit published cs at a.nextCk.
func (a *activation) checkpointed(cs *checkpointState) {
	a.cs, a.own, a.ckSeq = *cs, true, a.nextCk
	a.nextCk++
}

// step runs the current code state and commits its transition, re-running
// it on an object conflict up to the controller's bound.
func (a *activation) step(ctx context.Context) *ending {
	st := a.m.states[a.cs.State]
	switch {
	case st == nil:
		return a.fail(ctx, ReasonStateError,
			fmt.Sprintf("checkpointed state %q is not declared by agent type %q", a.cs.State, a.m.name))
	case st.end != "":
		return a.end(ctx, st.end.outcome())
	}
	for attempt := 0; ; attempt++ {
		var tr erasedTransition
		err := guard(func() (err error) {
			tr, err = st.code(ctx, a.env(), a.cs.Data)
			return err
		})
		if err != nil {
			return a.codeFailed(ctx, fmt.Errorf("state %q: %w", st.name, err))
		}
		// A state that returns normally after the context ended has its
		// result discarded: it has no durable effect of its own (ADR 0003,
		// item 5), and the cause decides the ending (RUN-2).
		if ctx.Err() != nil {
			return a.endByCause(ctx)
		}
		next, err := a.checkTransition(st, &tr)
		if err != nil {
			return a.fail(ctx, ReasonStateError, err.Error())
		}
		e, retry := a.commitTransition(ctx, st, next, &tr, attempt)
		if !retry {
			return e
		}
		if ctx.Err() != nil {
			return a.endByCause(ctx)
		}
	}
}

// checkTransition refuses an undeclared transition (STM-1) and operations a
// code state may not commit.
func (a *activation) checkTransition(st *erasedState, tr *erasedTransition) (*erasedState, error) {
	if !st.next[tr.to] {
		return nil, fmt.Errorf("state %q chose transition to %q, which it does not declare", st.name, tr.to)
	}
	for i, op := range tr.ops {
		if op.Kind != store.OpPutObject && op.Kind != store.OpDeleteObject {
			return nil, fmt.Errorf("state %q returned operation %d of kind %q; a transition carries object operations only", st.name, i, op.Kind)
		}
		if err := op.Validate(); err != nil {
			return nil, fmt.Errorf("state %q operation %d: %w", st.name, i, err)
		}
	}
	return a.m.states[tr.to], nil
}

// commitTransition commits a transition: the state's object operations,
// the transition entry and the new state's checkpoint, and, entering a
// terminal state, the run's ending, all in one commit (ADR 0004, item 4).
// It returns nil after a non-terminal transition, and retry true when the
// state should run again after an object conflict.
func (a *activation) commitTransition(ctx context.Context, st, next *erasedState, tr *erasedTransition, attempt int) (e *ending, retry bool) {
	step := a.step1()
	b := a.entries()
	b.ops = append(b.ops, tr.ops...)
	b.add(step, EntryTransition, TransitionEntry{From: st.name, To: next.name, Data: tr.data, Ops: tr.ops})
	cs := a.cs
	cs.State, cs.Data, cs.Step = next.name, tr.data, step+1
	terminal := next.end != ""
	out := next.end.outcome()
	if terminal {
		b.add(step, EntryOutcome, out)
	}
	b.checkpoint(&cs)
	if terminal {
		b.ops = append(b.ops, a.finalOps(&out)...)
	}
	if b.err != nil {
		return &ending{err: b.err}, false
	}
	err := a.commit(ctx, b.ops, b.next)
	var conflict *store.ConflictError
	switch {
	case err == nil:
		a.checkpointed(&cs)
		if terminal {
			return &ending{out: a.outcome(&out)}, false
		}
		return nil, false
	case errors.As(err, &conflict) && conflict.IsObjectConflict():
		if attempt < a.c.retries {
			return nil, true
		}
		return a.fail(ctx, ReasonObjectConflict, fmt.Sprintf("state %q: %v, after %d re-runs", st.name, err, attempt)), false
	case errors.Is(err, store.ErrInvalid) && len(tr.ops) > 0:
		// The store refused the state's operations (a key it cannot hold,
		// say); nothing landed.
		return a.fail(ctx, ReasonStateError, fmt.Sprintf("state %q: %v", st.name, err)), false
	}
	return commitErr(err), false
}

// finalOps are the operations that end an activation after its outcome
// entry: the run record's change and the release (ADR 0003, item 10).
func (a *activation) finalOps(out *OutcomeEntry) []store.Op {
	return []store.Op{
		store.UpdateRunOp(store.RunUpdate{Run: a.run.ID, ExpectVersion: a.run.Version, Status: out.Status, Reason: out.Reason}),
		store.ReleaseOp(),
	}
}

func (a *activation) outcome(out *OutcomeEntry) Outcome {
	return Outcome{Status: out.Status, Reason: out.Reason, Detail: out.Detail, Steps: a.step1() - 1, CheckpointSeq: a.ckSeq}
}

// end commits the activation's ending outside a transition: its outcome
// entry, the run record's change and the release, in one commit.
func (a *activation) end(ctx context.Context, out OutcomeEntry) *ending {
	b := a.entries()
	// A terminal ending before the run's start landed records the start,
	// so the run's inputs are on the record (REC-1). A non-terminal one
	// does not: the next activation starts the run.
	if a.start1 != nil && out.Status.Terminal() {
		a.start1.add(b)
	}
	b.add(a.step1(), EntryOutcome, out)
	if b.err != nil {
		return &ending{err: b.err}
	}
	if err := a.commit(ctx, append(b.ops, a.finalOps(&out)...), b.next); err != nil {
		return commitErr(err)
	}
	return &ending{out: a.outcome(&out)}
}

// fail ends the run as failed with the given reason, unless the context has
// ended, whose cause then decides the ending (ADR 0003, item 17).
func (a *activation) fail(ctx context.Context, reason, detail string) *ending {
	if ctx.Err() != nil {
		return a.endByCause(ctx)
	}
	return a.end(ctx, OutcomeEntry{Status: store.StatusFailed, Reason: store.Reason{Code: reason}, Detail: detail})
}

// endByCause ends an activation whose context has ended, by the context's
// cause (ADR 0003, item 17).
func (a *activation) endByCause(ctx context.Context) *ending {
	cause := context.Cause(ctx)
	var budget *BudgetExhausted
	out := OutcomeEntry{Status: store.StatusRunnable, Reason: store.Reason{Code: ReasonShutdown}}
	switch {
	case errors.Is(cause, ErrSuperseded):
		out = OutcomeEntry{Status: store.StatusCancelled, Reason: store.Reason{Code: ReasonSuperseded}}
	case errors.Is(cause, ErrOperator):
		out = OutcomeEntry{Status: store.StatusCancelled, Reason: store.Reason{Code: ReasonOperator}}
	case errors.As(cause, &budget):
		out = OutcomeEntry{Status: store.StatusFailed, Reason: store.Reason{Code: ReasonBudgetExhausted, Budget: budget.Budget}}
	}
	return a.end(ctx, out)
}

// codeFailed ends an activation after a code state, Init or Migrate
// returned an error: by the context's cause if it has ended, as a store
// failure if the error wraps one, and otherwise as a run failure.
func (a *activation) codeFailed(ctx context.Context, err error) *ending {
	var read *ReadError
	switch {
	case ctx.Err() != nil:
		return a.endByCause(ctx)
	case errors.As(err, &read):
		return &ending{err: storeFailure(read.Err)}
	}
	return a.fail(ctx, ReasonStateError, err.Error())
}

// guard runs agent-type code, turning a panic into an error.
func guard(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn()
}
