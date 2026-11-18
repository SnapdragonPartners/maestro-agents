package standalone

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// The claim's lease and its heartbeat (ADR 0017, item 14).
const (
	leaseDuration     = time.Minute
	heartbeatInterval = 20 * time.Second
)

// errHeartbeatLost is the cause with which a heartbeat that finds its
// ownership gone cancels its activation (ADR 0017, item 18). It is a
// shutdown cause to the controller, whose next commit then fails.
var errHeartbeatLost = errors.New("standalone: ownership lost at heartbeat")

// errAnotherConfiguration refuses an input whose run belongs to another
// agent type (ADR 0017, item 9).
var errAnotherConfiguration = errors.New("the store serves another configuration")

// host is one run command's state.
type host struct {
	st store.Store
	// locks serializes activations per instance in the process.
	locks  instanceLocks
	cfg    *Config
	ctrl   *agent.Controller
	client *agent.ModelClient
	logger *slog.Logger
	now    func() time.Time
	// submitted holds the program's submitted agent types (ADR 0021).
	submitted map[string]bool
	owner     string
	// maxInput bounds an input file.
	maxInput int64
	// heartbeat is heartbeatInterval, shortened by tests.
	heartbeat time.Duration
}

func newOwnerID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("standalone-%d-%s", os.Getpid(), hex.EncodeToString(b[:]))
}

// result is what became of one run: its outcome, or the error that left
// it without a confirmed one and the status reread after it.
type result struct {
	// Err is the activation's error, nil for a confirmed outcome.
	Err error
	// Status and Reason are the run's: the outcome's, or reread after an
	// error (empty if the reread failed).
	Reason   store.Reason
	Run      string
	Instance string
	Status   store.Status
	Outcome  agent.Outcome
	// Fatal marks a store that must be reopened (ADR 0017, item 10).
	Fatal bool
}

// interrupted reports whether the run was left unfinished.
func (r *result) interrupted() bool { return !r.Status.Terminal() }

// makeRun turns a trigger into its run (ADR 0017, item 12): enqueue the
// trigger, create the run, remove the trigger, each idempotent on the
// trigger's identity. A file trigger's run ID is its instance's; a
// submitted one's is its own (ADR 0021, item 2).
func (h *host) makeRun(ctx context.Context, t *store.Trigger, runID, agentType string) (store.Run, error) {
	if _, err := h.st.EnqueueTrigger(ctx, *t); err != nil {
		return store.Run{}, fmt.Errorf("enqueue trigger: %w", err)
	}
	run, created, err := h.st.CreateRun(ctx, store.Run{
		ID: runID, Instance: t.Instance, AgentType: agentType,
		TriggerID: t.ID, TriggerKind: t.Kind, Trigger: t.Payload,
	})
	if err != nil {
		return store.Run{}, fmt.Errorf("create run: %w", err)
	}
	if err := h.st.RemoveTrigger(ctx, t.Instance, t.ID); err != nil {
		return store.Run{}, fmt.Errorf("remove trigger: %w", err)
	}
	if run.AgentType != agentType {
		return store.Run{}, fmt.Errorf("%w: run %s is agent type %q's, not %q's", errAnotherConfiguration, run.ID, run.AgentType, agentType)
	}
	if created {
		h.logger.Info("standalone: run created", "run", run.ID, "agent_type", agentType, "trigger_kind", t.Kind)
	}
	return run, nil
}

// makeFileRun makes an input file's run, of the configured agent type.
func (h *host) makeFileRun(ctx context.Context, in *input, kind string) (store.Run, error) {
	t := store.Trigger{Instance: in.instance, ID: in.id, Kind: kind, Payload: in.payload}
	return h.makeRun(ctx, &t, in.instance, h.cfg.AgentType)
}

// activate claims a run's instance and activates the run (ADR 0017, items
// 14, 17 and 18). It always returns a result.
func (h *host) activate(ctx context.Context, run *store.Run) *result {
	dctx := context.WithoutCancel(ctx)
	own, err := h.st.Claim(dctx, run.Instance, h.owner, leaseDuration)
	if err != nil {
		return h.afterError(dctx, run, fmt.Errorf("claim: %w", err), nil)
	}
	fence := own.Fence()
	actx, cancel := context.WithCancelCause(ctx)
	beat := h.startHeartbeat(actx, cancel, fence)
	in := agent.ActivationInput{
		Client:    h.client,
		Scope:     store.Scope{Kind: store.ScopeRun, ID: run.ID},
		Fence:     fence,
		Config:    h.cfg.instanceConfig(),
		Namespace: h.cfg.Namespace,
		Principal: h.cfg.Principal,
		Budgets:   h.cfg.Budgets.budgets(h.now()),
	}
	if run.AgentType != h.cfg.AgentType {
		// A submitted type's configuration is absent and its budgets its
		// manifest's defaults (ADR 0021, item 1).
		in.Config, in.Budgets = json.RawMessage("null"), agent.Budgets{}
	}
	out, err := h.ctrl.Activate(actx, in)
	cancel(nil)
	if fatal := <-beat; fatal != nil {
		// The store must be reopened: whatever the activation returned,
		// the host stops (ADR 0017, item 10).
		r := h.afterError(dctx, run, fatal, nil)
		r.Fatal = true
		return r
	}
	if err != nil {
		return h.afterError(dctx, run, err, &fence)
	}
	h.logger.Info("standalone: run activated", "run", run.ID, "status", out.Status, "reason", out.Reason.Code,
		"budget", out.Reason.Budget, "steps", out.Steps)
	return &result{Run: run.ID, Instance: run.Instance, Outcome: out, Status: out.Status, Reason: out.Reason}
}

