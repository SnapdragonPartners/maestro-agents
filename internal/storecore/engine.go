package storecore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Options configures an Engine.
type Options struct {
	// Now is the store's clock, used for leases and timestamps. Nil means
	// time.Now.
	Now func() time.Time
	// Pause is the test-only pause hook (ADR 0005, item 6).
	Pause store.PauseHook
}

// Engine implements store.Store over a Backend.
//
// One RWMutex serializes commits and control-plane writes (Lock) against
// each other and against reads (RLock), so a read never sees a commit half
// applied. Leases live in memory only (ADR 0005, item 16); the ownership
// file holds the owner and the epoch.
type Engine struct {
	b      Backend
	now    func() time.Time
	pause  store.PauseHook
	leases map[string]time.Time
	// unusable is set when the store is closed or a failure past a commit
	// point has poisoned it.
	unusable error
	mu       sync.RWMutex
}

var _ store.Store = (*Engine)(nil)

// New returns an engine over b.
func New(b Backend, o Options) *Engine {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Engine{b: b, now: now, pause: o.Pause, leases: map[string]time.Time{}}
}

// ownershipFile is the ownership record on disk: owner and epoch, never the
// lease.
type ownershipFile struct {
	Owner string `json:"owner,omitempty"`
	Epoch uint64 `json:"epoch"`
}

// objectFile is an object, or the tombstone a deletion leaves so the key's
// version never repeats.
type objectFile struct {
	JSON    json.RawMessage `json:"json,omitempty"`
	Bytes   []byte          `json:"bytes"`
	Version uint64          `json:"version"`
	Deleted bool            `json:"deleted,omitempty"`
}

// runIndexFile maps an (instance, trigger) pair to its run.
type runIndexFile struct {
	Run string `json:"run"`
}

type reader interface {
	Read(path string) ([]byte, bool, error)
}

func readJSON(r reader, path string, v any) (bool, error) {
	data, ok, err := r.Read(path)
	if err != nil {
		return false, fmt.Errorf("store: read %s: %w", path, err)
	}
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("store: corrupt record %s: %w", path, err)
	}
	return true, nil
}

func marshal(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", store.ErrInvalid, err)
	}
	return data, nil
}

// usable reports ErrUnusable, with its cause, for a closed or poisoned
// store. The caller holds mu.
func (e *Engine) usable() error {
	if e.unusable != nil {
		return fmt.Errorf("%w: %w", store.ErrUnusable, e.unusable)
	}
	return nil
}

// apply writes through the backend and poisons the store on an
// indeterminate failure. The caller holds mu for writing.
func (e *Engine) apply(writes []Write) error {
	err := e.b.Apply(writes)
	var ind *IndeterminateError
	if errors.As(err, &ind) {
		e.unusable = err
	}
	if err != nil {
		return fmt.Errorf("store: apply: %w", err)
	}
	return nil
}

// Close implements store.Store.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if errors.Is(e.unusable, errClosed) {
		return nil
	}
	e.unusable = errClosed
	if err := e.b.Close(); err != nil {
		return fmt.Errorf("store: close: %w", err)
	}
	return nil
}

var errClosed = errors.New("store closed")

// tx overlays a commit's pending writes on the backend, so each operation's
// precondition sees the commit's earlier operations (ADR 0005, item 13).
type tx struct {
	b     Backend
	over  map[string]int
	order []Write
}

func newTx(b Backend) *tx { return &tx{b: b, over: map[string]int{}} }

func (t *tx) Read(path string) ([]byte, bool, error) {
	if i, ok := t.over[path]; ok {
		w := t.order[i]
		return w.Data, !w.Delete, nil
	}
	data, ok, err := t.b.Read(path)
	if err != nil {
		return nil, false, fmt.Errorf("store: read %s: %w", path, err)
	}
	return data, ok, nil
}

func (t *tx) set(w Write) {
	if i, ok := t.over[w.Path]; ok {
		t.order[i] = w
		return
	}
	t.over[w.Path] = len(t.order)
	t.order = append(t.order, w)
}

func (t *tx) putJSON(path string, v any) error {
	data, err := marshal(v)
	if err != nil {
		return err
	}
	t.set(Write{Path: path, Data: data})
	return nil
}

