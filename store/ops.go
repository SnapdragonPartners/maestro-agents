package store

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// OpKind names the kind of a commit operation.
type OpKind string

// The operation kinds of ADR 0005, item 3. New kinds of record add
// operations; they do not add write paths.
const (
	OpPutObject     OpKind = "put_object"
	OpDeleteObject  OpKind = "delete_object"
	OpAppendEntry   OpKind = "append_entry"
	OpPutCheckpoint OpKind = "put_checkpoint"
	OpUpdateRun     OpKind = "update_run"
	OpPutArtifact   OpKind = "put_artifact"
	OpRelease       OpKind = "release"
)

// Op is one operation of a commit. It is a JSON value (P-6): Kind names
// the operation and exactly the matching field is set. Build one with the
// constructors ([PutObjectOp] and the rest).
type Op struct {
	PutObject     *PutObject    `json:"put_object,omitempty"`
	DeleteObject  *DeleteObject `json:"delete_object,omitempty"`
	AppendEntry   *Entry        `json:"append_entry,omitempty"`
	PutCheckpoint *Checkpoint   `json:"put_checkpoint,omitempty"`
	UpdateRun     *RunUpdate    `json:"update_run,omitempty"`
	PutArtifact   *Artifact     `json:"put_artifact,omitempty"`
	Release       *Release      `json:"release,omitempty"`
	Kind          OpKind        `json:"kind"`
}

// PutObject writes an object, conditional on its current version: the
// version of the object, or for an absent key the version it would have to
// be created over (0 for a key never written; the deleted object's version
// after a deletion). The new version is ExpectVersion+1. Exactly one of
// JSON and Bytes is set.
type PutObject struct {
	Namespace string          `json:"namespace"`
	Key       string          `json:"key"`
	JSON      json.RawMessage `json:"json,omitempty"`
	// Bytes has no omitempty, so an empty byte value ("") survives a JSON
	// round trip as distinct from an unset one (null).
	Bytes         []byte `json:"bytes"`
	ExpectVersion uint64 `json:"expect_version"`
}

// DeleteObject deletes an existing object at ExpectVersion. The deletion is
// itself a version (ExpectVersion+1), so a later creation of the key must
// expect it and no writer holding an older version can succeed.
type DeleteObject struct {
	Namespace     string `json:"namespace"`
	Key           string `json:"key"`
	ExpectVersion uint64 `json:"expect_version"`
}

// RunUpdate changes a run record's mutable fields, conditional on its
// version. Under a passed fence the precondition fails only if the runtime
// or the store has broken its contract, since only the owner writes its
// runs (ADR 0005, item 3). A terminal run cannot be made non-terminal.
type RunUpdate struct {
	Run      string          `json:"run"`
	Status   Status          `json:"status"`
	Reason   Reason          `json:"reason"`
	Consumed json.RawMessage `json:"consumed,omitempty"`
	// ExpectVersion is the run record's current version.
	ExpectVersion uint64 `json:"expect_version"`
}

// Artifact stores content-addressed, write-once content with its metadata.
// Storing a digest already stored is a no-op whatever its metadata: the
// first metadata stands, and the operation never conflicts.
type Artifact struct {
	// Digest is the lowercase hex SHA-256 of Content; the store verifies
	// it.
	Digest    string `json:"digest"`
	MediaType string `json:"media_type"`
	Content   []byte `json:"content"`
}

// Release clears the fenced instance's owner and lease, keeping its epoch
// (ADR 0003, item 10). An activation's final commit carries it.
type Release struct{}

// PutObjectJSON returns an operation that writes a JSON value.
func PutObjectJSON(namespace, key string, expectVersion uint64, value json.RawMessage) Op {
	return Op{Kind: OpPutObject, PutObject: &PutObject{
		Namespace: namespace, Key: key, ExpectVersion: expectVersion, JSON: value,
	}}
}

// PutObjectBytes returns an operation that writes a byte value.
func PutObjectBytes(namespace, key string, expectVersion uint64, value []byte) Op {
	return Op{Kind: OpPutObject, PutObject: &PutObject{
		Namespace: namespace, Key: key, ExpectVersion: expectVersion, Bytes: value,
	}}
}

// DeleteObjectOp returns an operation that deletes an object.
func DeleteObjectOp(namespace, key string, expectVersion uint64) Op {
	return Op{Kind: OpDeleteObject, DeleteObject: &DeleteObject{
		Namespace: namespace, Key: key, ExpectVersion: expectVersion,
	}}
}

// AppendEntryOp returns an operation that appends an execution-record entry
// at e.Seq, which must be the run's next sequence number.
func AppendEntryOp(e Entry) Op { return Op{Kind: OpAppendEntry, AppendEntry: &e} }

