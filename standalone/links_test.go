package standalone

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/filestore"
)

// The standalone host migrates a file store written before artifact links
// existed on first open, from the runtime's own records (ADR 0024, item 9).
func TestOpenStoreMigratesLegacyArtifactLinks(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := filestore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.CreateRun(ctx, store.Run{ID: "r", Instance: "i", AgentType: "a", TriggerID: "t"}); err != nil {
		t.Fatal(err)
	}
	own, err := s.Claim(ctx, "i", "o", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`{"model":"m"}`)
	sum := sha256.Sum256(cfg)
	d := hex.EncodeToString(sum[:])
	started, _ := json.Marshal(agent.RunStarted{Config: d})
	if err = s.Commit(ctx, own.Fence(), []store.Op{
		store.PutArtifactOp(store.Artifact{Digest: d, MediaType: "application/json", Content: cfg}),
		store.AppendEntryOp(store.Entry{Run: "r", Seq: 1, Kind: agent.EntryRunStarted, Payload: started}),
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	for _, p := range []string{"format", filepath.Join("instances", "i", "artifacts")} {
		if err = os.RemoveAll(filepath.Join(dir, p)); err != nil {
			t.Fatal(err)
		}
	}
	s, err = openStore(&StoreConfig{Kind: StoreFile, Dir: dir})
	if err != nil {
		t.Fatalf("openStore(legacy) = %v", err)
	}
	defer s.Close()
	if _, got, err := s.Artifact(ctx, "i", d); err != nil || string(got) != string(cfg) {
		t.Fatalf("Artifact after migration = %q, %v", got, err)
	}
}