// Commit implements store.Committer.
func (e *Engine) Commit(ctx context.Context, f store.Fence, ops []store.Op) error {
	inst, err := checkCommit(ctx, f, ops)
	if err != nil {
		return err
	}
	if e.pause != nil {
		e.pause(ctx, store.PauseBeforeCommit, f)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err = e.usable(); err != nil {
		return err
	}
	var own ownershipFile
	if _, err = readJSON(e.b, ownershipPath(inst), &own); err != nil {
		return err
	}
	if e.pause != nil {
		e.pause(ctx, store.PauseAfterOwnershipRead, f)
	}
	if own.Owner == "" || own.Owner != f.Owner || own.Epoch != f.Epoch {
		return fmt.Errorf("%w: instance %q is owned by %q at epoch %d, fence names %q at epoch %d",
			store.ErrOwnership, f.Instance, own.Owner, own.Epoch, f.Owner, f.Epoch)
	}
	t := newTx(e.b)
	release := false
	for i, op := range ops {
		if err = e.applyOp(t, f, inst, own, i, op); err != nil {
			return err
		}
		release = release || op.Kind == store.OpRelease
	}
	// A commit whose operations change nothing (a re-stored artifact, say)
	// has passed the fence and its preconditions; it lands without touching
	// the backend, so a no-op cannot fail or journal.
	if len(t.order) == 0 {
		return nil
	}
	if err = e.apply(t.order); err != nil {
		return err
	}
	if release {
		delete(e.leases, f.Instance)
	}
	return nil
}

// checkCommit validates a commit before it starts, returning the encoded
// instance.
func checkCommit(ctx context.Context, f store.Fence, ops []store.Op) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("store: commit not started: %w", err)
	}
	if len(ops) == 0 {
		return "", fmt.Errorf("%w: commit with no operations", store.ErrInvalid)
	}
	for i, op := range ops {
		if err := op.Validate(); err != nil {
			return "", fmt.Errorf("operation %d: %w", i, err)
		}
	}
	return component("instance", f.Instance)
}

func (e *Engine) applyOp(t *tx, f store.Fence, inst string, own ownershipFile, i int, op store.Op) error {
	var err error
	switch op.Kind {
	case store.OpPutObject:
		err = putObject(t, i, op.PutObject)
	case store.OpDeleteObject:
		err = deleteObject(t, i, op.DeleteObject)
	case store.OpAppendEntry:
		err = appendEntry(t, f, i, op.AppendEntry)
	case store.OpPutCheckpoint:
		err = putCheckpoint(t, f, i, op.PutCheckpoint)
	case store.OpUpdateRun:
		err = e.updateRun(t, f, i, op.UpdateRun)
	case store.OpPutArtifact:
		err = putArtifact(t, op.PutArtifact)
	case store.OpRelease:
		err = t.putJSON(ownershipPath(inst), ownershipFile{Epoch: own.Epoch})
	}
	if err != nil {
		return fmt.Errorf("operation %d: %w", i, err)
	}
	return nil
}

// committedVersion reads a record's version from the backend, ignoring the
// commit's own pending writes: a rejected commit applies none of them, so a
// conflict must never report a version only the overlay holds.
func committedVersion(t *tx, path string) (uint64, error) {
	var v struct {
		Version uint64 `json:"version"`
	}
	if _, err := readJSON(t.b, path, &v); err != nil {
		return 0, err
	}
	return v.Version, nil
}

func conflict(i int, kind store.OpKind, current uint64, format string, args ...any) error {
	return &store.ConflictError{Op: i, Kind: kind, Current: current, Detail: fmt.Sprintf(format, args...)}
}

func objectPathFor(ns, key string) (string, error) {
	n, err := component("namespace", ns)
	if err != nil {
		return "", err
	}
	k, err := component("key", key)
	if err != nil {
		return "", err
	}
	return objectPath(n, k), nil
}

func putObject(t *tx, i int, p *store.PutObject) error {
	path, err := objectPathFor(p.Namespace, p.Key)
	if err != nil {
		return err
	}
	var cur objectFile
	if _, err := readJSON(t, path, &cur); err != nil {
		return err
	}
	if p.ExpectVersion != cur.Version {
		committed, err := committedVersion(t, path)
		if err != nil {
			return err
		}
		return conflict(i, store.OpPutObject, committed, "object %q/%q is at version %d in this commit, not %d",
			p.Namespace, p.Key, cur.Version, p.ExpectVersion)
	}
	return t.putJSON(path, objectFile{Version: cur.Version + 1, JSON: p.JSON, Bytes: p.Bytes})
}

func deleteObject(t *tx, i int, d *store.DeleteObject) error {
	path, err := objectPathFor(d.Namespace, d.Key)
	if err != nil {
		return err
	}
	var cur objectFile
	found, err := readJSON(t, path, &cur)
	if err != nil {
		return err
	}
	if !found || cur.Deleted || d.ExpectVersion != cur.Version {
		committed, err := committedVersion(t, path)
		if err != nil {
			return err
		}
		return conflict(i, store.OpDeleteObject, committed, "object %q/%q is at version %d in this commit (deleted or absent: %v), not %d",
			d.Namespace, d.Key, cur.Version, !found || cur.Deleted, d.ExpectVersion)
	}
	return t.putJSON(path, objectFile{Version: cur.Version + 1, Deleted: true})
}

