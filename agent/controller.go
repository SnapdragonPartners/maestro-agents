package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Store is what the controller needs of a store: the fenced commit and the
// reads.
type Store interface {
	store.Committer
	store.Reader
}

// DefaultConflictRetries is how many times a code state, or a model
// state's atomic-local-transaction tool, is run again when its object
// operations conflict with another instance's commit (ADR 0005, item 10).
const DefaultConflictRetries = 3

// Controller runs activations (ADR 0003, item 12). It holds no per-run
// state, so one controller serves every instance a host runs, concurrently.
type Controller struct {
	store    Store
	registry *Registry
	policy   Policy
	retries  int
}

// Option configures a Controller.
type Option func(*Controller)

// WithConflictRetries sets how many times a code state is re-run on an
// object conflict before the run fails with ReasonObjectConflict, and how
// many times an atomic-local-transaction tool is re-run before its call's
// result becomes a tool error the model receives (ADR 0005, item 10); the
// run does not fail. Negative values count as zero.
func WithConflictRetries(n int) Option {
	return func(c *Controller) { c.retries = max(n, 0) }
}

// WithPolicy sets the policy hook every tool call passes through before its
// effect (TOOL-3). The default, AllowAll, allows every call; nil restores
// it.
func WithPolicy(p Policy) Option {
	return func(c *Controller) {
		if p == nil {
			p = AllowAll
		}
		c.policy = p
	}
}

