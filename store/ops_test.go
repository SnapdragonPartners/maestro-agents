package store_test

import (
	"encoding/json"
	"testing"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Every operation survives a JSON round trip (P-6) and still validates; in
// particular an empty byte value stays distinct from an unset one.
func TestOpJSONRoundTrip(t *testing.T) {
	ops := []store.Op{
		store.PutObjectBytes("ns", "k", 0, []byte{}),
		store.PutObjectBytes("ns", "k", 0, []byte{1}),
		store.PutObjectJSON("ns", "k", 3, json.RawMessage(`{"a":1}`)),
		store.DeleteObjectOp("ns", "k", 2),
		store.AppendEntryOp(store.Entry{Run: "r", Seq: 1, Kind: "turn"}),
		store.PutCheckpointOp(store.Checkpoint{Instance: "i", Seq: 1, Scope: store.Scope{Kind: store.ScopeRun, ID: "r"}, State: json.RawMessage(`{}`)}),
		store.UpdateRunOp(store.RunUpdate{Run: "r", ExpectVersion: 1, Status: store.StatusSucceeded}),
		store.PutArtifactOp(store.Artifact{Digest: "d", MediaType: "text/plain", Content: []byte{}}),
		store.ReleaseOp(),
	}
	for _, op := range ops {
		if err := op.Validate(); err != nil {
			t.Fatalf("%s: Validate before round trip: %v", op.Kind, err)
		}
		data, err := json.Marshal(op)
		if err != nil {
			t.Fatal(err)
		}
		var back store.Op
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatal(err)
		}
		if err := back.Validate(); err != nil {
			t.Fatalf("%s: Validate after round trip of %s: %v", op.Kind, data, err)
		}
		if p := back.PutObject; p != nil && op.PutObject.Bytes != nil && (p.Bytes == nil || len(p.Bytes) != len(op.PutObject.Bytes)) {
			t.Fatalf("bytes value lost in round trip: %s", data)
		}
	}
}