// ownRun reads a run the fenced instance must own. A missing run, or one
// of another instance, is a caller defect.
func ownRun(r reader, f store.Fence, id string) (store.Run, string, error) {
	enc, err := component("run", id)
	if err != nil {
		return store.Run{}, "", err
	}
	var run store.Run
	found, err := readJSON(r, runPath(enc), &run)
	if err != nil {
		return store.Run{}, "", err
	}
	if !found {
		return store.Run{}, "", fmt.Errorf("%w: run %q does not exist", store.ErrInvalid, id)
	}
	if run.Instance != f.Instance {
		return store.Run{}, "", fmt.Errorf("%w: run %q belongs to instance %q, not the fenced %q",
			store.ErrInvalid, id, run.Instance, f.Instance)
	}
	return run, enc, nil
}

// checkNext enforces a sequence precondition: seq must not exist, and seq-1
// must (unless seq is 1).
func checkNext(t *tx, i int, kind store.OpKind, dir string, seq uint64) error {
	if seq == 0 {
		return fmt.Errorf("%w: %s sequence numbers start at 1", store.ErrInvalid, kind)
	}
	_, exists, err := t.Read(dir + "/" + seqName(seq))
	if err != nil {
		return err
	}
	if exists {
		return conflict(i, kind, 0, "sequence %d already exists", seq)
	}
	if seq == 1 {
		return nil
	}
	_, prev, err := t.Read(dir + "/" + seqName(seq-1))
	if err != nil {
		return err
	}
	if !prev {
		return conflict(i, kind, 0, "sequence %d is not next: %d does not exist", seq, seq-1)
	}
	return nil
}

func appendEntry(t *tx, f store.Fence, i int, en *store.Entry) error {
	if en.Kind == "" {
		return fmt.Errorf("%w: entry with no kind", store.ErrInvalid)
	}
	_, run, err := ownRun(t, f, en.Run)
	if err != nil {
		return err
	}
	if err := checkNext(t, i, store.OpAppendEntry, recordDir(run), en.Seq); err != nil {
		return err
	}
	return t.putJSON(recordDir(run)+"/"+seqName(en.Seq), en)
}

func putCheckpoint(t *tx, f store.Fence, i int, c *store.Checkpoint) error {
	if c.Instance != f.Instance {
		return fmt.Errorf("%w: checkpoint for instance %q under a fence for %q", store.ErrInvalid, c.Instance, f.Instance)
	}
	if c.Scope.Kind == "" || c.Scope.ID == "" {
		return fmt.Errorf("%w: checkpoint with no scope", store.ErrInvalid)
	}
	if c.Scope.Kind == store.ScopeRun {
		if _, _, err := ownRun(t, f, c.Scope.ID); err != nil {
			return err
		}
	}
	inst, err := component("instance", c.Instance)
	if err != nil {
		return err
	}
	if err := checkNext(t, i, store.OpPutCheckpoint, checkpointDir(inst), c.Seq); err != nil {
		return err
	}
	return t.putJSON(checkpointDir(inst)+"/"+seqName(c.Seq), c)
}

func (e *Engine) updateRun(t *tx, f store.Fence, i int, u *store.RunUpdate) error {
	run, enc, err := ownRun(t, f, u.Run)
	if err != nil {
		return err
	}
	if !u.Status.Valid() {
		return fmt.Errorf("%w: invalid status %q", store.ErrInvalid, u.Status)
	}
	if run.Status.Terminal() && !u.Status.Terminal() {
		return fmt.Errorf("%w: run %q is %s, which is final", store.ErrInvalid, u.Run, run.Status)
	}
	if u.ExpectVersion != run.Version {
		committed, err := committedVersion(t, runPath(enc))
		if err != nil {
			return err
		}
		return conflict(i, store.OpUpdateRun, committed, "run %q is at version %d in this commit, not %d",
			u.Run, run.Version, u.ExpectVersion)
	}
	run.Status = u.Status
	run.Reason = u.Reason
	if u.Consumed != nil {
		run.Consumed = u.Consumed
	}
	run.Version++
	run.UpdatedAt = e.now().UTC()
	return t.putJSON(runPath(enc), run)
}

func validDigest(d string) bool {
	if len(d) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(d); i++ {
		c := d[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func putArtifact(t *tx, a *store.Artifact) error {
	if !validDigest(a.Digest) {
		return fmt.Errorf("%w: artifact digest %q is not lowercase hex SHA-256", store.ErrInvalid, a.Digest)
	}
	sum := sha256.Sum256(a.Content)
	if hex.EncodeToString(sum[:]) != a.Digest {
		return fmt.Errorf("%w: artifact digest %s does not match its content", store.ErrInvalid, a.Digest)
	}
	if a.MediaType == "" {
		return fmt.Errorf("%w: artifact %s has no media type", store.ErrInvalid, a.Digest)
	}
	_, exists, err := t.Read(artifactMetaPath(a.Digest))
	if err != nil || exists {
		return err
	}
	t.set(Write{Path: artifactPath(a.Digest), Data: a.Content})
	return t.putJSON(artifactMetaPath(a.Digest), store.ArtifactMeta{
		Digest: a.Digest, MediaType: a.MediaType, Size: int64(len(a.Content)),
	})
}