// NewController returns a controller that runs the registry's agent types
// against s.
func NewController(s Store, r *Registry, opts ...Option) *Controller {
	c := &Controller{store: s, registry: r, retries: DefaultConflictRetries, policy: AllowAll}
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
	// Client is the model client the host built (LLM-1), used by every
	// model state this activation runs; the route it reports is recorded
	// with every attempt (LLM-3). A model state reached with no client
	// fails the run with ReasonModelError.
	Client llms.ChatClient
	// Scope is the run's scope: kind run, with the run's ID.
	Scope store.Scope
	// Fence is the host's claim on the run's instance (ADR 0003, item 8).
	Fence store.Fence
	// Config is the resolved configuration, recorded when the run starts
	// and used by every later activation in its place (RUN-1). A later
	// activation ignores this field.
	Config json.RawMessage
	// Namespace is the object-store namespace the run reads and writes
	// (STORE-2, ADR 0012), assigned by the host: instances given the same
	// namespace share its objects, and a run reaches no other. It is
	// required, recorded when the run starts and fixed for the run: a later
	// activation ignores this field. A run started with none, with one that
	// is not valid UTF-8, or with one the store refuses, fails with
	// ReasonConfigError.
	Namespace string
	// Budgets are the run's budgets (BUD-1, ADR 0009), recorded when the
	// run starts and fixed for the run: a later activation ignores this
	// field. A zero token, turn or active-time budget takes the agent
	// type's default (Type.Budgets, ADR 0011 item 7), and sets no limit
	// only if the type has none; a deadline or cost is never defaulted.
	Budgets Budgets
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
// The run's time budgets end ctx with a *BudgetExhausted cause (BUD-2).
func (c *Controller) Activate(ctx context.Context, in ActivationInput) (Outcome, error) { //nolint:gocritic // hugeParam: the input is passed by value, as ADR 0003 item 12 shows
	a, err := c.begin(ctx, &in)
	if err != nil {
		return Outcome{}, err
	}
	ctx, stop := a.clocks(ctx)
	defer stop()
	e := a.start(ctx, in.Config)
	for e == nil {
		switch {
		case ctx.Err() != nil:
			e = a.endByCause(ctx)
		case in.MaxSteps > 0 && a.steps >= in.MaxSteps:
			e = a.end(ctx, OutcomeEntry{Status: store.StatusRunnable, Reason: store.Reason{Code: ReasonStepLimit}})
		default:
			e = a.step(ctx)
		}
	}
	return e.out, e.err
}

// activation is one activation's view of its run. Only the activation
// writes the run's records, so the view stays exact (ADR 0005, item 5).
type activation struct {
	// clockStart is when this activation's clocks started (ADR 0009).
	clockStart time.Time
	client     llms.ChatClient
	m          *machine
	c          *Controller
	// model is the model state running, if any.
	model *modelRun
	// tail is what the execution record holds past the checkpoint.
	tail tailState
	// start1 is the run's start while it is not yet committed.
	start1 *runStart
	scope  store.Scope
	fence  store.Fence
	// config is the run's resolved configuration; cs.Config its digest.
	config json.RawMessage
	// namespace is the run's object-store namespace (ADR 0012): the
	// host's until the run's start lands, then the recorded one.
	namespace string
	run       store.Run
	cs        checkpointState
	// budgets are the run's budgets; used is what it has consumed of them
	// as of the entries committed, its active time as of the activation's
	// start (activeBase; consumed adds the time since clockStart).
	budgets Budgets
	used    Consumption
	// ckSeq is the run's latest checkpoint, nextCk the instance's next
	// checkpoint sequence.
	ckSeq  uint64
	nextCk uint64
	// nextEntry is the run's next execution-record sequence.
	nextEntry uint64
	// steps counts the steps this activation completed, maxSteps the
	// host's limit (zero: none).
	steps      int
	maxSteps   int
	activeBase time.Duration
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
	a := &activation{
		c: c, m: m, fence: in.Fence, run: run, scope: in.Scope, nextCk: 1, client: in.Client, maxSteps: max(in.MaxSteps, 0),
		namespace: in.Namespace,
		// A run not yet started takes the agent type's defaults for the
		// budgets the host leaves unset (ADR 0011, item 7), here, before
		// the activation's clocks are installed from them; a started run
		// takes its recorded budgets below.
		budgets: in.Budgets.withDefaults(&m.defaults),
	}
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
			// The run's budgets are the ones recorded with its start.
			a.budgets = Budgets{}
			if a.cs.Budgets != nil {
				a.budgets = *a.cs.Budgets
			}
			if a.cs.Consumed != nil {
				a.used = a.cs.Consumed.clone()
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

// readTail reads the entries after the checkpoint (ADR 0004, item 5) and
// folds them into a.tail, refusing a record this version cannot recover
// before anything is written.
func (a *activation) readTail(ctx context.Context, after uint64) error {
	tail, err := a.c.store.Entries(durable(ctx), a.run.ID, after)
	if err != nil {
		return storeFailure(err)
	}
	next := after + 1
	f := tailFold{used: &a.used}
	for i := range tail {
		e := &tail[i]
		if e.Seq != next {
			return storeFailure(fmt.Errorf("agent: run %q record has entry %d where %d belongs", a.run.ID, e.Seq, next))
		}
		if err := f.add(e); err != nil {
			return refuse("run %q record entry %d (%s) past its checkpoint: %v", a.run.ID, e.Seq, e.Kind, err)
		}
		next++
	}
	a.nextEntry = next
	// An attempt the tail leaves open was interrupted: it is charged its
	// reservation before any new attempt is admitted (ADR 0004, item 13).
	f.interrupt()
	if err := a.checkTail(&f); err != nil {
		return refuse("run %q: %v", a.run.ID, err)
	}
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
	// The run's namespace is the one recorded with its start, whatever the
	// host passes now (ADR 0012).
	if a.cs.Namespace == "" {
		return refuse("run %q checkpoint %d records no namespace (ADR 0012)", a.run.ID, c.Seq)
	}
	a.namespace = a.cs.Namespace
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
	var point *WrapUpPoint
	if a.model != nil {
		point = &a.model.point
	}
	return &Env{reader: a.c.store, namespace: a.namespace, config: a.config, run: a.run, state: state, step: a.step1(), budget: a.budgetStatus(point)}
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
	err    error
	a      *activation
	stored map[string]bool // artifacts already added
	ops    []store.Op
	next   uint64
	// pending reports that the model state's pending results are in ops.
	pending bool
}

func (a *activation) entries() *entries { return &entries{a: a, next: a.nextEntry} }

// payload is an entry payload: every one embeds Clock.
type payload interface{ stamp(time.Duration) }

// add adds an entry, stamped with the run's active time (ADR 0009, item
// 10).
func (b *entries) add(step uint64, kind string, p payload) {
	if b.err != nil {
		return
	}
	p.stamp(b.a.activeTime())
	raw, err := json.Marshal(p)
	if err != nil {
		b.err = storeFailure(fmt.Errorf("agent: encode %s entry: %w", kind, err))
		return
	}
	b.ops = append(b.ops, store.AppendEntryOp(store.Entry{Run: b.a.run.ID, Seq: b.next, Step: step, Kind: kind, Payload: raw}))
	b.next++
}

// checkpoint adds the checkpoint covering every entry added so far, with
// the run's consumption as of those entries (ADR 0009).
func (b *entries) checkpoint(cs *checkpointState) {
	if b.err != nil {
		return
	}
	used := b.a.consumed()
	cs.Consumed = &used
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

// outcome adds the activation's outcome entry, with the run's consumption.
func (b *entries) outcome(step uint64, out *OutcomeEntry) {
	used := b.a.consumed()
	out.Consumed = &used
	b.add(step, EntryOutcome, out)
}

// start commits activation_started, then either starts the run (Init, the
// run_started entry and the first checkpoint) or migrates a checkpoint
// from another version of the agent type.
func (a *activation) start(ctx context.Context, config json.RawMessage) *ending {
	b := a.entries()
	b.add(a.step1(), EntryActivationStarted, &ActivationStarted{
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
	start := &runStart{
		entry: RunStarted{
			AgentType: a.m.name, AgentVersion: a.m.version, Manifest: a.m.manifest.digest,
			TriggerID: a.run.TriggerID, TriggerKind: a.run.TriggerKind, Trigger: a.run.Trigger,
		},
		manifest: &store.Artifact{Digest: a.m.manifest.digest, MediaType: "application/json", Content: a.m.manifest.json},
	}
	a.start1 = start
	start.entry.Namespace = recordable(a.namespace)
	// The run's inputs are prepared before anything can refuse them, so a
	// start that fails records them (REC-1). a.budgets already holds the
	// agent type's defaults (begin).
	var budgets *Budgets
	if a.budgets != (Budgets{}) {
		budgets = &a.budgets
		start.entry.Budgets = budgets
	}
	if err := store.ValidJSON(config); err != nil {
		return a.fail(ctx, ReasonConfigError, "configuration: "+err.Error())
	}
	if config != nil {
		sum := sha256.Sum256(config)
		start.entry.Config = hex.EncodeToString(sum[:])
		start.config = &store.Artifact{Digest: start.entry.Config, MediaType: "application/json", Content: config}
	}
	if e := a.checkNamespace(ctx); e != nil {
		return e
	}
	if !a.m.triggers[a.run.TriggerKind] {
		return a.fail(ctx, ReasonConfigError, fmt.Sprintf(
			"trigger kind %q is not one agent type %q declares (MAN-2)", a.run.TriggerKind, a.m.name))
	}
	if a.m.config != nil {
		raw := config
		if raw == nil {
			raw = json.RawMessage("null")
		}
		if err := a.m.config.Validate(raw); err != nil {
			return a.fail(ctx, ReasonConfigError, "configuration: "+err.Error())
		}
	}
	if err := a.budgets.validate(); err != nil {
		return a.fail(ctx, ReasonConfigError, "budgets: "+err.Error())
	}
	if a.budgets.Cost > 0 {
		return a.refuseBudget(ctx, &budgetRefusal{ReasonBudgetUnenforceable, BudgetCost,
			"cost budgets are not built yet (MVP.md, BUD-1)"})
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
		Budgets: budgets, Namespace: a.namespace,
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

// checkNamespace checks the run's namespace before its start (ADR 0012,
// item 5): a namespace that is missing or that the store refuses fails the
// run with config_error, the host's configuration to fix; a store failure
// ends the activation with an error. It returns nil if the namespace is
// usable.
func (a *activation) checkNamespace(ctx context.Context) *ending {
	switch {
	case a.namespace == "":
		return a.fail(ctx, ReasonConfigError, "the host assigned no object-store namespace (STORE-2, ADR 0012)")
	case !utf8.ValidString(a.namespace):
		return a.fail(ctx, ReasonConfigError, "namespace is not valid UTF-8")
	}
	// Each store has its own limits on a namespace (the file store's on its
	// encoded length, say). A read that names it finds out before agent
	// code does, so a namespace the store refuses is the host's
	// configuration error, not the state's defect.
	if _, _, err := a.c.store.Object(durable(ctx), a.namespace, namespaceProbe); err != nil {
		if errors.Is(err, store.ErrInvalid) {
			return a.fail(ctx, ReasonConfigError, "namespace: "+err.Error())
		}
		return &ending{err: storeFailure(err)}
	}
	return nil
}

// runStart is a run's start, not yet committed: the manifest's artifact,
// the configuration's, if any, and the run_started entry.
type runStart struct {
	manifest *store.Artifact
	config   *store.Artifact
	entry    RunStarted
}

func (r *runStart) add(b *entries) {
	b.ops = append(b.ops, store.PutArtifactOp(*r.manifest))
	if r.config != nil {
		b.ops = append(b.ops, store.PutArtifactOp(*r.config))
	}
	b.add(1, EntryRunStarted, &r.entry)
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
	case st.model != nil:
		return a.modelStep(ctx, st)
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
		if err := a.inNamespace(op); err != nil {
			return nil, fmt.Errorf("state %q operation %d: %w", st.name, i, err)
		}
	}
	return a.m.states[tr.to], nil
}

// recordable returns s if the record can hold it as passed, and empty
// otherwise. A namespace that is not valid UTF-8 is refused at the run's
// start, and is not recorded: encoding/json would record U+FFFD in place
// of its invalid bytes, a value the host never passed.
func recordable(s string) string {
	if !utf8.ValidString(s) {
		return ""
	}
	return s
}

// namespaceProbe is the key a run's start reads to check that the store
// accepts the run's namespace.
const namespaceProbe = "namespace-probe"

// inNamespace refuses an object operation outside the run's namespace
// (STORE-2, ADR 0012). Every object operation agent code returns, a code
// state's or a tool's, passes here before it reaches a commit.
func (a *activation) inNamespace(op store.Op) error {
	ns := ""
	switch {
	case op.PutObject != nil:
		ns = op.PutObject.Namespace
	case op.DeleteObject != nil:
		ns = op.DeleteObject.Namespace
	}
	if ns != a.namespace {
		return fmt.Errorf("operation is in namespace %q, not the run's (ADR 0012)", ns)
	}
	return nil
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
	b.add(step, EntryTransition, &TransitionEntry{From: st.name, To: next.name, Data: tr.data, Ops: tr.ops})
	cs := a.cs
	cs.State, cs.Data, cs.Step = next.name, tr.data, step+1
	terminal := next.end != ""
	out := next.end.outcome()
	if terminal {
		b.outcome(step, &out)
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
		a.steps++
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
// entry: the run record's change, with the consumption the outcome entry
// records (ADR 0003 item 13, ADR 0005 item 3), and the release (ADR 0003,
// item 10).
func (a *activation) finalOps(out *OutcomeEntry) []store.Op {
	u := store.RunUpdate{Run: a.run.ID, ExpectVersion: a.run.Version, Status: out.Status, Reason: out.Reason}
	if out.Consumed != nil {
		// Consumption is plain data; it always encodes.
		u.Consumed, _ = json.Marshal(out.Consumed) //nolint:errchkjson // see above
	}
	return []store.Op{store.UpdateRunOp(u), store.ReleaseOp()}
}

func (a *activation) outcome(out *OutcomeEntry) Outcome {
	o := Outcome{Status: out.Status, Reason: out.Reason, Detail: out.Detail, Steps: a.step1() - 1, CheckpointSeq: a.ckSeq}
	if out.Consumed != nil {
		o.Consumed = *out.Consumed
	}
	return o
}

// end commits the activation's ending outside a transition: its outcome
// entry, the run record's change and the release, in one commit.
func (a *activation) end(ctx context.Context, out OutcomeEntry) *ending { //nolint:gocritic // hugeParam: callers pass literals; end stamps its own copy
	b := a.entries()
	// A terminal ending before the run's start landed records the start,
	// so the run's inputs are on the record (REC-1). A non-terminal one
	// does not: the next activation starts the run.
	if a.start1 != nil && out.Status.Terminal() {
		a.start1.add(b)
	}
	// A model state's results not yet committed go with the ending, and
	// with the turn's checkpoint if they complete it (ADR 0008, item 13).
	var cs *checkpointState
	step := a.step1()
	if a.model != nil {
		if cs = a.model.endEntries(b); cs != nil {
			step = cs.Step
		}
	}
	b.outcome(step, &out)
	if b.err != nil {
		return &ending{err: b.err}
	}
	if err := a.commit(ctx, append(b.ops, a.finalOps(&out)...), b.next); err != nil {
		return commitErr(err)
	}
	if a.model != nil && b.pending {
		a.model.pending = nil
	}
	if cs != nil {
		a.model.completed(cs)
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

// refuseBudget ends the run on an attempt its budgets refuse (BUD-1,
// BUD-3), unless the context has ended, whose cause then decides.
func (a *activation) refuseBudget(ctx context.Context, r *budgetRefusal) *ending {
	if ctx.Err() != nil {
		return a.endByCause(ctx)
	}
	return a.end(ctx, OutcomeEntry{
		Status: store.StatusFailed, Reason: store.Reason{Code: r.code, Budget: r.budget}, Detail: r.detail,
	})
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
