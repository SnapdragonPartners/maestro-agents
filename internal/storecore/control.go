package storecore

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// ownership combines the ownership file with the in-memory lease. The
// caller holds mu.
func (e *Engine) ownership(instance string, f ownershipFile) store.Ownership {
	o := store.Ownership{Instance: instance, Owner: f.Owner, Epoch: f.Epoch}
	if f.Owner != "" {
		o.LeaseExpiry = e.leases[instance]
	}
	return o
}

func (e *Engine) readOwnership(instance string) (string, ownershipFile, error) {
	inst, err := component("instance", instance)
	if err != nil {
		return "", ownershipFile{}, err
	}
	var f ownershipFile
	if _, err := readJSON(e.b, ownershipPath(inst), &f); err != nil {
		return "", ownershipFile{}, err
	}
	return inst, f, nil
}

// Claim implements store.ControlPlane.
func (e *Engine) Claim(ctx context.Context, instance, owner string, lease time.Duration) (store.Ownership, error) {
	if err := ctx.Err(); err != nil {
		return store.Ownership{}, fmt.Errorf("store: claim not started: %w", err)
	}
	if owner == "" || lease <= 0 {
		return store.Ownership{}, fmt.Errorf("%w: a claim needs an owner and a positive lease", store.ErrInvalid)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.usable(); err != nil {
		return store.Ownership{}, err
	}
	inst, cur, err := e.readOwnership(instance)
	if err != nil {
		return store.Ownership{}, err
	}
	now := e.now()
	if e.ownership(instance, cur).Live(now) {
		return store.Ownership{}, fmt.Errorf("%w: instance %q", store.ErrNotClaimable, instance)
	}
	next := ownershipFile{Owner: owner, Epoch: cur.Epoch + 1}
	data, err := marshal(next)
	if err != nil {
		return store.Ownership{}, err
	}
	if err := e.apply([]Write{{Path: ownershipPath(inst), Data: data}}); err != nil {
		return store.Ownership{}, err
	}
	e.leases[instance] = now.Add(lease)
	return e.ownership(instance, next), nil
}

// checkFence verifies a control-plane write's fence. The caller holds mu.
func (e *Engine) checkFence(f store.Fence) (string, ownershipFile, error) {
	inst, cur, err := e.readOwnership(f.Instance)
	if err != nil {
		return "", ownershipFile{}, err
	}
	if cur.Owner == "" || cur.Owner != f.Owner || cur.Epoch != f.Epoch {
		return "", ownershipFile{}, fmt.Errorf("%w: instance %q is owned by %q at epoch %d",
			store.ErrOwnership, f.Instance, cur.Owner, cur.Epoch)
	}
	return inst, cur, nil
}

// Heartbeat implements store.ControlPlane.
func (e *Engine) Heartbeat(ctx context.Context, f store.Fence, lease time.Duration) (store.Ownership, error) {
	if err := ctx.Err(); err != nil {
		return store.Ownership{}, fmt.Errorf("store: heartbeat not started: %w", err)
	}
	if lease <= 0 {
		return store.Ownership{}, fmt.Errorf("%w: a heartbeat needs a positive lease", store.ErrInvalid)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.usable(); err != nil {
		return store.Ownership{}, err
	}
	_, cur, err := e.checkFence(f)
	if err != nil {
		return store.Ownership{}, err
	}
	e.leases[f.Instance] = e.now().Add(lease)
	return e.ownership(f.Instance, cur), nil
}

// Release implements store.ControlPlane.
func (e *Engine) Release(ctx context.Context, f store.Fence) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("store: release not started: %w", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.usable(); err != nil {
		return err
	}
	inst, cur, err := e.checkFence(f)
	if err != nil {
		return err
	}
	data, err := marshal(ownershipFile{Epoch: cur.Epoch})
	if err != nil {
		return err
	}
	if err := e.apply([]Write{{Path: ownershipPath(inst), Data: data}}); err != nil {
		return err
	}
	delete(e.leases, f.Instance)
	return nil
}

// triggers reads an instance's queued triggers in arrival order, with their
// file names. The caller holds mu.
func (e *Engine) triggers(inst string) ([]store.Trigger, []string, error) {
	names, err := e.b.List(triggerDir(inst))
	if err != nil {
		return nil, nil, fmt.Errorf("store: list triggers: %w", err)
	}
	slices.Sort(names)
	var out []store.Trigger
	var paths []string
	for _, name := range names {
		if _, ok := parseSeqName(name); !ok {
			continue
		}
		var t store.Trigger
		path := triggerDir(inst) + "/" + name
		found, err := readJSON(e.b, path, &t)
		if err != nil {
			return nil, nil, err
		}
		if found {
			out = append(out, t)
			paths = append(paths, path)
		}
	}
	return out, paths, nil
}

// EnqueueTrigger implements store.ControlPlane.
func (e *Engine) EnqueueTrigger(ctx context.Context, t store.Trigger) (store.Trigger, error) {
	if err := ctx.Err(); err != nil {
		return store.Trigger{}, fmt.Errorf("store: enqueue not started: %w", err)
	}
	inst, err := component("instance", t.Instance)
	if err != nil {
		return store.Trigger{}, err
	}
	if _, err = component("trigger", t.ID); err != nil {
		return store.Trigger{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err = e.usable(); err != nil {
		return store.Trigger{}, err
	}
	queued, _, err := e.triggers(inst)
	if err != nil {
		return store.Trigger{}, err
	}
	var last uint64
	for _, q := range queued {
		if q.ID == t.ID {
			return q, nil
		}
		last = max(last, q.Seq)
	}
	t.Seq = last + 1
	t.EnqueuedAt = e.now().UTC()
	data, err := marshal(t)
	if err != nil {
		return store.Trigger{}, err
	}
	if err := e.apply([]Write{{Path: triggerDir(inst) + "/" + seqName(t.Seq), Data: data}}); err != nil {
		return store.Trigger{}, err
	}
	return t, nil
}

// RemoveTrigger implements store.ControlPlane.
func (e *Engine) RemoveTrigger(ctx context.Context, instance, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("store: remove not started: %w", err)
	}
	inst, err := component("instance", instance)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err = e.usable(); err != nil {
		return err
	}
	queued, paths, err := e.triggers(inst)
	if err != nil {
		return err
	}
	for i, q := range queued {
		if q.ID == id {
			return e.apply([]Write{{Path: paths[i], Delete: true}})
		}
	}
	return nil
}

// activeRun finds an instance's non-terminal run. The caller holds mu.
func (e *Engine) activeRun(inst string) (store.Run, bool, error) {
	names, err := e.b.List(runIndexDir(inst))
	if err != nil {
		return store.Run{}, false, fmt.Errorf("store: list runs: %w", err)
	}
	for _, name := range names {
		var idx runIndexFile
		found, err := readJSON(e.b, runIndexDir(inst)+"/"+name, &idx)
		if err != nil {
			return store.Run{}, false, err
		}
		if !found {
			continue
		}
		run, ok, err := e.readRun(idx.Run)
		if err != nil {
			return store.Run{}, false, err
		}
		if ok && !run.Status.Terminal() {
			return run, true, nil
		}
	}
	return store.Run{}, false, nil
}

func (e *Engine) readRun(id string) (store.Run, bool, error) {
	enc, err := component("run", id)
	if err != nil {
		return store.Run{}, false, err
	}
	var run store.Run
	found, err := readJSON(e.b, runPath(enc), &run)
	return run, found, err
}

// CreateRun implements store.ControlPlane.
func (e *Engine) CreateRun(ctx context.Context, r store.Run) (store.Run, bool, error) {
	if err := ctx.Err(); err != nil {
		return store.Run{}, false, fmt.Errorf("store: create run not started: %w", err)
	}
	inst, err := component("instance", r.Instance)
	if err != nil {
		return store.Run{}, false, err
	}
	trig, err := component("trigger", r.TriggerID)
	if err != nil {
		return store.Run{}, false, err
	}
	runEnc, err := component("run", r.ID)
	if err != nil {
		return store.Run{}, false, err
	}
	if r.AgentType == "" {
		return store.Run{}, false, fmt.Errorf("%w: run %q has no agent type", store.ErrInvalid, r.ID)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err = e.usable(); err != nil {
		return store.Run{}, false, err
	}
	existing, found, err := e.runForTrigger(inst, trig, r.TriggerID)
	if err != nil || found {
		return existing, false, err
	}
	if err = e.checkNewRun(inst, r); err != nil {
		return store.Run{}, false, err
	}
	now := e.now().UTC()
	r.Status, r.Reason, r.Version, r.CreatedAt, r.UpdatedAt = store.StatusRunnable, store.Reason{}, 1, now, now
	runData, err := marshal(r)
	if err != nil {
		return store.Run{}, false, err
	}
	idxData, err := marshal(runIndexFile{Run: r.ID})
	if err != nil {
		return store.Run{}, false, err
	}
	if err = e.apply([]Write{
		{Path: runPath(runEnc), Data: runData},
		{Path: runIndexPath(inst, trig), Data: idxData},
	}); err != nil {
		return store.Run{}, false, err
	}
	return r, true, nil
}

// runForTrigger finds the run created for (instance, trigger). The caller
// holds mu.
func (e *Engine) runForTrigger(inst, trig, triggerID string) (store.Run, bool, error) {
	var idx runIndexFile
	found, err := readJSON(e.b, runIndexPath(inst, trig), &idx)
	if err != nil || !found {
		return store.Run{}, false, err
	}
	run, ok, err := e.readRun(idx.Run)
	if err != nil {
		return store.Run{}, false, err
	}
	if !ok {
		return store.Run{}, false, fmt.Errorf("store: corrupt index: trigger %q names missing run %q", triggerID, idx.Run)
	}
	return run, true, nil
}

// checkNewRun refuses a run ID in use and a busy instance. The caller holds
// mu.
func (e *Engine) checkNewRun(inst string, r store.Run) error {
	_, exists, err := e.readRun(r.ID)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%w: run ID %q is already in use", store.ErrInvalid, r.ID)
	}
	active, busy, err := e.activeRun(inst)
	if err != nil {
		return err
	}
	if busy {
		return fmt.Errorf("%w: instance %q has run %q", store.ErrInstanceBusy, r.Instance, active.ID)
	}
	return nil
}
