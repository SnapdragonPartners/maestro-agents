package standalone

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/filestore"
)

type probeData struct {
	Size int `json:"size"`
}

// probe is a code-only agent type whose single code state reads its input
// with FileTrigger.Read and acts on the content: "fail..." ends
// invalid_output, "block..." waits for its context to end (a shutdown),
// "slow..." sleeps a little, anything else succeeds. It records how many
// runs execute at once and which runs started.
type probe struct {
	started    chan string
	mu         sync.Mutex
	running    int
	maxRunning int
	readErrs   int
}

func newProbe() *probe { return &probe{started: make(chan string, 64)} }

const probeSchema = `{"type":"object","properties":{"greeting":{"type":"string"}},"additionalProperties":false}`

func (p *probe) typ() *agent.Type[probeData] {
	return &agent.Type[probeData]{
		Name:     "probe",
		Version:  "1",
		Triggers: []string{agent.TriggerOneShot, agent.TriggerFolder},
		Config:   json.RawMessage(probeSchema),
		Initial:  "read",
		States: []agent.State[probeData]{
			{Name: "read", Next: []string{"done", "rejected"}, Code: p.read},
			{Name: "done", End: agent.EndChanged},
			{Name: "rejected", End: agent.EndInvalidOutput},
		},
	}
}

func (p *probe) read(ctx context.Context, env *agent.Env, d probeData) (agent.Transition[probeData], error) {
	t, err := ParseFileTrigger(env.Trigger())
	if err != nil {
		return agent.Transition[probeData]{}, err
	}
	data, err := t.Read()
	if err != nil {
		p.mu.Lock()
		p.readErrs++
		p.mu.Unlock()
		return agent.Transition[probeData]{}, err
	}
	p.mu.Lock()
	p.running++
	p.maxRunning = max(p.maxRunning, p.running)
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.running--
		p.mu.Unlock()
	}()
	p.started <- env.RunID()
	d.Size = len(data)
	switch s := string(data); {
	case strings.HasPrefix(s, "fail"):
		return agent.Goto("rejected", d), nil
	case strings.HasPrefix(s, "block"):
		<-ctx.Done()
		return agent.Transition[probeData]{}, ctx.Err()
	case strings.HasPrefix(s, "slow"):
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return agent.Transition[probeData]{}, ctx.Err()
		}
	}
	return agent.Goto("done", d), nil
}

func probeProgram(t testing.TB, p *probe) Program {
	t.Helper()
	r := agent.NewRegistry()
	if err := r.Register(p.typ()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return Program{Name: "probe", Registry: r, getenv: func(string) string { return "" }}
}

// writeConfig writes a configuration to dir/config.json.
func writeConfig(t testing.TB, dir string, cfg map[string]any) string {
	t.Helper()
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFile(t testing.TB, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// oneShotConfig is a valid one-shot configuration on a file store in dir.
func oneShotConfig(dir string) map[string]any {
	return map[string]any{
		"agent_type": "probe",
		"namespace":  "intake",
		"config":     map[string]any{},
		"store":      map[string]any{"kind": "file", "dir": filepath.Join(dir, "state")},
		"trigger":    map[string]any{"kind": "one_shot"},
		"log":        map[string]any{"level": "error"},
	}
}

type runOutput struct {
	stdout, stderr bytes.Buffer
	code           int
}

func runProgram(ctx context.Context, p Program, args ...string) *runOutput { //nolint:gocritic // hugeParam: tests
	o := &runOutput{}
	o.code = Run(ctx, p, args, &o.stdout, &o.stderr)
	return o
}

func (o *runOutput) report(t testing.TB) report {
	t.Helper()
	var r report
	if err := json.Unmarshal(o.stdout.Bytes(), &r); err != nil {
		t.Fatalf("report %q: %v (stderr %s)", o.stdout.String(), err, o.stderr.String())
	}
	return r
}

// openState reopens a file store after the program has exited.
func openState(t testing.TB, dir string) store.Store {
	t.Helper()
	s, err := filestore.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatalf("filestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// waitFor waits for n run IDs from started.
func waitFor(t testing.TB, started <-chan string, n int) []string {
	t.Helper()
	var ids []string
	deadline := time.After(10 * time.Second)
	for len(ids) < n {
		select {
		case id := <-started:
			ids = append(ids, id)
		case <-deadline:
			t.Fatalf("waited for %d runs, saw %d: %v", n, len(ids), ids)
		}
	}
	return ids
}

// fakeProvider returns a Provider that records the credential it was
// given and builds a scripted client.
func fakeProvider(got *string, script ...llms.ChatResponse) Provider {
	return func(_ ModelRoute, key agent.Secret) (llms.ChatClient, error) {
		*got = key.Reveal()
		return scriptedClient(script), nil
	}
}
