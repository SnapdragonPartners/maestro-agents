package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/document"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/intake"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/synthpdf"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
	"github.com/SnapdragonPartners/maestro-agents/standalone"
	"github.com/SnapdragonPartners/maestro-agents/store/filestore"
)

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"export"}, {"verify"}, {"verify", "-pdf", "x"}, {"verify", "-bogus"}} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), args, &out, &errb); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
		if hostCommand(args) {
			t.Errorf("%v: routed to the standalone host", args)
		}
	}
	// With no command, or an unknown one, the usage lists every command.
	for _, args := range [][]string{nil, {"export"}} {
		var errb bytes.Buffer
		run(context.Background(), args, io.Discard, &errb)
		for _, cmd := range []string{"addepar run ", "addepar config validate ", "addepar verify "} {
			if !strings.Contains(errb.String(), cmd) {
				t.Errorf("%v: usage %q does not list %q", args, errb.String(), cmd)
			}
		}
	}
	for _, args := range [][]string{{"run"}, {"config", "validate"}, {"config"}} {
		if !hostCommand(args) {
			t.Errorf("%v: not routed to the standalone host", args)
		}
	}
}

func TestVerifyCommand(t *testing.T) {
	for _, bin := range []string{"pdftotext", "pdfinfo"} {
		if _, err := exec.LookPath(bin); err != nil {
			if os.Getenv("ADDEPAR_REQUIRE_POPPLER") != "" {
				t.Fatalf("%s not installed (poppler-utils), and ADDEPAR_REQUIRE_POPPLER is set", bin)
			}
			t.Skipf("%s not installed", bin)
		}
	}
	dir := t.TempDir()
	synth := filepath.Join("internal", "verify", "testdata", "synthetic")
	text, err := os.ReadFile(filepath.Join(synth, "brightwater-iv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	pdf, err := synthpdf.Build([]string{string(text)}, synthpdf.Options{CopyRestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	pdfPath := filepath.Join(dir, "s.pdf")
	if err = os.WriteFile(pdfPath, pdf, 0o600); err != nil {
		t.Fatal(err)
	}
	var c struct {
		Record json.RawMessage `json:"record"`
	}
	cb, err := os.ReadFile(filepath.Join(synth, "brightwater-iv.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(cb, &c); err != nil {
		t.Fatal(err)
	}
	recPath := filepath.Join(dir, "r.json")
	if err = os.WriteFile(recPath, c.Record, 0o600); err != nil {
		t.Fatal(err)
	}

	verdict := func(args ...string) (int, verify.Result) {
		t.Helper()
		var out, errb bytes.Buffer
		code := run(context.Background(), append([]string{"verify", "-pdf", pdfPath, "-record", recPath}, args...), &out, &errb)
		var res verify.Result
		if code != exitUsage {
			if err := json.Unmarshal(out.Bytes(), &res); err != nil {
				t.Fatalf("output: %v\n%s", err, out.String())
			}
		}
		return code, res
	}
	// Declined (no override): the restricted text is not read.
	code, res := verdict()
	if code != exitFail || res.Verdict != verify.Fail || res.Checks[0].Findings[0].Code != "text_not_read" {
		t.Fatalf("without override: exit %d, %+v", code, res)
	}
	// Accepted: verified, with the currency warning left for a person.
	code, res = verdict("-override-by", "operator one", "-override-reason", "our own statement")
	if code != exitOK || res.Verdict != verify.Warn {
		t.Fatalf("with override: exit %d, %+v", code, res)
	}
	// An override without a reason is refused.
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"verify", "-pdf", pdfPath, "-record", recPath, "-override-by", "x"}, &out, &errb); code != exitUsage || !strings.Contains(errb.String(), "reason") {
		t.Fatalf("incomplete override: exit %d, %s", code, errb.String())
	}
}

// TestRunCommand runs the intake agent the way an operator does, through
// the standalone host: one statement in on the command line, a scripted
// model, the file store; its verified record and outcome are in the store
// afterwards, and running the same file again processes nothing (I-2).
func TestRunCommand(t *testing.T) {
	for _, bin := range []string{"pdftotext", "pdfinfo"} {
		if _, err := exec.LookPath(bin); err != nil {
			if os.Getenv("ADDEPAR_REQUIRE_POPPLER") != "" {
				t.Fatalf("%s not installed (poppler-utils), and ADDEPAR_REQUIRE_POPPLER is set", bin)
			}
			t.Skipf("%s not installed", bin)
		}
	}
	if runtime.GOOS == "windows" {
		t.Skip("the file store does not support Windows")
	}
	dir := t.TempDir()
	synth := filepath.Join("internal", "verify", "testdata", "synthetic")
	text, err := os.ReadFile(filepath.Join(synth, "keelson-vi.txt"))
	if err != nil {
		t.Fatal(err)
	}
	pdf, err := synthpdf.Build([]string{strings.TrimSuffix(string(text), "\n")}, synthpdf.Options{})
	if err != nil {
		t.Fatal(err)
	}
	pdfPath := filepath.Join(dir, "statement.pdf")
	if err = os.WriteFile(pdfPath, pdf, 0o600); err != nil {
		t.Fatal(err)
	}
	var c struct {
		Record json.RawMessage `json:"record"`
	}
	cb, err := os.ReadFile(filepath.Join(synth, "keelson-vi.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(cb, &c); err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal(map[string]any{"records": []json.RawMessage{c.Record}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(id, name, args string) llms.ChatResponse {
		tc := llms.ToolCall{ID: id, Name: name, Parameters: json.RawMessage(args)}
		return llms.ChatResponse{
			Message:   llms.Message{Role: llms.RoleAssistant, Content: []llms.ContentPart{{Type: llms.ContentToolCall, ToolCall: &tc}}},
			ToolCalls: []llms.ToolCall{tc},
		}
	}
	calls := 0
	fake := func(standalone.ModelRoute, agent.Secret) (llms.ChatClient, error) {
		return &testllm.FakeChatClient{
			ModelRef: llms.ModelRef{Provider: "fake", Name: "scripted"},
			Func: func(_ context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
				calls++
				if len(req.Messages) == 1 {
					return call("1", "get_document", `{}`), nil
				}
				return call("2", "submit_extraction", string(args)), nil
			},
		}, nil
	}
	cfg, err := json.Marshal(map[string]any{
		"agent_type": intake.TypeName,
		"namespace":  "intake",
		"store":      map[string]any{"kind": "file", "dir": filepath.Join(dir, "state")},
		"trigger":    map[string]any{"kind": "one_shot"},
		"model":      map[string]any{"provider": "fake", "name": "scripted"},
		"log":        map[string]any{"level": "error"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err = os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	p := program(map[llms.ProviderID]standalone.Provider{"fake": fake})
	var out, errb bytes.Buffer
	if code := standalone.Run(context.Background(), p, []string{"run", "-config", cfgPath, "-input", pdfPath}, &out, &errb); code != standalone.ExitComplete {
		t.Fatalf("exit %d: %s %s", code, out.String(), errb.String())
	}
	if calls != 2 {
		t.Fatalf("%d model calls, want 2", calls)
	}
	if code := standalone.Run(context.Background(), p, []string{"run", "-config", cfgPath, "-input", pdfPath}, &out, &errb); code != standalone.ExitComplete || calls != 2 {
		t.Fatalf("second run: exit %d, %d model calls: %s", code, calls, errb.String())
	}

	s, err := filestore.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sha := document.Hash(pdf)
	obj, ok, err := s.Object(context.Background(), "intake", intake.OutcomeKey(sha))
	if err != nil || !ok {
		t.Fatalf("outcome: %v, %v", ok, err)
	}
	var o intake.Outcome
	if err := json.Unmarshal(obj.JSON, &o); err != nil {
		t.Fatal(err)
	}
	if o.Disposition != intake.DispositionVerified || len(o.Records) != 1 || o.Attempts != 1 {
		t.Fatalf("outcome = %+v", o)
	}
}
