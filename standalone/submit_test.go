package standalone

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// note is a submitted, code-only agent type: its state puts the payload's
// text at notes/<instance>/<run> and succeeds; "block" waits for its
// context to end (a shutdown), "fail" ends invalid_output.
type note struct {
	mu   sync.Mutex
	runs []string
	// gate, when set, is waited on before the state commits.
	gate chan struct{}
}

type noteData struct{}

func (n *note) typ() *agent.Type[noteData] {
	return &agent.Type[noteData]{
		Name:     "note",
		Version:  "1",
		Triggers: []string{SubmittedKind},
		Initial:  "write",
		States: []agent.State[noteData]{
			{Name: "write", Next: []string{"done", "rejected"}, Code: n.write},
			{Name: "done", End: agent.EndChanged},
			{Name: "rejected", End: agent.EndInvalidOutput},
		},
	}
}

func (n *note) write(ctx context.Context, env *agent.Env, d noteData) (agent.Transition[noteData], error) {
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(env.Trigger(), &p); err != nil {
		return agent.Transition[noteData]{}, err
	}
	n.mu.Lock()
	n.runs = append(n.runs, env.RunID())
	gate := n.gate
	n.mu.Unlock()
	switch p.Text {
	case "block":
		<-ctx.Done()
		return agent.Transition[noteData]{}, ctx.Err()
	case "fail":
		return agent.Goto("rejected", d), nil
	}
	if gate != nil {
		<-gate
	}
	b, _ := json.Marshal(p.Text)
	t := agent.Goto("done", d)
	t.Ops = []store.Op{env.PutJSON("notes/"+env.RunID(), 0, b)}
	return t, nil
}

func noteProgram(t testing.TB, pr *probe, n *note) Program {
	t.Helper()
	p := probeProgram(t, pr)
	if err := p.Registry.Register(n.typ()); err != nil {
		t.Fatal(err)
	}
	p.Submitted = []string{"note"}
	return p
}

func openNoteHost(t *testing.T, n *note) *Host {
	t.Helper()
	dir := t.TempDir()
	cfg := oneShotConfig(dir)
	cfg["store"] = map[string]any{"kind": "memory"}
	h, err := Open(context.Background(), noteProgram(t, newProbe(), n), writeConfig(t, dir, cfg), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func payload(text string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"text": text})
	return b
}

// TestSubmitRunsASubmittedType: each submission is its own run on the
// instance, ID <instance>.<hex>, of the submitted type, with the payload as
// its trigger, the absent configuration and the type's default budgets
// (ADR 0021, items 1 and 2).
func TestSubmitRunsASubmittedType(t *testing.T) {
	n := &note{}
	h := openNoteHost(t, n)
	ctx := context.Background()
	var ids []string
	for _, text := range []string{"one", "two"} {
		rep, err := h.Submit(ctx, Submission{AgentType: "note", Instance: "sha256-abc", Payload: payload(text)})
		if err != nil {
			t.Fatal(err)
		}
		if !rep.Activated || rep.Status != store.StatusSucceeded || !strings.HasPrefix(rep.Run, "sha256-abc.") || len(rep.Run) != len("sha256-abc.")+32 {
			t.Fatalf("report %+v", rep)
		}
		run, err := h.Store().Run(ctx, rep.Run)
		if err != nil {
			t.Fatal(err)
		}
		if run.Instance != "sha256-abc" || run.AgentType != "note" || run.TriggerKind != SubmittedKind || !strings.HasPrefix(run.TriggerID, "submitted:") {
			t.Fatalf("run %+v", run)
		}
		obj, ok, err := h.Store().Object(ctx, h.Namespace(), "notes/"+rep.Run)
		if err != nil || !ok || string(obj.JSON) != `"`+text+`"` {
			t.Fatalf("note %s: %s %v %v", rep.Run, obj.JSON, ok, err)
		}
		ids = append(ids, rep.Run)
	}
	if ids[0] == ids[1] {
		t.Fatal("two submissions share a run")
	}
	trig, err := h.h.st.Triggers(ctx, "sha256-abc")
	if err != nil || len(trig) != 0 {
		t.Fatalf("triggers left queued: %v %v", trig, err)
	}
	// A failed run is a report, not an error.
	rep, err := h.Submit(ctx, Submission{AgentType: "note", Instance: "sha256-abc", Payload: payload("fail")})
	if err != nil || rep.Status != store.StatusFailed {
		t.Fatalf("failed run: %+v %v", rep, err)
	}
}

