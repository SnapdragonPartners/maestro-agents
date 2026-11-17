package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
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
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/export"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/intake"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/synthpdf"
	"github.com/SnapdragonPartners/maestro-agents/scoring"
	"github.com/SnapdragonPartners/maestro-agents/standalone"
)

func requirePoppler(t *testing.T) {
	t.Helper()
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
}

// synthStatement renders a synthetic statement into dir and returns its
// SHA-256 and its hand-written record.
func synthStatement(t *testing.T, dir, name string) (sha string, record json.RawMessage) {
	t.Helper()
	synth := filepath.Join("internal", "verify", "testdata", "synthetic")
	text, err := os.ReadFile(filepath.Join(synth, name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	pages := strings.Split(strings.TrimSuffix(string(text), "\n"), "\n<<<page>>>\n")
	pdf, err := synthpdf.Build(pages, synthpdf.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, name+".pdf"), pdf, 0o600); err != nil {
		t.Fatal(err)
	}
	var c struct {
		Record json.RawMessage `json:"record"`
	}
	cb, err := os.ReadFile(filepath.Join(synth, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(cb, &c); err != nil {
		t.Fatal(err)
	}
	return document.Hash(pdf), c.Record
}

func toolCall(id, name, args string) llms.ChatResponse {
	tc := llms.ToolCall{ID: id, Name: name, Parameters: json.RawMessage(args)}
	return llms.ChatResponse{
		Message:   llms.Message{Role: llms.RoleAssistant, Content: []llms.ContentPart{{Type: llms.ContentToolCall, ToolCall: &tc}}},
		ToolCalls: []llms.ToolCall{tc},
	}
}

// scriptedProgram's model reads the document, then answers by the
// statement it was given: the text naming a key of answers gets that
// response's tool call.
func scriptedProgram(answers map[string]llms.ChatResponse) standalone.Program {
	fake := func(standalone.ModelRoute, agent.Secret) (llms.ChatClient, error) {
		return &testllm.FakeChatClient{
			ModelRef: llms.ModelRef{Provider: "fake", Name: "scripted"},
			Func: func(_ context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
				if len(req.Messages) == 1 {
					return toolCall("1", "get_document", `{}`), nil
				}
				seen, _ := json.Marshal(req.Messages)
				for marker, resp := range answers {
					if bytes.Contains(seen, []byte(marker)) {
						return resp, nil
					}
				}
				return toolCall("2", "request_manual_review", `{"reason": "unscripted"}`), nil
			},
		}, nil
	}
	return program(map[llms.ProviderID]standalone.Provider{"fake": fake})
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func scoreConfig(t *testing.T, dir, state string) string {
	t.Helper()
	path := filepath.Join(dir, state+".json")
	writeJSONFile(t, path, map[string]any{
		"agent_type": intake.TypeName,
		"namespace":  "intake",
		"store":      map[string]any{"kind": "file", "dir": filepath.Join(dir, state)},
		"trigger":    map[string]any{"kind": "one_shot"},
		"model":      map[string]any{"provider": "fake", "name": "scripted"},
		"log":        map[string]any{"level": "error"},
	})
	return path
}

// writeKey writes an answer key as Excel saves the template: a title row,
// the header, the rows (date as M/D/YYYY).
func writeKey(t *testing.T, path string, rows ...[]string) {
	t.Helper()
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	title := make([]string, 19)
	title[1] = "Valuations and snapshots import template"
	all := append([][]string{title, export.Columns()}, rows...)
	if err := w.WriteAll(all); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func answerRow(ownerID, owner, assetID, asset, value string) []string {
	r := make([]string, 19)
	r[0], r[1], r[3], r[4], r[10], r[11], r[12], r[17] = ownerID, owner, assetID, asset, "Value Based", "3/31/2026", value, "USD"
	return r
}

func TestScoreCommand(t *testing.T) {
	requirePoppler(t)
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	if err := os.Mkdir(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	keelson, keelsonRec := synthStatement(t, inbox, "keelson-vi")
	tributary, tributaryRec := synthStatement(t, inbox, "tributary-3")
	halyard, _ := synthStatement(t, inbox, "halyard-ii")
	_, brightwaterRec := synthStatement(t, inbox, "brightwater-ii")
	writeFile := func(name, content string) {
		if err := os.WriteFile(filepath.Join(inbox, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(".hidden.pdf", "not scored")
	writeFile("notes.txt", "not scored")

	submit := func(rec json.RawMessage) llms.ChatResponse {
		args, err := json.Marshal(map[string]any{"records": []json.RawMessage{rec}})
		if err != nil {
			t.Fatal(err)
		}
		return toolCall("2", "submit_extraction", string(args))
	}
	p := scriptedProgram(map[string]llms.ChatResponse{
		"Keelson Partners VI":          submit(keelsonRec),
		"Tributary Fund 3":             submit(tributaryRec),
		"Halyard Growth Fund II":       toolCall("2", "request_manual_review", `{"reason": "the columns are unclear"}`),
		"Brightwater Ventures Fund II": submit(brightwaterRec),
	})

	mapPath := filepath.Join(dir, "map.json")
	writeJSONFile(t, mapPath, map[string]any{"statements": []any{
		map[string]any{"sha256": keelson, "positions": []any{map[string]any{
			"investor": "Example Holdings IV, LLC", "fund": "Keelson Partners VI, L.P.", "owner_id": "1", "asset_id": "10"}}},
		map[string]any{"sha256": tributary, "positions": []any{map[string]any{
			"investor": "Sample Partners VII, LLC", "fund": "Tributary Fund 3, LP", "owner_id": "2", "asset_id": "20"}}},
		map[string]any{"sha256": halyard, "positions": []any{map[string]any{
			"investor": "Example Holdings IV, LLC", "fund": "Halyard Growth Fund II, L.P.", "owner_id": "1", "asset_id": "30"}}},
	}})
	keyPath := filepath.Join(dir, "key.csv")
	rows := func(tributaryValue string) [][]string {
		return [][]string{
			answerRow("1", "EXAMPLE HOLDINGS IV, LLC", "10", "Keelson Partners VI, LP", "678,250"),
			answerRow("2", "SAMPLE PARTNERS VII, LLC", "20", "Tributary Fund III, LP", tributaryValue),
			answerRow("1", "EXAMPLE HOLDINGS IV, LLC", "30", "Halyard Growth Fund II, LP", "714760.65"),
		}
	}
	// The key says Tributary is one dollar more than the verified record:
	// a wrong value that passed every check.
	writeKey(t, keyPath, rows("6038339")...)

	var out, errb bytes.Buffer
	args := []string{"score", "-config", scoreConfig(t, dir, "state1"), "-inputs", inbox, "-key", keyPath, "-map", mapPath, "-json"}
	if code := run(context.Background(), p, args, &out, &errb); code != exitFail {
		t.Fatalf("exit %d, want %d: %s %s", code, exitFail, out.String(), errb.String())
	}
	var rep scoring.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	got := map[string]string{}
	for _, r := range rep.Results {
		vs := []string{}
		for _, it := range r.Items {
			vs = append(vs, string(it.Verdict))
		}
		if r.Uncovered {
			vs = append(vs, "uncovered")
		}
		if r.Error != "" {
			vs = append(vs, "error: "+r.Error)
		}
		got[filepath.Base(r.Input.Path)] = strings.Join(vs, ",")
	}
	want := map[string]string{
		"brightwater-ii.pdf": "uncovered", "halyard-ii.pdf": "exception",
		"keelson-vi.pdf": "match", "tributary-3.pdf": "mismatch",
	}
	if len(got) != len(want) {
		t.Fatalf("scored %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
	if rep.Totals.Tokens <= 0 || rep.Totals.Turns != 8 || rep.Totals.Recorded != 0 || !rep.Complete {
		t.Errorf("totals %+v", rep.Totals)
	}

	// The same store again: every input is its recorded run.
	out.Reset()
	args = args[:len(args)-1] // the text report this time
	if code := run(context.Background(), p, args, &out, &errb); code != exitFail {
		t.Fatalf("rerun exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "4 input(s) reported from runs already in the store") || !strings.Contains(out.String(), "MISMATCH") {
		t.Errorf("text report:\n%s", out.String())
	}

	// With the right key, nothing wrong came out: exit 0, though Halyard
	// is an exception.
	writeKey(t, keyPath, rows("6038338")...)
	out.Reset()
	args = []string{"score", "-config", scoreConfig(t, dir, "state2"), "-inputs", inbox, "-key", keyPath, "-map", mapPath}
	if code := run(context.Background(), p, args, &out, &errb); code != exitOK {
		t.Fatalf("exit %d, want 0: %s %s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "PASS") {
		t.Errorf("text report:\n%s", out.String())
	}
}

func TestScoreUsage(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.csv")
	writeKey(t, keyPath, answerRow("1", "A", "10", "F", "1"))
	mapPath := filepath.Join(dir, "map.json")
	writeJSONFile(t, mapPath, map[string]any{"statements": []any{}})
	badMap := filepath.Join(dir, "bad.json")
	writeJSONFile(t, badMap, map[string]any{"statements": []any{map[string]any{
		"sha256": strings.Repeat("a", 64), "positions": []any{map[string]any{"investor": "A", "fund": "F", "owner_id": "1", "asset_id": "99"}}}}})
	empty := filepath.Join(dir, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	inbox := filepath.Join(dir, "inbox")
	if err := os.Mkdir(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "a.PDF"), []byte("%PDF"), 0o600); err != nil {
		t.Fatal(err)
	}
	folderCfg := filepath.Join(dir, "folder.json")
	writeJSONFile(t, folderCfg, map[string]any{
		"agent_type": intake.TypeName, "namespace": "intake",
		"store":   map[string]any{"kind": "memory"},
		"trigger": map[string]any{"kind": "folder", "dir": inbox},
		"model":   map[string]any{"provider": "fake", "name": "scripted"},
	})
	p := scriptedProgram(nil)
	for name, args := range map[string][]string{
		"no flags":             {"score"},
		"missing map":          {"score", "-config", folderCfg, "-inputs", inbox, "-key", keyPath},
		"extra argument":       {"score", "-config", folderCfg, "-inputs", inbox, "-key", keyPath, "-map", mapPath, "x"},
		"unreadable key":       {"score", "-config", folderCfg, "-inputs", inbox, "-key", filepath.Join(dir, "none.csv"), "-map", mapPath},
		"key not a key":        {"score", "-config", folderCfg, "-inputs", inbox, "-key", mapPath, "-map", mapPath},
		"mapping names no row": {"score", "-config", folderCfg, "-inputs", inbox, "-key", keyPath, "-map", badMap},
		"no PDF":               {"score", "-config", folderCfg, "-inputs", empty, "-key", keyPath, "-map", mapPath},
		"folder trigger":       {"score", "-config", folderCfg, "-inputs", inbox, "-key", keyPath, "-map", mapPath},
	} {
		t.Run(name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := run(context.Background(), p, args, &out, &errb); code != exitUsage {
				t.Fatalf("exit %d, want %d: %s", code, exitUsage, errb.String())
			}
		})
	}
	in, err := pdfInputs(inbox)
	if err != nil || len(in) != 1 {
		t.Fatalf("a .PDF is an input: %v, %v", in, err)
	}
}
