//go:build integration

package intake_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/intake"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// TestLocalSamples is the slice's step 1 and 2 on representative
// statements: each real sample (client data, never committed; ADR 0015) is
// loaded by the agent through its layout-mode text, in the file store, and
// the scripted model submits the sample's hand-written record, which is
// verified and stored. The two copy-restricted samples are read under the
// implicit acceptance (#17). The test skips when .local/addepar is absent.
// Failures name the case only, never an amount.
func TestLocalSamples(t *testing.T) {
	needPoppler(t)
	dir := os.Getenv("ADDEPAR_LOCAL_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", "..", "..", ".local", "addepar")
	}
	files, err := filepath.Glob(filepath.Join(dir, "cases", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skipf("no local cases in %s", dir)
	}
	reg := registry(t, intake.Options{})
	for _, f := range files {
		t.Run(strings.TrimSuffix(filepath.Base(f), ".json"), func(t *testing.T) {
			c := readCase(t, f)
			var answer struct {
				Answer string `json:"answer"`
			}
			b, err := os.ReadFile(f) //nolint:gosec // local test data
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, &answer); err != nil {
				t.Fatal(err)
			}
			clk := &clock{now: fixedNow}
			s := openFile(t, t.TempDir(), clk)
			defer s.Close()
			sha := createRun(t, s, filepath.Join(dir, "statements", c.Statement))
			sc := &scripted{script: []llms.ChatResponse{getDocument(), submit("2", c.Record)}}
			out, err := activate(t, s, s, reg, sha, ownerA, sc, nil, agent.Budgets{})
			if err != nil || out.Status != store.StatusSucceeded {
				t.Fatalf("activation = %s/%s, %v", out.Status, out.Reason.Code, err)
			}
			o, ok := outcome(t, s, sha)
			if !ok || o.Disposition != intake.DispositionVerified {
				t.Fatalf("disposition %q, want verified (attempt verdicts: %v)", o.Disposition, verdicts(attempts(t, s, sha)))
			}
			recs := records(t, s, sha)
			if len(recs) != 1 || recs[0].Record.Ending.Amount.String() != answer.Answer {
				t.Fatalf("%d records; the ending does not match the answer key", len(recs))
			}
		})
	}
}

func verdicts(atts []intake.Attempt) []string {
	out := make([]string, len(atts))
	for i, a := range atts {
		out[i] = string(a.Verdict)
	}
	return out
}