// afterError handles an activation without a confirmed outcome (ADR 0003,
// item 14): release the claim if still held, then reread the run.
func (h *host) afterError(ctx context.Context, run *store.Run, err error, fence *store.Fence) *result {
	r := &result{Run: run.ID, Instance: run.Instance, Err: err, Fatal: errors.Is(err, store.ErrUnusable)}
	h.logger.Error("standalone: activation has no confirmed outcome", "run", run.ID, "error_class", errorClass(err))
	if r.Fatal {
		return r
	}
	if fence != nil {
		if rerr := h.st.Release(ctx, *fence); rerr != nil && !errors.Is(rerr, store.ErrOwnership) {
			h.logger.Warn("standalone: release failed", "run", run.ID, "error_class", errorClass(rerr))
			r.Fatal = errors.Is(rerr, store.ErrUnusable)
		}
	}
	reread, rerr := h.st.Run(ctx, run.ID)
	if rerr != nil {
		h.logger.Warn("standalone: reread failed", "run", run.ID, "error_class", errorClass(rerr))
		r.Fatal = r.Fatal || errors.Is(rerr, store.ErrUnusable)
		return r
	}
	r.Status, r.Reason = reread.Status, reread.Reason
	return r
}

// startHeartbeat renews the claim's lease until ctx ends. Its channel
// delivers one value when it has stopped: the store.ErrUnusable that
// stopped it, after cancelling the activation (ADR 0017, item 10), or nil.
func (h *host) startHeartbeat(ctx context.Context, cancel context.CancelCauseFunc, f store.Fence) <-chan error {
	done := make(chan error, 1)
	go func() {
		var fatal error
		defer func() { done <- fatal }()
		t := time.NewTicker(h.heartbeat)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			_, err := h.st.Heartbeat(context.WithoutCancel(ctx), f, leaseDuration)
			switch {
			case err == nil:
			case errors.Is(err, store.ErrOwnership):
				if h.releasedByUs(ctx, f) {
					return // the activation's final commit released the claim
				}
				h.logger.Error("standalone: ownership lost at heartbeat", "instance", f.Instance)
				cancel(errHeartbeatLost)
				return
			case errors.Is(err, store.ErrUnusable):
				h.logger.Error("standalone: the store must be reopened", "instance", f.Instance)
				fatal = fmt.Errorf("heartbeat: %w", err)
				cancel(fatal)
				return
			default:
				h.logger.Warn("standalone: heartbeat failed", "instance", f.Instance, "error_class", errorClass(err))
			}
		}
	}()
	return done
}

// releasedByUs reports whether the instance has no owner at the fence's
// epoch: the activation's final commit released it, racing the heartbeat
// (ADR 0003, item 10). A claim by anyone else moves the epoch.
func (h *host) releasedByUs(ctx context.Context, f store.Fence) bool {
	own, err := h.st.Ownership(context.WithoutCancel(ctx), f.Instance)
	return err == nil && own.Owner == "" && own.Epoch == f.Epoch
}

// errorClass names an error's class for a log line, never its text, which
// can quote content (ADR 0016, item 10).
func errorClass(err error) string {
	for _, c := range []struct {
		err  error
		name string
	}{
		{errAnotherConfiguration, "another_configuration"},
		{agent.ErrOwnershipLost, "ownership_lost"},
		{agent.ErrNotActivatable, "not_activatable"},
		{store.ErrUnusable, "store_unusable"},
		{agent.ErrStoreFailure, "store_failure"},
		{store.ErrOwnership, "ownership"},
		{store.ErrNotClaimable, "not_claimable"},
		{store.ErrNotFound, "not_found"},
		{store.ErrInvalid, "invalid"},
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "deadline_exceeded"},
	} {
		if errors.Is(err, c.err) {
			return c.name
		}
	}
	return "other"
}
