// Package filestore is the file reference store (STORE-4, ADR 0005 items
// 12 to 16): one JSON file per record, written by atomic rename, with a
// journal for commits that write more than one file. It needs no database
// driver.
//
// One process uses a store directory at a time: [Open] takes an exclusive,
// non-blocking lock and fails at once if another process (or another Open
// in this one) holds it. Network file systems are not supported.
//
// A failure after a commit point (a journal's rename, or a one-file
// commit's rename) leaves the store refusing every operation with
// [store.ErrUnusable] until it is closed and opened again; opening it
// finishes any interrupted commit before serving anything.
//
// The store does not encrypt; data at rest is protected by the host or the
// operating system (ADR 0005, item 18).
package filestore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/internal/storecore"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

const (
	lockName   = "LOCK"
	journalDir = "journal"
	dirPerm    = 0o700
	filePerm   = 0o600
)

type config struct {
	// failpoint, set only by this package's tests, injects a failure at a
	// named point of a write.
	failpoint func(point string) error
	core      storecore.Options
}

// Option configures a file store.
type Option func(*config)

// WithClock sets the store's clock, used for leases and timestamps. The
// default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(c *config) { c.core.Now = now }
}

// WithPauseHook sets the pause hook at TEST-6's two pause points (ADR 0005,
// item 6). Test-only: production code never sets it.
func WithPauseHook(h store.PauseHook) Option {
	return func(c *config) { c.core.Pause = h }
}

// errUnsupported refuses to open a file store where this package cannot
// take an exclusive lock, rather than open one unprotected.
var errUnsupported = errors.New("the file store's directory lock is not supported on this platform")

// Open opens the store in dir, creating the directory if needed. It takes
// the directory's lock, finishes every interrupted commit and removes
// leftover temporary files before returning. Every recorded owner is
// treated as having no lease, so every instance is claimable at once and
// its next claim increments the epoch (ADR 0005, item 16).
func Open(dir string, opts ...Option) (store.Store, error) {
	var c config
	for _, opt := range opts {
		opt(&c)
	}
	if err := createRoot(dir); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("filestore: open %s: %w", dir, err)
	}
	lock, err := root.OpenFile(lockName, os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("filestore: open lock in %s: %w", dir, err)
	}
	if err := lockFile(lock); err != nil {
		_ = lock.Close()
		_ = root.Close()
		if errors.Is(err, errUnsupported) {
			return nil, fmt.Errorf("filestore: cannot lock %s: %w", dir, err)
		}
		return nil, fmt.Errorf("filestore: store directory %s is in use by another process: %w", dir, err)
	}
	b := &backend{root: root, lock: lock, dirs: map[string]bool{".": true}, failpoint: c.failpoint}
	if err := b.recover(); err != nil {
		_ = b.Close()
		return nil, fmt.Errorf("filestore: recover %s: %w", dir, err)
	}
	return storecore.New(b, c.core), nil
}

// createRoot creates the store directory and any missing ancestors, then
// makes each new entry durable: every directory it created, and the store
// directory itself, is synced together with its parent before Open accepts
// a write, so an acknowledged commit cannot vanish with an unsynced root
// (ADR 0005, item 13).
func createRoot(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("filestore: resolve %s: %w", dir, err)
	}
	var missing []string
	for p := abs; ; p = filepath.Dir(p) {
		_, err := os.Stat(p)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("filestore: stat %s: %w", p, err)
		}
		missing = append(missing, p)
		if filepath.Dir(p) == p {
			break
		}
	}
	toSync := []string{abs, filepath.Dir(abs)}
	for i := len(missing) - 1; i >= 0; i-- {
		p := missing[i]
		if err := os.Mkdir(p, dirPerm); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("filestore: create %s: %w", p, err)
		}
		toSync = append(toSync, p, filepath.Dir(p))
	}
	for _, p := range toSync {
		if err := syncPath(p); err != nil {
			return err
		}
	}
	return nil
}

// syncPath syncs a directory outside the store root.
func syncPath(p string) error {
	f, err := os.Open(filepath.Clean(p))
	if err != nil {
		return fmt.Errorf("filestore: open directory %s: %w", p, err)
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("filestore: sync directory %s: %w", p, err)
	}
	return nil
}

// backend stores each record as a file under root. The engine serializes
// Apply against every other call.
type backend struct {
	root      *os.Root
	lock      *os.File
	failpoint func(string) error
	// dirs caches directories known to exist durably.
	dirs        map[string]bool
	journalNext uint64
}

func (b *backend) fail(point string) error {
	if b.failpoint == nil {
		return nil
	}
	return b.failpoint(point)
}

func local(p string) string { return filepath.FromSlash(p) }

func (b *backend) Read(p string) ([]byte, bool, error) {
	data, err := b.root.ReadFile(local(p))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("filestore: read %s: %w", p, err)
	}
	return data, true, nil
}

