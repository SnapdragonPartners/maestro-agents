package storecore

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// rlock takes the read lock and checks the store is usable. On success the
// caller must call e.mu.RUnlock.
func (e *Engine) rlock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("store: read not started: %w", err)
	}
	e.mu.RLock()
	if err := e.usable(); err != nil {
		e.mu.RUnlock()
		return err
	}
	return nil
}

// Ownership implements store.Reader.
func (e *Engine) Ownership(ctx context.Context, instance string) (store.Ownership, error) {
	if err := e.rlock(ctx); err != nil {
		return store.Ownership{}, err
	}
	defer e.mu.RUnlock()
	_, f, err := e.readOwnership(instance)
	if err != nil {
		return store.Ownership{}, err
	}
	return e.ownership(instance, f), nil
}

// Claimable implements store.Reader.
func (e *Engine) Claimable(ctx context.Context) ([]store.Claimable, error) {
	if err := e.rlock(ctx); err != nil {
		return nil, err
	}
	defer e.mu.RUnlock()
	names, err := e.b.List("instances")
	if err != nil {
		return nil, fmt.Errorf("store: list instances: %w", err)
	}
	now := e.now()
	var out []store.Claimable
	for _, name := range names {
		instance, ok := decode(name)
		if !ok {
			continue
		}
		_, f, err := e.readOwnership(instance)
		if err != nil {
			return nil, err
		}
		if e.ownership(instance, f).Live(now) {
			continue
		}
		run, ok, err := e.activeRun(name)
		if err != nil {
			return nil, err
		}
		if ok && run.Status == store.StatusRunnable {
			out = append(out, store.Claimable{Instance: instance, Run: run.ID})
		}
	}
	slices.SortFunc(out, func(a, b store.Claimable) int { return strings.Compare(a.Instance, b.Instance) })
	return out, nil
}

// Run implements store.Reader.
func (e *Engine) Run(ctx context.Context, id string) (store.Run, error) {
	if err := e.rlock(ctx); err != nil {
		return store.Run{}, err
	}
	defer e.mu.RUnlock()
	run, ok, err := e.readRun(id)
	if err != nil {
		return store.Run{}, err
	}
	if !ok {
		return store.Run{}, fmt.Errorf("%w: run %q", store.ErrNotFound, id)
	}
	return run, nil
}

// ActiveRun implements store.Reader.
func (e *Engine) ActiveRun(ctx context.Context, instance string) (store.Run, bool, error) {
	inst, err := component("instance", instance)
	if err != nil {
		return store.Run{}, false, err
	}
	if err := e.rlock(ctx); err != nil {
		return store.Run{}, false, err
	}
	defer e.mu.RUnlock()
	return e.activeRun(inst)
}

// Triggers implements store.Reader.
func (e *Engine) Triggers(ctx context.Context, instance string) ([]store.Trigger, error) {
	inst, err := component("instance", instance)
	if err != nil {
		return nil, err
	}
	if err = e.rlock(ctx); err != nil {
		return nil, err
	}
	defer e.mu.RUnlock()
	out, _, err := e.triggers(inst)
	return out, err
}

