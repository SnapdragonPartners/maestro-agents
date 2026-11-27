package filestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

const (
	instA = "inst-a"
	instB = "inst-b"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// legacyStore is a store in the layout of a release before artifact links
// (ADR 0024, item 9): instance A stored dA and dOrphan, instance B stored
// dB, and no links or marker exist. dOrphan is the shape storetest's
// durability case writes: an artifact no record names.
type legacyStore struct {
	dir                string
	dA, dB, dOrphan    string
	refs               map[string][]string
	unstoredReferenced string
}

func newLegacyStore(t *testing.T) *legacyStore {
	t.Helper()
	l := &legacyStore{dir: t.TempDir()}
	s := openT(t, l.dir, nil)
	ctx := context.Background()
	put := func(instance string, content string) string {
		t.Helper()
		if _, _, err := s.CreateRun(ctx, store.Run{ID: "run-" + instance + "-" + content, Instance: instance, AgentType: "a", TriggerID: content}); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		own, err := s.Claim(ctx, instance, "owner", time.Minute)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		d := sum([]byte(content))
		if err := s.Commit(ctx, own.Fence(), []store.Op{
			store.PutArtifactOp(store.Artifact{Digest: d, MediaType: "text/plain", Content: []byte(content)}),
			store.UpdateRunOp(store.RunUpdate{Run: "run-" + instance + "-" + content, ExpectVersion: 1, Status: store.StatusSucceeded}),
			store.ReleaseOp(),
		}); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		return d
	}
	l.dA = put(instA, "a")
	l.dOrphan = put(instA, "orphan")
	l.dB = put(instB, "b")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"format", "instances/" + instA + "/artifacts", "instances/" + instB + "/artifacts"} {
		if err := os.RemoveAll(filepath.Join(l.dir, filepath.FromSlash(p))); err != nil {
			t.Fatal(err)
		}
	}
	l.unstoredReferenced = sum([]byte("never stored"))
	l.refs = map[string][]string{instA: {l.dA}, instB: {l.dB, l.unstoredReferenced}}
	return l
}

func (l *legacyStore) references(_ context.Context, _ store.Reader, instance string) ([]string, error) {
	return l.refs[instance], nil
}

func (l *legacyStore) open(opts ...Option) (store.Store, error) { return Open(l.dir, opts...) }

// wantNothingWritten checks a refused migration left the legacy layout.
func (l *legacyStore) wantNothingWritten(t *testing.T) {
	t.Helper()
	for _, p := range []string{"format/artifact-links.json", "instances/" + instA + "/artifacts", "instances/" + instB + "/artifacts"} {
		if _, err := os.Stat(filepath.Join(l.dir, filepath.FromSlash(p))); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s after a refused migration: %v, want absent", p, err)
		}
	}
}

func wantReads(t *testing.T, s store.Store, instance string, readable, hidden []string) {
	t.Helper()
	ctx := context.Background()
	for _, d := range readable {
		if _, _, err := s.Artifact(ctx, instance, d); err != nil {
			t.Fatalf("Artifact(%s, %s) = %v, want readable", instance, d, err)
		}
	}
	for _, d := range hidden {
		if _, _, err := s.Artifact(ctx, instance, d); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Artifact(%s, %s) = %v, want ErrNotFound", instance, d, err)
		}
	}
}

func TestLegacyStoreRefusedWithoutMigration(t *testing.T) {
	l := newLegacyStore(t)
	if _, err := l.open(); !errors.Is(err, ErrMigrationRequired) {
		t.Fatalf("Open = %v, want ErrMigrationRequired", err)
	}
	l.wantNothingWritten(t)
	// The refused open released the directory's lock.
	if _, err := l.open(); !errors.Is(err, ErrMigrationRequired) {
		t.Fatalf("second Open = %v, want ErrMigrationRequired again", err)
	}
}

// Stated owners add to the references; they are no substitute for them,
// even when they name every stored artifact.
func TestLegacyMigrationOwnersAloneRefused(t *testing.T) {
	l := newLegacyStore(t)
	_, err := l.open(WithArtifactOwners(map[string][]string{l.dA: {instA}, l.dOrphan: {instA}, l.dB: {instB}}))
	if !errors.Is(err, ErrMigrationRequired) || !strings.Contains(err.Error(), "WithLegacyArtifactLinks") {
		t.Fatalf("Open = %v, want ErrMigrationRequired naming WithLegacyArtifactLinks", err)
	}
	l.wantNothingWritten(t)
}

