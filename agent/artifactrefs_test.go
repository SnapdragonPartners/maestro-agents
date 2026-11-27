package agent_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/memstore"
)

func hexDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ArtifactReferences reads the runtime's typed digest fields alone: a
// digest inside an agent type's checkpoint data, a transition's data or an
// entry kind it does not know is not a reference, and another instance's
// records name nothing for this one.
func TestArtifactReferencesTypedFieldsOnly(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()
	d := map[string]string{}
	for _, n := range []string{"manifest", "config", "served", "params", "state-params", "message", "response", "turn-message", "result", "ck-params", "ck-transcript", "data", "unknown", "transition", "other"} {
		d[n] = hexDigest(n)
	}
	setup := func(instance, run string) store.Fence {
		t.Helper()
		if _, _, err := s.CreateRun(ctx, store.Run{ID: run, Instance: instance, AgentType: "a", TriggerID: run}); err != nil {
			t.Fatal(err)
		}
		own, err := s.Claim(ctx, instance, "o", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return own.Fence()
	}
	f := setup("inst-x", "run-x")
	payload := func(v any) json.RawMessage {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	state := payload(map[string]any{
		"agent_type": "a", "agent_version": "1", "state": "s", "namespace": "n", "step": 1,
		"config": d["config"], "served": d["served"],
		"model": map[string]any{"params": d["ck-params"], "transcript": []string{d["ck-transcript"]}},
		"data":  map[string]string{"looks_like_a_reference": d["data"]},
	})
	ops := []store.Op{
		store.AppendEntryOp(store.Entry{Run: "run-x", Seq: 1, Kind: agent.EntryRunStarted, Payload: payload(agent.RunStarted{Manifest: d["manifest"], Config: d["config"], Served: d["served"]})}),
		store.AppendEntryOp(store.Entry{Run: "run-x", Seq: 2, Kind: agent.EntryModelAttempt, Payload: payload(agent.ModelAttemptEntry{Params: d["params"], StateParams: d["state-params"], Messages: []string{d["message"]}})}),
		store.AppendEntryOp(store.Entry{Run: "run-x", Seq: 3, Kind: agent.EntryTurn, Payload: payload(agent.TurnEntry{Response: d["response"], Message: d["turn-message"]})}),
		store.AppendEntryOp(store.Entry{Run: "run-x", Seq: 4, Kind: agent.EntryResult, Payload: payload(agent.ResultEntry{Result: d["result"]})}),
		store.AppendEntryOp(store.Entry{Run: "run-x", Seq: 5, Kind: "subagent_started", Payload: payload(map[string]string{"artifact": d["unknown"]})}),
		store.AppendEntryOp(store.Entry{Run: "run-x", Seq: 6, Kind: agent.EntryTransition, Payload: payload(map[string]any{"from": "s", "to": "t", "data": map[string]string{"x": d["transition"]}})}),
		store.PutCheckpointOp(store.Checkpoint{Instance: "inst-x", Scope: store.Scope{Kind: store.ScopeRun, ID: "run-x"}, Seq: 1, RecordSeq: 6, Format: 1, State: state}),
	}
	if err := s.Commit(ctx, f, ops); err != nil {
		t.Fatal(err)
	}
	g := setup("inst-y", "run-y")
	if err := s.Commit(ctx, g, []store.Op{store.AppendEntryOp(store.Entry{Run: "run-y", Seq: 1, Kind: agent.EntryRunStarted, Payload: payload(agent.RunStarted{Config: d["other"]})})}); err != nil {
		t.Fatal(err)
	}
	got, err := agent.ArtifactReferences(ctx, s, "inst-x")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, n := range []string{"manifest", "config", "served", "params", "state-params", "message", "response", "turn-message", "result", "ck-params", "ck-transcript"} {
		want = append(want, d[n])
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("ArtifactReferences = %v\nwant %v", got, want)
	}
	if got, err := agent.ArtifactReferences(ctx, s, "inst-none"); err != nil || len(got) != 0 {
		t.Fatalf("ArtifactReferences(no records) = %v, %v; want none", got, err)
	}
}

func TestArtifactReferencesRefusesWhatItCannotRead(t *testing.T) {
	cases := []struct {
		name   string
		op     store.Op
		detail string
	}{
		{"undecodable recognized entry", store.AppendEntryOp(store.Entry{Run: "run-x", Seq: 1, Kind: agent.EntryRunStarted, Payload: json.RawMessage(`{"config":7}`)}), "entry 1 (run_started)"},
		{"unknown checkpoint format", store.PutCheckpointOp(store.Checkpoint{Instance: "inst-x", Scope: store.Scope{Kind: store.ScopeRun, ID: "run-x"}, Seq: 1, Format: 9, State: json.RawMessage(`{}`)}), "format 9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := memstore.New()
			if _, _, err := s.CreateRun(ctx, store.Run{ID: "run-x", Instance: "inst-x", AgentType: "a", TriggerID: "t"}); err != nil {
				t.Fatal(err)
			}
			own, err := s.Claim(ctx, "inst-x", "o", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Commit(ctx, own.Fence(), []store.Op{tc.op}); err != nil {
				t.Fatal(err)
			}
			if _, err := agent.ArtifactReferences(ctx, s, "inst-x"); err == nil || !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("ArtifactReferences = %v, want an error naming %q", err, tc.detail)
			}
		})
	}
}