// PutCheckpointOp returns an operation that publishes a checkpoint at c.Seq,
// which must be the instance's next checkpoint sequence number.
func PutCheckpointOp(c Checkpoint) Op { return Op{Kind: OpPutCheckpoint, PutCheckpoint: &c} }

// UpdateRunOp returns an operation that changes a run record.
func UpdateRunOp(u RunUpdate) Op { return Op{Kind: OpUpdateRun, UpdateRun: &u} }

// PutArtifactOp returns an operation that stores an artifact.
func PutArtifactOp(a Artifact) Op { return Op{Kind: OpPutArtifact, PutArtifact: &a} }

// ReleaseOp returns an operation that releases the fenced instance.
func ReleaseOp() Op { return Op{Kind: OpRelease, Release: &Release{}} }

// Validate reports whether the operation is well formed: Kind is known and
// exactly its field is set. It does not check preconditions. The error
// wraps [ErrInvalid].
func (op Op) Validate() error {
	fields := map[OpKind]bool{
		OpPutObject:     op.PutObject != nil,
		OpDeleteObject:  op.DeleteObject != nil,
		OpAppendEntry:   op.AppendEntry != nil,
		OpPutCheckpoint: op.PutCheckpoint != nil,
		OpUpdateRun:     op.UpdateRun != nil,
		OpPutArtifact:   op.PutArtifact != nil,
		OpRelease:       op.Release != nil,
	}
	set := 0
	for _, isSet := range fields {
		if isSet {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("%w: operation %q must set exactly one operation field, has %d", ErrInvalid, op.Kind, set)
	}
	isSet, known := fields[op.Kind]
	if !known {
		return fmt.Errorf("%w: unknown operation kind %q", ErrInvalid, op.Kind)
	}
	if !isSet {
		return fmt.Errorf("%w: operation kind %q does not match the field set", ErrInvalid, op.Kind)
	}
	for _, s := range op.strings() {
		if !utf8.ValidString(s) {
			return fmt.Errorf("%w: operation %q holds a string that is not valid UTF-8", ErrInvalid, op.Kind)
		}
	}
	for _, raw := range op.raws() {
		if err := ValidJSON(raw); err != nil {
			return fmt.Errorf("operation %q: %w", op.Kind, err)
		}
	}
	if p := op.PutObject; p != nil && (p.JSON == nil) == (p.Bytes == nil) {
		return fmt.Errorf("%w: put_object must set exactly one of json and bytes", ErrInvalid)
	}
	return nil
}

// ValidJSON reports whether raw, if set, is a JSON value a store can keep:
// valid JSON in valid UTF-8 (RFC 8259, section 8.1). encoding/json accepts
// and stores invalid UTF-8 inside a raw value byte for byte, but a reader
// decoding it gets U+FFFD in place of the bytes, and a store on a JSON
// column type refuses it. A nil raw is unset and valid; an empty, non-nil
// one is not JSON. The error wraps [ErrInvalid].
func ValidJSON(raw json.RawMessage) error {
	if raw == nil {
		return nil
	}
	if !json.Valid(raw) {
		return fmt.Errorf("%w: value is not valid JSON", ErrInvalid)
	}
	if !utf8.Valid(raw) {
		return fmt.Errorf("%w: JSON value is not valid UTF-8", ErrInvalid)
	}
	return nil
}

// raws returns every raw JSON value the operation stores.
func (op Op) raws() []json.RawMessage {
	var out []json.RawMessage
	if p := op.PutObject; p != nil {
		out = append(out, p.JSON)
	}
	if e := op.AppendEntry; e != nil {
		out = append(out, e.Payload)
	}
	if c := op.PutCheckpoint; c != nil {
		out = append(out, c.State)
	}
	if u := op.UpdateRun; u != nil {
		out = append(out, u.Consumed)
	}
	return out
}

// strings returns every string the operation stores or matches on. Each
// must be valid UTF-8: encoding/json replaces invalid bytes with U+FFFD, so
// such a string would not read back as written.
func (op Op) strings() []string {
	var out []string
	if p := op.PutObject; p != nil {
		out = append(out, p.Namespace, p.Key)
	}
	if d := op.DeleteObject; d != nil {
		out = append(out, d.Namespace, d.Key)
	}
	if e := op.AppendEntry; e != nil {
		out = append(out, e.Run, e.Kind)
	}
	if c := op.PutCheckpoint; c != nil {
		out = append(out, c.Instance, string(c.Scope.Kind), c.Scope.ID)
	}
	if u := op.UpdateRun; u != nil {
		out = append(out, u.Run, string(u.Status), u.Reason.Code, u.Reason.Budget)
	}
	if a := op.PutArtifact; a != nil {
		out = append(out, a.Digest, a.MediaType)
	}
	return out
}