// seqs lists the sequence numbers in a directory, ascending.
func (e *Engine) seqs(dir string) ([]uint64, error) {
	names, err := e.b.List(dir)
	if err != nil {
		return nil, fmt.Errorf("store: list %s: %w", dir, err)
	}
	var out []uint64
	for _, name := range names {
		if n, ok := parseSeqName(name); ok {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}

// LatestCheckpoint implements store.Reader.
func (e *Engine) LatestCheckpoint(ctx context.Context, instance string) (store.Checkpoint, bool, error) {
	inst, err := component("instance", instance)
	if err != nil {
		return store.Checkpoint{}, false, err
	}
	if err = e.rlock(ctx); err != nil {
		return store.Checkpoint{}, false, err
	}
	defer e.mu.RUnlock()
	seqs, err := e.seqs(checkpointDir(inst))
	if err != nil || len(seqs) == 0 {
		return store.Checkpoint{}, false, err
	}
	var c store.Checkpoint
	found, err := readJSON(e.b, checkpointDir(inst)+"/"+seqName(seqs[len(seqs)-1]), &c)
	return c, found, err
}

// Checkpoint implements store.Reader.
func (e *Engine) Checkpoint(ctx context.Context, instance string, seq uint64) (store.Checkpoint, error) {
	inst, err := component("instance", instance)
	if err != nil {
		return store.Checkpoint{}, err
	}
	if err = e.rlock(ctx); err != nil {
		return store.Checkpoint{}, err
	}
	defer e.mu.RUnlock()
	var c store.Checkpoint
	found, err := readJSON(e.b, checkpointDir(inst)+"/"+seqName(seq), &c)
	if err != nil {
		return store.Checkpoint{}, err
	}
	if !found {
		return store.Checkpoint{}, fmt.Errorf("%w: checkpoint %d of instance %q", store.ErrNotFound, seq, instance)
	}
	return c, nil
}

// Entries implements store.Reader.
func (e *Engine) Entries(ctx context.Context, run string, after uint64) ([]store.Entry, error) {
	enc, err := component("run", run)
	if err != nil {
		return nil, err
	}
	if err = e.rlock(ctx); err != nil {
		return nil, err
	}
	defer e.mu.RUnlock()
	return readSeqDir[store.Entry](e, recordDir(enc), after)
}

// readSeqDir reads the sequence-named files in dir with sequence numbers
// greater than after, in order. The caller holds mu for reading.
func readSeqDir[T any](e *Engine, dir string, after uint64) ([]T, error) {
	seqs, err := e.seqs(dir)
	if err != nil {
		return nil, err
	}
	var out []T
	for _, n := range seqs {
		if n <= after {
			continue
		}
		var v T
		found, err := readJSON(e.b, dir+"/"+seqName(n), &v)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, v)
		}
	}
	return out, nil
}

// Artifact implements store.Reader.
func (e *Engine) Artifact(ctx context.Context, digest string) (store.ArtifactMeta, []byte, error) {
	if !validDigest(digest) {
		return store.ArtifactMeta{}, nil, fmt.Errorf("%w: artifact digest %q is not lowercase hex SHA-256", store.ErrInvalid, digest)
	}
	if err := e.rlock(ctx); err != nil {
		return store.ArtifactMeta{}, nil, err
	}
	defer e.mu.RUnlock()
	var meta store.ArtifactMeta
	found, err := readJSON(e.b, artifactMetaPath(digest), &meta)
	if err != nil {
		return store.ArtifactMeta{}, nil, err
	}
	if !found {
		return store.ArtifactMeta{}, nil, fmt.Errorf("%w: artifact %s", store.ErrNotFound, digest)
	}
	content, ok, err := e.b.Read(artifactPath(digest))
	if err != nil {
		return store.ArtifactMeta{}, nil, fmt.Errorf("store: read artifact %s: %w", digest, err)
	}
	if !ok {
		return store.ArtifactMeta{}, nil, fmt.Errorf("store: corrupt artifact %s: metadata without content", digest)
	}
	return meta, content, nil
}

// Object implements store.Reader.
func (e *Engine) Object(ctx context.Context, namespace, key string) (store.Object, bool, error) {
	path, err := objectPathFor(namespace, key)
	if err != nil {
		return store.Object{}, false, err
	}
	if err = e.rlock(ctx); err != nil {
		return store.Object{}, false, err
	}
	defer e.mu.RUnlock()
	var f objectFile
	found, err := readJSON(e.b, path, &f)
	if err != nil {
		return store.Object{}, false, err
	}
	obj := store.Object{Namespace: namespace, Key: key, Version: f.Version}
	if !found || f.Deleted {
		return obj, false, nil
	}
	obj.JSON, obj.Bytes = f.JSON, f.Bytes
	return obj, true, nil
}

// ListObjects implements store.Reader.
func (e *Engine) ListObjects(ctx context.Context, namespace, prefix string) ([]store.ObjectInfo, error) {
	ns, err := component("namespace", namespace)
	if err != nil {
		return nil, err
	}
	if err = e.rlock(ctx); err != nil {
		return nil, err
	}
	defer e.mu.RUnlock()
	names, err := e.b.List(objectDir(ns))
	if err != nil {
		return nil, fmt.Errorf("store: list namespace %q: %w", namespace, err)
	}
	var out []store.ObjectInfo
	for _, name := range names {
		base, ok := strings.CutSuffix(name, ".json")
		if !ok {
			continue
		}
		key, ok := decode(base)
		if !ok || !strings.HasPrefix(key, prefix) {
			continue
		}
		var f objectFile
		found, err := readJSON(e.b, objectDir(ns)+"/"+name, &f)
		if err != nil {
			return nil, err
		}
		if found && !f.Deleted {
			out = append(out, store.ObjectInfo{Key: key, Version: f.Version})
		}
	}
	slices.SortFunc(out, func(a, b store.ObjectInfo) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}
