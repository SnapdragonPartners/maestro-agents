package filestore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

const (
	inst = "inst-1"
	run  = "run-1"
	ns   = "shared"
)

var errInjected = errors.New("injected failure")

// failAt returns a failpoint that fails once, at the first point whose name
// starts with prefix.
func failAt(prefix string) func(string) error {
	fired := false
	return func(point string) error {
		if !fired && strings.HasPrefix(point, prefix) {
			fired = true
			return errInjected
		}
		return nil
	}
}

func openT(t *testing.T, dir string, fp func(string) error) store.Store {
	t.Helper()
	s, err := Open(dir, func(c *config) { c.failpoint = fp })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// prepare opens a store with a run and an owner, then closes it.
func prepare(t *testing.T) (string, store.Fence) {
	t.Helper()
	dir := t.TempDir()
	s := openT(t, dir, nil)
	ctx := context.Background()
	if _, _, err := s.CreateRun(ctx, store.Run{ID: run, Instance: inst, AgentType: "a", TriggerID: "t"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	own, err := s.Claim(ctx, inst, "owner", time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return dir, own.Fence()
}

// stepOps is a step boundary's commit: a result with its object write, the
// checkpoint and a run-record change.
func stepOps() []store.Op {
	return []store.Op{
		store.PutObjectJSON(ns, "submission", 0, json.RawMessage(`{"amount":1}`)),
		store.AppendEntryOp(store.Entry{Run: run, Seq: 1, Step: 1, Kind: "result"}),
		store.PutCheckpointOp(store.Checkpoint{Instance: inst, Seq: 1, Scope: store.Scope{Kind: store.ScopeRun, ID: run}, RecordSeq: 1, State: json.RawMessage(`{}`)}),
		store.UpdateRunOp(store.RunUpdate{Run: run, ExpectVersion: 1, Status: store.StatusSucceeded, Reason: store.Reason{Code: "changed"}}),
	}
}

// landed reports whether the step commit is visible, failing the test if
// it is partly visible.
func landed(t *testing.T, s store.Store) bool {
	t.Helper()
	ctx := context.Background()
	_, okObj, err := s.Object(ctx, ns, "submission")
	if err != nil {
		t.Fatalf("Object: %v", err)
	}
	es, err := s.Entries(ctx, run, 0)
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	_, okCP, err := s.LatestCheckpoint(ctx, inst)
	if err != nil {
		t.Fatalf("LatestCheckpoint: %v", err)
	}
	r, err := s.Run(ctx, run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	okRun := r.Status == store.StatusSucceeded
	okEntry := len(es) == 1
	if okObj != okEntry || okEntry != okCP || okCP != okRun {
		t.Fatalf("commit partly visible: object %v, entry %v, checkpoint %v, run %v", okObj, okEntry, okCP, okRun)
	}
	return okObj
}

func noTemps(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if names, _ := os.ReadDir(filepath.Join(dir, journalDir)); len(names) != 0 {
		t.Errorf("journal not empty: %d files", len(names))
	}
}

// A failure at each point of a multi-file commit: before the journal's
// rename nothing lands and the store stays usable; from the rename on the
// commit is indeterminate, the store refuses everything until reopened,
// and reopening finishes the commit.
func TestJournalFailurePoints(t *testing.T) {
	cases := []struct {
		point    string
		landed   bool
		poisoned bool
	}{
		{point: "before-rename:journal/", landed: false, poisoned: false},
		{point: "after-rename:journal/", landed: true, poisoned: true},
		{point: "before-rename:objects/", landed: true, poisoned: true},
		{point: "applied:objects/", landed: true, poisoned: true},
		{point: "after-rename:runs/run-1/run.json", landed: true, poisoned: true},
		{point: "journal-remove", landed: true, poisoned: true},
		{point: "after-remove:journal/", landed: true, poisoned: true},
	}
	for _, c := range cases {
		t.Run(c.point, func(t *testing.T) {
			dir, f := prepare(t)
			s := openT(t, dir, failAt(c.point))
			ctx := context.Background()
			err := s.Commit(ctx, f, stepOps())
			if !errors.Is(err, errInjected) {
				t.Fatalf("Commit = %v, want the injected failure", err)
			}
			if store.IsDefinite(err) {
				t.Fatalf("a failed write was classified as definite: %v", err)
			}
			_, rerr := s.Run(ctx, run)
			if c.poisoned != errors.Is(rerr, store.ErrUnusable) {
				t.Fatalf("read after failure = %v, want unusable %v", rerr, c.poisoned)
			}
			if c.poisoned {
				if _, err = s.Claim(ctx, "other", "o", time.Minute); !errors.Is(err, store.ErrUnusable) {
					t.Fatalf("claim on a poisoned store = %v", err)
				}
			} else if landed(t, s) {
				t.Fatal("commit landed although it failed before its commit point")
			}
			if err = s.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			s = openT(t, dir, nil)
			defer s.Close()
			if got := landed(t, s); got != c.landed {
				t.Fatalf("after reopen landed = %v, want %v", got, c.landed)
			}
			noTemps(t, dir)
			// The reopened store is fully usable: the epoch moves on.
			own, err := s.Claim(ctx, inst, "next", time.Minute)
			if err != nil || own.Epoch != f.Epoch+1 {
				t.Fatalf("claim after reopen = %+v, %v", own, err)
			}
		})
	}
}

// A one-file commit's rename is its commit point.
func TestSingleFileFailurePoints(t *testing.T) {
	for _, c := range []struct {
		point    string
		landed   bool
		poisoned bool
	}{
		{point: "before-rename:objects/", landed: false, poisoned: false},
		{point: "after-rename:objects/", landed: true, poisoned: true},
	} {
		t.Run(c.point, func(t *testing.T) {
			dir, f := prepare(t)
			s := openT(t, dir, failAt(c.point))
			ctx := context.Background()
			err := s.Commit(ctx, f, []store.Op{store.PutObjectJSON(ns, "k", 0, json.RawMessage(`1`))})
			if !errors.Is(err, errInjected) || store.IsDefinite(err) {
				t.Fatalf("Commit = %v", err)
			}
			_, _, rerr := s.Object(ctx, ns, "k")
			if c.poisoned != errors.Is(rerr, store.ErrUnusable) {
				t.Fatalf("read after failure = %v, want unusable %v", rerr, c.poisoned)
			}
			_ = s.Close()
			s = openT(t, dir, nil)
			defer s.Close()
			_, ok, err := s.Object(ctx, ns, "k")
			if err != nil || ok != c.landed {
				t.Fatalf("after reopen object present = %v, %v; want %v", ok, err, c.landed)
			}
			noTemps(t, dir)
		})
	}
}

// A process that died mid-commit leaves a journal and temporary files;
// opening the store finishes the journal and removes the temporaries. A
// temporary file left by an interrupted journal write is never applied.
func TestOpenFinishesJournalAndSweepsTemps(t *testing.T) {
	dir, f := prepare(t)
	// Capture a journal by failing just after its rename, then copy it
	// back after a clean reopen has finished it, as if the process had
	// died there.
	s := openT(t, dir, failAt("after-rename:journal/"))
	_ = s.Commit(context.Background(), f, stepOps())
	_ = s.Close()
	journals, _ := os.ReadDir(filepath.Join(dir, journalDir))
	if len(journals) != 1 {
		t.Fatalf("journals = %d, want 1", len(journals))
	}
	data, err := os.ReadFile(filepath.Join(dir, journalDir, journals[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	// A stray temporary journal holding a different commit must be ignored.
	stray := strings.Replace(string(data), "submission", "stray", 1)
	if err := os.WriteFile(filepath.Join(dir, journalDir, ".tmp-stray"), []byte(stray), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runs", ".tmp-x"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	s = openT(t, dir, nil)
	defer s.Close()
	if !landed(t, s) {
		t.Fatal("journal was not finished on open")
	}
	if _, ok, _ := s.Object(context.Background(), ns, "stray"); ok {
		t.Fatal("a temporary journal was applied")
	}
	noTemps(t, dir)
}

func TestOneProcessPerDirectory(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir, nil)
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("second Open = %v, want an error naming the directory", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := s.Run(context.Background(), run); !errors.Is(err, store.ErrUnusable) {
		t.Fatalf("read after Close = %v, want ErrUnusable", err)
	}
	s2 := openT(t, dir, nil)
	_ = s2.Close()
}

// Records are one JSON file each, at the paths ADR 0005 item 13 names, and
// readable by hand.
func TestLayout(t *testing.T) {
	dir, f := prepare(t)
	s := openT(t, dir, nil)
	defer s.Close()
	if err := s.Commit(context.Background(), f, stepOps()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	for _, p := range []string{
		"LOCK",
		"instances/inst-1/ownership.json",
		"instances/inst-1/checkpoints/00000000000000000001.json",
		"instances/inst-1/runs/t.json",
		"runs/run-1/run.json",
		"runs/run-1/record/00000000000000000001.json",
		"objects/shared/submission.json",
	} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p))); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "instances", "inst-1", "ownership.json"))
	if err != nil || string(data) != `{"owner":"owner","epoch":1}` {
		t.Fatalf("ownership file = %s, %v", data, err)
	}
	info, err := os.Stat(filepath.Join(dir, "runs", "run-1", "run.json"))
	if err != nil || info.Mode().Perm() != filePerm {
		t.Fatalf("run file mode = %v, %v; want %v", info.Mode().Perm(), err, os.FileMode(filePerm))
	}
}

// Open creates missing ancestors of the store directory.
func TestOpenCreatesNestedRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "store")
	s := openT(t, dir, nil)
	defer s.Close()
	if _, err := os.Stat(filepath.Join(dir, lockName)); err != nil {
		t.Fatalf("lock file in nested root: %v", err)
	}
	if err := createRoot(dir); err != nil {
		t.Fatalf("createRoot on an existing root: %v", err)
	}
}