func TestSubmitRefuses(t *testing.T) {
	h := openNoteHost(t, &note{})
	ctx := context.Background()
	for name, s := range map[string]Submission{
		"configured type":   {AgentType: "probe", Instance: "i", Payload: payload("x")},
		"unregistered type": {AgentType: "other", Instance: "i", Payload: payload("x")},
		"empty instance":    {AgentType: "note", Payload: payload("x")},
		"slash in instance": {AgentType: "note", Instance: "a/b", Payload: payload("x")},
		"space in instance": {AgentType: "note", Instance: "a b", Payload: payload("x")},
		"long instance":     {AgentType: "note", Instance: strings.Repeat("a", maxInstanceID+1), Payload: payload("x")},
		// Codex round 1: the stores encode 'A' and '.' in three bytes; the
		// derived run ID must fit a path component.
		"run ID too long encoded": {AgentType: "note", Instance: strings.Repeat("A", 69), Payload: payload("x")},
		"payload not JSON":        {AgentType: "note", Instance: "i", Payload: json.RawMessage("{")},
		"payload is missing":      {AgentType: "note", Instance: "i"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := h.Submit(ctx, s); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
	if _, err := h.Submit(ctx, Submission{AgentType: "note", Instance: strings.Repeat("a", maxInstanceID), Payload: payload("x")}); err != nil {
		t.Fatalf("a %d-byte instance: %v", maxInstanceID, err)
	}
	// Nothing was enqueued for a refused instance.
	if trig, err := h.h.st.Triggers(ctx, strings.Repeat("A", 69)); err == nil && len(trig) != 0 {
		t.Fatalf("triggers %v", trig)
	}
	// The longest instance that encodes within the limit is accepted.
	if _, err := h.Submit(ctx, Submission{AgentType: "note", Instance: strings.Repeat("A", 68), Payload: payload("x")}); err != nil {
		t.Fatalf("68 uppercase letters: %v", err)
	}
}

// TestSubmitBusy: an instance whose run another owner holds is ErrBusy,
// and the refused submission leaves no trigger queued (ADR 0021, item 3).
func TestSubmitBusy(t *testing.T) {
	h := openNoteHost(t, &note{})
	ctx := context.Background()
	st := h.h.st
	if _, _, err := st.CreateRun(ctx, store.Run{ID: "held", Instance: "inst", AgentType: "note", TriggerID: "t", TriggerKind: SubmittedKind, Trigger: payload("x")}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Claim(ctx, "inst", "another-host", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Submit(ctx, Submission{AgentType: "note", Instance: "inst", Payload: payload("x")}); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if trig, err := st.Triggers(ctx, "inst"); err != nil || len(trig) != 0 {
		t.Fatalf("triggers %v %v", trig, err)
	}
	// Another agent type's unfinished run is busy too: this host leaves it
	// alone.
	if _, _, err := st.CreateRun(ctx, store.Run{ID: "foreign", Instance: "inst2", AgentType: "other", TriggerID: "t", TriggerKind: "k"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Submit(ctx, Submission{AgentType: "note", Instance: "inst2", Payload: payload("x")}); !errors.Is(err, ErrBusy) {
		t.Fatalf("foreign run: err = %v, want ErrBusy", err)
	}
}

// TestSubmitDrainsALeftoverRun: a runnable run left on the instance (a
// crash or a shutdown) is finished before the submission's run is made
// (ADR 0021, item 3).
func TestSubmitDrainsALeftoverRun(t *testing.T) {
	n := &note{}
	h := openNoteHost(t, n)
	ctx := context.Background()
	if _, _, err := h.h.st.CreateRun(ctx, store.Run{ID: "left.1", Instance: "inst", AgentType: "note", TriggerID: "submitted:1", TriggerKind: SubmittedKind, Trigger: payload("left")}); err != nil {
		t.Fatal(err)
	}
	rep, err := h.Submit(ctx, Submission{AgentType: "note", Instance: "inst", Payload: payload("new")})
	if err != nil || rep.Status != store.StatusSucceeded {
		t.Fatalf("%+v %v", rep, err)
	}
	if left, err := h.Store().Run(ctx, "left.1"); err != nil || left.Status != store.StatusSucceeded {
		t.Fatalf("leftover %+v %v", left, err)
	}
	if len(n.runs) != 2 || n.runs[0] != "left.1" {
		t.Fatalf("runs %v", n.runs)
	}
}

// TestSubmitCancelled: a submission shut down mid-run leaves its run
// runnable, and the next submission on the instance finishes it first.
func TestSubmitCancelled(t *testing.T) {
	n := &note{}
	h := openNoteHost(t, n)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Report)
	go func() {
		rep, _ := h.Submit(ctx, Submission{AgentType: "note", Instance: "inst", Payload: payload("block")})
		done <- rep
	}()
	waitRuns(t, n, 1)
	cancel()
	rep := <-done
	if rep.Status != store.StatusRunnable {
		t.Fatalf("cancelled run %+v", rep)
	}
	// The blocked run would block again; the drain sees it unfinished and
	// reports the instance busy rather than stacking a run behind it.
	dctx, dcancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer dcancel()
	if _, err := h.Submit(dctx, Submission{AgentType: "note", Instance: "inst", Payload: payload("x")}); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
}

// TestSubmitSerializesAnInstance: concurrent submissions on one instance
// run one at a time, each to its end; on different instances they overlap.
func TestSubmitSerializesAnInstance(t *testing.T) {
	n := &note{gate: make(chan struct{})}
	h := openNoteHost(t, n)
	ctx := context.Background()
	errs := make(chan error, 3)
	for _, inst := range []string{"a", "a", "b"} {
		go func() {
			rep, err := h.Submit(ctx, Submission{AgentType: "note", Instance: inst, Payload: payload("x")})
			if err == nil && rep.Status != store.StatusSucceeded {
				err = errors.New("status " + string(rep.Status))
			}
			errs <- err
		}()
	}
	waitRuns(t, n, 2) // one on a, one on b; the second on a waits
	time.Sleep(50 * time.Millisecond)
	if got := len(snapshot(n)); got != 2 {
		t.Fatalf("%d runs started while one instance was busy", got)
	}
	close(n.gate)
	for range 3 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

// TestWatchActivatesSubmittedRuns: the scheduler finishes a claimable run
// of a submitted type, as after a restart (ADR 0021, item 4), and leaves
// an unknown type's alone.
func TestWatchActivatesSubmittedRuns(t *testing.T) {
	n := &note{}
	pr := newProbe()
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	path := writeConfigFile(t, dir, folderConfig(dir, inbox, 1))
	st := openState(t, dir)
	ctx := context.Background()
	for _, r := range []store.Run{
		{ID: "inst.1", Instance: "inst", AgentType: "note", TriggerID: "submitted:1", TriggerKind: SubmittedKind, Trigger: payload("left")},
		{ID: "x.1", Instance: "x", AgentType: "unknown", TriggerID: "t", TriggerKind: "k"},
	} {
		if _, _, err := st.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan *runOutput)
	go func() { done <- runProgram(rctx, noteProgram(t, pr, n), "run", "-config", path) }()
	waitRuns(t, n, 1)
	cancel()
	if out := <-done; out.code != ExitComplete {
		t.Fatalf("exit %d: %s", out.code, out.stderr.String())
	}
	st = openState(t, dir)
	defer st.Close() //nolint:errcheck // test
	if r, err := st.Run(ctx, "inst.1"); err != nil || r.Status != store.StatusSucceeded {
		t.Fatalf("submitted run %+v %v", r, err)
	}
	if r, err := st.Run(ctx, "x.1"); err != nil || r.Status != store.StatusRunnable {
		t.Fatalf("unknown type's run %+v %v", r, err)
	}
}

// TestValidateSubmitted: the program's submitted types are checked with
// the configuration (ADR 0021, item 1).
func TestValidateSubmitted(t *testing.T) {
	withConfig := &agent.Type[noteData]{
		Name: "configured", Version: "1", Triggers: []string{SubmittedKind}, Initial: "s",
		Config: json.RawMessage(`{"type":"object"}`),
		States: []agent.State[noteData]{
			{Name: "s", Next: []string{"e"}, Code: func(context.Context, *agent.Env, noteData) (agent.Transition[noteData], error) {
				return agent.Goto("e", noteData{}), nil
			}},
			{Name: "e", End: agent.EndChanged},
		},
	}
	noKind := &agent.Type[noteData]{
		Name: "nokind", Version: "1", Triggers: []string{"other"}, Initial: "s",
		States: []agent.State[noteData]{
			{Name: "s", Next: []string{"e"}, Code: func(context.Context, *agent.Env, noteData) (agent.Transition[noteData], error) {
				return agent.Goto("e", noteData{}), nil
			}},
			{Name: "e", End: agent.EndChanged},
		},
	}
	for name, tc := range map[string]struct {
		submitted []string
		want      string
	}{
		"unregistered":    {[]string{"missing"}, `"missing" is not registered`},
		"twice":           {[]string{"note", "note"}, `"note" is listed twice`},
		"configured type": {[]string{"probe"}, `"probe" is the configured agent_type`},
		"no kind":         {[]string{"nokind"}, `does not declare the trigger kind "submitted"`},
		"takes a config":  {[]string{"configured"}, `"configured" must take no configuration`},
		// Codex round 1: a submitted model state needs the model route as
		// the configured type's would.
		"model state, no model": {[]string{"submodel"}, `agent type "submodel" has a model state ("think"), so model is required`},
	} {
		t.Run(name, func(t *testing.T) {
			p := noteProgram(t, newProbe(), &note{})
			for _, typ := range []*agent.Type[noteData]{withConfig, noKind} {
				if err := p.Registry.Register(typ); err != nil {
					t.Fatal(err)
				}
			}
			sub := modelType()
			sub.Name, sub.Triggers = "submodel", []string{SubmittedKind}
			if err := p.Registry.Register(sub); err != nil {
				t.Fatal(err)
			}
			p.Submitted = tc.submitted
			dir := t.TempDir()
			path := writeConfigFile(t, dir, oneShotConfig(dir))
			out := runProgram(context.Background(), p, "config", "validate", "-config", path)
			if out.code != ExitUsage || !strings.Contains(out.stderr.String(), tc.want) {
				t.Fatalf("exit %d, stderr %q, want %q", out.code, out.stderr.String(), tc.want)
			}
		})
	}
}

func snapshot(n *note) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.runs...)
}

func waitRuns(t *testing.T, n *note, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(snapshot(n)) < want {
		if time.Now().After(deadline) {
			t.Fatalf("%d runs started, want %d", len(snapshot(n)), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// blindStore hides every instance's active run, as when another process
// creates one between Submit's drain and its CreateRun.
type blindStore struct{ store.Store }

func (blindStore) ActiveRun(context.Context, string) (store.Run, bool, error) {
	return store.Run{}, false, nil
}

// TestSubmitBusyAtCreate: a run that appears between the drain and the
// create is ErrBusy, and the submission's trigger does not linger.
func TestSubmitBusyAtCreate(t *testing.T) {
	h := openNoteHost(t, &note{})
	ctx := context.Background()
	if _, _, err := h.h.st.CreateRun(ctx, store.Run{ID: "other", Instance: "inst", AgentType: "note", TriggerID: "t", TriggerKind: SubmittedKind, Trigger: payload("x")}); err != nil {
		t.Fatal(err)
	}
	h.h.st = blindStore{h.h.st}
	if _, err := h.Submit(ctx, Submission{AgentType: "note", Instance: "inst", Payload: payload("x")}); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if trig, err := h.h.st.Triggers(ctx, "inst"); err != nil || len(trig) != 0 {
		t.Fatalf("triggers %v %v", trig, err)
	}
}