func TestLegacyMigrationRefusesUnclaimedArtifacts(t *testing.T) {
	l := newLegacyStore(t)
	_, err := l.open(WithLegacyArtifactLinks(l.references))
	var unclaimed *UnclaimedArtifactsError
	if !errors.As(err, &unclaimed) || !errors.Is(err, ErrMigrationRequired) || !slices.Equal(unclaimed.Digests, []string{l.dOrphan}) {
		t.Fatalf("Open = %v, want unclaimed [%s]", err, l.dOrphan)
	}
	l.wantNothingWritten(t)
}

func TestLegacyMigrationLinksByReferenceAndStatedOwner(t *testing.T) {
	l := newLegacyStore(t)
	s, err := l.open(WithLegacyArtifactLinks(l.references), WithArtifactOwners(map[string][]string{l.dOrphan: {instA}}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	wantReads(t, s, instA, []string{l.dA, l.dOrphan}, []string{l.dB, l.unstoredReferenced})
	wantReads(t, s, instB, []string{l.dB}, []string{l.dA, l.dOrphan, l.unstoredReferenced})
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	// Migrated once: a reopen neither needs nor runs the migration.
	s, err = l.open(WithLegacyArtifactLinks(func(context.Context, store.Reader, string) ([]string, error) {
		t.Fatal("migration ran again on a migrated store")
		return nil, nil
	}))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	wantReads(t, s, instB, []string{l.dB}, []string{l.dA})
	_ = s.Close()
	s, err = l.open()
	if err != nil {
		t.Fatalf("reopen without the option: %v", err)
	}
	_ = s.Close()
}

func TestLegacyMigrationRefusesBadOwners(t *testing.T) {
	cases := map[string]func(l *legacyStore) map[string][]string{
		"unknown instance": func(l *legacyStore) map[string][]string { return map[string][]string{l.dOrphan: {"inst-z"}} },
		"unstored digest":  func(l *legacyStore) map[string][]string { return map[string][]string{l.unstoredReferenced: {instA}} },
	}
	for name, owners := range cases {
		t.Run(name, func(t *testing.T) {
			l := newLegacyStore(t)
			if _, err := l.open(WithLegacyArtifactLinks(l.references), WithArtifactOwners(owners(l))); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("Open = %v, want ErrInvalid", err)
			}
			l.wantNothingWritten(t)
		})
	}
}

func TestLegacyMigrationReferenceError(t *testing.T) {
	l := newLegacyStore(t)
	boom := errors.New("undecodable record")
	_, err := l.open(WithLegacyArtifactLinks(func(context.Context, store.Reader, string) ([]string, error) { return nil, boom }))
	if !errors.Is(err, boom) {
		t.Fatalf("Open = %v, want the references error", err)
	}
	l.wantNothingWritten(t)
}

// A failure while the migration's batch is written, before or after its
// commit point, leaves a store whose next open ends migrated, with every
// link: the batch is one atomic apply.
func TestLegacyMigrationInterrupted(t *testing.T) {
	for _, point := range []string{"before-rename:", "applied:", "journal-remove"} {
		t.Run(point, func(t *testing.T) {
			l := newLegacyStore(t)
			opts := []Option{WithLegacyArtifactLinks(l.references), WithArtifactOwners(map[string][]string{l.dOrphan: {instA}})}
			fp := failAt(point)
			if _, err := l.open(append(opts, func(c *config) { c.failpoint = fp })...); !errors.Is(err, errInjected) {
				t.Fatalf("Open = %v, want the injected failure", err)
			}
			s, err := l.open(opts...)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer s.Close()
			wantReads(t, s, instA, []string{l.dA, l.dOrphan}, []string{l.dB})
			wantReads(t, s, instB, []string{l.dB}, []string{l.dA, l.dOrphan})
		})
	}
}

func TestNewStoreNeedsNoMigration(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir, nil)
	if _, err := os.Stat(filepath.Join(dir, "format", "artifact-links.json")); err != nil {
		t.Fatalf("marker of a new store: %v", err)
	}
	_ = s.Close()
	dir, f := prepare(t)
	s = openT(t, dir, nil)
	content := []byte("x")
	if err := s.Commit(context.Background(), f, []store.Op{store.PutArtifactOp(store.Artifact{Digest: sum(content), MediaType: "text/plain", Content: content})}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s = openT(t, dir, nil)
	defer s.Close()
	wantReads(t, s, f.Instance, []string{sum(content)}, nil)
}