func (b *backend) List(dir string) ([]string, error) {
	entries, err := fs.ReadDir(b.root.FS(), dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("filestore: list %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), storecore.TempPrefix) {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

func (b *backend) Close() error {
	err := errors.Join(unlockFile(b.lock), b.lock.Close(), b.root.Close())
	if err != nil {
		return fmt.Errorf("filestore: close: %w", err)
	}
	return nil
}

// syncDir makes a directory's entries durable.
func (b *backend) syncDir(dir string) error {
	f, err := b.root.Open(local(dir))
	if err != nil {
		return fmt.Errorf("filestore: open directory %s: %w", dir, err)
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("filestore: sync directory %s: %w", dir, err)
	}
	return nil
}

// ensureDir creates a directory and any missing parents, syncing each new
// directory and its parent before anything is renamed into it (ADR 0005,
// item 13).
func (b *backend) ensureDir(dir string) error {
	if b.dirs[dir] {
		return nil
	}
	if parent := path.Dir(dir); parent != dir {
		if err := b.ensureDir(parent); err != nil {
			return err
		}
	}
	err := b.root.Mkdir(local(dir), dirPerm)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("filestore: create directory %s: %w", dir, err)
	}
	if err := b.syncDir(dir); err != nil {
		return err
	}
	if err := b.syncDir(path.Dir(dir)); err != nil {
		return err
	}
	b.dirs[dir] = true
	return nil
}

func tempName() (string, error) {
	var r [12]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", fmt.Errorf("filestore: temporary name: %w", err)
	}
	return storecore.TempPrefix + hex.EncodeToString(r[:]), nil
}

// writeFile replaces a file atomically: it writes and syncs a temporary
// file in the same directory, renames it over the target and syncs the
// directory. renamed reports whether the rename happened, after which a
// failure leaves the outcome indeterminate.
func (b *backend) writeFile(p string, data []byte) (renamed bool, err error) {
	dir := path.Dir(p)
	if err = b.ensureDir(dir); err != nil {
		return false, err
	}
	name, err := tempName()
	if err != nil {
		return false, err
	}
	tmp := path.Join(dir, name)
	f, err := b.root.OpenFile(local(tmp), os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm)
	if err != nil {
		return false, fmt.Errorf("filestore: create %s: %w", tmp, err)
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = b.fail("before-rename:" + p)
	}
	if err != nil {
		_ = b.root.Remove(local(tmp))
		return false, fmt.Errorf("filestore: write %s: %w", p, err)
	}
	if err := b.root.Rename(local(tmp), local(p)); err != nil {
		_ = b.root.Remove(local(tmp))
		return false, fmt.Errorf("filestore: rename %s: %w", p, err)
	}
	if err := b.fail("after-rename:" + p); err != nil {
		return true, err
	}
	return true, b.syncDir(dir)
}

// removeFile deletes a file; a file already absent is not an error.
// removed reports whether the deletion happened.
func (b *backend) removeFile(p string) (removed bool, err error) {
	err = b.root.Remove(local(p))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("filestore: remove %s: %w", p, err)
	}
	if err := b.fail("after-remove:" + p); err != nil {
		return true, err
	}
	return true, b.syncDir(path.Dir(p))
}

func (b *backend) applyWrite(w storecore.Write) (done bool, err error) {
	if w.Delete {
		return b.removeFile(w.Path)
	}
	return b.writeFile(w.Path, w.Data)
}

// journalFile is a commit in progress: every write's final contents.
type journalFile struct {
	Writes []journalWrite `json:"writes"`
}

type journalWrite struct {
	Path   string `json:"path"`
	Data   []byte `json:"data,omitempty"`
	Delete bool   `json:"delete,omitempty"`
}

func indeterminate(err error) error { return &storecore.IndeterminateError{Err: err} }

// Apply implements storecore.Backend (ADR 0005, item 14). A one-file
// commit's rename is its commit point; a larger commit's is the rename of
// its journal.
func (b *backend) Apply(writes []storecore.Write) error {
	if len(writes) == 1 {
		done, err := b.applyWrite(writes[0])
		if err != nil && done {
			return indeterminate(err)
		}
		return err
	}
	j := journalFile{Writes: make([]journalWrite, len(writes))}
	for i, w := range writes {
		j.Writes[i] = journalWrite{Path: w.Path, Data: w.Data, Delete: w.Delete}
	}
	data, err := json.Marshal(j)
	if err != nil {
		return fmt.Errorf("filestore: encode journal: %w", err)
	}
	b.journalNext++
	name := path.Join(journalDir, fmt.Sprintf("%020d.json", b.journalNext))
	renamed, err := b.writeFile(name, data)
	if err != nil {
		if renamed {
			return indeterminate(err)
		}
		return err
	}
	if err := b.finish(name, j); err != nil {
		return indeterminate(err)
	}
	return nil
}

// finish applies a journal's writes, deletes the journal and syncs its
// directory.
func (b *backend) finish(name string, j journalFile) error {
	for _, w := range j.Writes {
		if _, err := b.applyWrite(storecore.Write{Path: w.Path, Data: w.Data, Delete: w.Delete}); err != nil {
			return err
		}
		if err := b.fail("applied:" + w.Path); err != nil {
			return err
		}
	}
	if err := b.fail("journal-remove"); err != nil {
		return err
	}
	_, err := b.removeFile(name)
	return err
}

// recover finishes every journal, in order, then deletes every leftover
// temporary file (ADR 0005, item 15).
func (b *backend) recover() error {
	if err := b.ensureDir(journalDir); err != nil {
		return err
	}
	names, err := b.List(journalDir)
	if err != nil {
		return err
	}
	slices.Sort(names)
	for _, n := range names {
		name := path.Join(journalDir, n)
		data, _, err := b.Read(name)
		if err != nil {
			return err
		}
		var j journalFile
		if err := json.Unmarshal(data, &j); err != nil {
			return fmt.Errorf("filestore: corrupt journal %s: %w", name, err)
		}
		if err := b.finish(name, j); err != nil {
			return err
		}
	}
	return b.sweep()
}

// sweep deletes every temporary file in the store.
func (b *backend) sweep() error {
	var temps []string
	err := fs.WalkDir(b.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), storecore.TempPrefix) {
			temps = append(temps, p)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("filestore: scan for temporary files: %w", err)
	}
	for _, p := range temps {
		if _, err := b.removeFile(p); err != nil {
			return err
		}
	}
	return nil
}
