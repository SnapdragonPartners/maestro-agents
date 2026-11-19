// Package review records the people's actions on the intake agent's
// valuation records (intake R-2 to R-4) and exports approved records (X-1,
// X-2), each action a run of the review agent type (ADR 0021).
//
// The extracted record (records/<sha>/<NN>) is never rewritten. Each
// action on it is one entry of its append-only review log,
// reviews/<sha>/<NN>/<MMM>, which claims its number: an action carries how
// many entries the person was shown, and one made on a stale view is
// refused, or loses at the commit. A record's current state is the log
// folded over the stored record (Current); an approval stores the complete
// record approved, its snapshot, which is what an export exports.
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/export"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/intake"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
	"github.com/SnapdragonPartners/maestro-agents/internal/strictjson"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Kind is an action's kind (ADR 0021, items 7 and 9).
type Kind string

// The actions.
const (
	// Correct replaces the record's extraction with a person's
	// correction, verified again (R-2).
	Correct Kind = "correct"
	// Manual replaces it with, or adds to a document, a record a person
	// read off the page, verified with failed checks as warnings (R-2).
	Manual Kind = "manual"
	// AcceptWarning accepts one current warning (R-2).
	AcceptWarning Kind = "accept_warning"
	// SetEntities sets or clears the Addepar owner and asset (R-3).
	SetEntities Kind = "set_entities"
	// Approve approves the record as it stands, storing its snapshot
	// (R-4).
	Approve Kind = "approve"
	// Export exports approved records (X-1, X-2).
	Export Kind = "export"
)

// Action is a review run's trigger payload: what a person did.
type Action struct {
	// At is when, by the submitting process's clock.
	At time.Time `json:"at"`
	// Owner and Asset are set_entities' values; nil clears one.
	Owner *export.Entity `json:"owner,omitempty"`
	Asset *export.Entity `json:"asset,omitempty"`
	Kind  Kind           `json:"kind"`
	// Record is the record acted on, "<sha>/<NN>"; empty for a manual
	// record added to a document, and for an export.
	Record string `json:"record,omitempty"`
	// Document is the document a manual record is added to.
	Document string `json:"document,omitempty"`
	// By is who: never empty.
	By string `json:"by"`
	// Reason is why: required for every action but approve and export.
	Reason string `json:"reason,omitempty"`
	// Finding is the accepted warning's ID (verify.Finding.ID).
	Finding string `json:"finding,omitempty"`
	// Extraction is correct's or manual's record, in the extraction
	// record's shape, decoded strictly (ADR 0021, item 13).
	Extraction json.RawMessage `json:"extraction,omitempty"`
	// Records are the records an export exports.
	Records []string `json:"records,omitempty"`
	// Seen is how many entries the person was shown: the record's review
	// log's length, or for a manual record added to a document, how many
	// records it had.
	Seen int `json:"seen"`
}

// DecodeAction reads an action strictly.
func DecodeAction(raw []byte) (*Action, error) {
	var a Action
	if err := strictjson.Decode(raw, &a); err != nil {
		return nil, fmt.Errorf("review: action: %w", err)
	}
	return &a, nil
}

// Entry is one entry of a record's review log: the action and the
// record's verification result after it.
type Entry struct {
	At     time.Time      `json:"at"`
	Owner  *export.Entity `json:"owner,omitempty"`
	Asset  *export.Entity `json:"asset,omitempty"`
	Result *verify.Result `json:"result"`
	// Record is correct's or manual's extraction record.
	Record *statement.Record `json:"record,omitempty"`
	// Snapshot is an approval's record as approved, in the exporter's
	// shape, and SnapshotSHA256 its hash (export.SnapshotSHA256).
	Snapshot       *export.Record `json:"snapshot,omitempty"`
	Kind           Kind           `json:"kind"`
	By             string         `json:"by"`
	Reason         string         `json:"reason,omitempty"`
	Finding        string         `json:"finding,omitempty"`
	Change         string         `json:"change,omitempty"`
	SnapshotSHA256 string         `json:"snapshot_sha256,omitempty"`
	// Run is the review run that wrote it, whose execution record holds
	// the action as submitted.
	Run string `json:"run"`
	N   int    `json:"n"`
}

// Refusal is what a refused action leaves: who tried what, and why not.
type Refusal struct {
	At      time.Time `json:"at"`
	Kind    Kind      `json:"kind"`
	By      string    `json:"by"`
	Record  string    `json:"record,omitempty"`
	Code    string    `json:"code"`
	Message string    `json:"message"`
	Run     string    `json:"run"`
}

// Exported is an export's stored record (ADR 0021, item 9): the records
// and approvals it holds, and the CSV and manifest, from which the files
// can be written again byte for byte.
type Exported struct {
	At      time.Time        `json:"at"`
	Export  *export.Export   `json:"export"`
	By      string           `json:"by"`
	Run     string           `json:"run"`
	Records []ExportedRecord `json:"records"`
}

// ExportedRecord is one record of an export: its approval entry.
type ExportedRecord struct {
	ID             string `json:"id"`
	SnapshotSHA256 string `json:"snapshot_sha256"`
	Approval       int    `json:"approval"`
}

// The object keys the review type writes (ADR 0021, items 6 and 9).

// EntryPrefix lists a record's review log.
func EntryPrefix(record string) string { return "reviews/" + record + "/" }

// EntryKey is a record's entry n's key, n counted from 1.
func EntryKey(record string, n int) string { return fmt.Sprintf("%s%03d", EntryPrefix(record), n) }

// RefusalKey is a refused run's key.
func RefusalKey(run string) string { return "refusals/" + run }

// ExportPrefix lists the exports.
const ExportPrefix = "exports/"

// ExportKey is an export run's key.
func ExportKey(run string) string { return ExportPrefix + run }

// DocumentInstance is the instance of the document with the given SHA-256:
// its intake run's (ADR 0017, item 11), which its reviews share.
func DocumentInstance(sha string) string { return "sha256-" + sha }

// ExportInstancePrefix begins an export run's instance (ADR 0021, item 9).
const ExportInstancePrefix = "export-"

var recordIDPattern = regexp.MustCompile(`^([0-9a-f]{64})/(\d{2,})$`)

// SplitRecordID returns a record ID's document SHA-256 and number.
func SplitRecordID(id string) (sha string, n int, ok bool) {
	m := recordIDPattern.FindStringSubmatch(id)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n < 1 || intake.RecordID(m[1], n) != id {
		return "", 0, false
	}
	return m[1], n, true
}

// State is a record's current state: the log folded over the stored
// record (ADR 0021, item 8).
type State struct {
	// Snapshot is the current approval's snapshot.
	Snapshot *export.Record
	// Record is the record as it stands, in the exporter's shape: its
	// extraction, result, origin, owner and asset, every review action,
	// and the approval when it is approved.
	Record export.Record
	// Entries is the log's length: what an action's Seen must be.
	Entries int
	// Approval is the current approval's entry number, zero when the
	// record is not approved: approved only while the approval is the
	// log's last entry (R-4).
	Approval int
}

// Approved reports whether the record is approved.
func (s *State) Approved() bool { return s.Approval > 0 }

// ErrCorrupt reports a review log that does not fold: entries missing or
// out of number, or an entry without what its kind needs.
var ErrCorrupt = errors.New("review: corrupt review log")

// Current folds the entries, which must be numbered 1 to n, over the
// stored record.
func Current(base *export.Record, entries []Entry) (State, error) {
	r := *base
	r.Reviews = append([]export.ReviewAction(nil), base.Reviews...)
	r.Approval = nil
	s := State{Entries: len(entries)}
	for i := range entries {
		e := &entries[i]
		if e.N != i+1 {
			return State{}, fmt.Errorf("%w: entry %d is numbered %d", ErrCorrupt, i+1, e.N)
		}
		if e.Result == nil {
			return State{}, fmt.Errorf("%w: entry %d has no result", ErrCorrupt, e.N)
		}
		switch e.Kind {
		case Correct, Manual:
			if e.Record == nil {
				return State{}, fmt.Errorf("%w: entry %d has no record", ErrCorrupt, e.N)
			}
			r.Record = e.Record
			if e.Kind == Manual {
				r.Origin = export.OriginManual
			}
		case SetEntities:
			r.Owner, r.Asset = e.Owner, e.Asset
		case AcceptWarning:
		case Approve:
			if e.Snapshot == nil {
				return State{}, fmt.Errorf("%w: approval %d has no snapshot", ErrCorrupt, e.N)
			}
			continue
		default:
			return State{}, fmt.Errorf("%w: entry %d has kind %q", ErrCorrupt, e.N, e.Kind)
		}
		r.Result = *e.Result
		r.Reviews = append(r.Reviews, export.ReviewAction{
			At: e.At, Kind: export.ReviewKind(e.Kind), By: e.By, Reason: e.Reason, Finding: e.Finding, Change: e.Change,
		})
	}
	if n := len(entries); n > 0 && entries[n-1].Kind == Approve {
		last := &entries[n-1]
		s.Approval, s.Snapshot = last.N, last.Snapshot
		r.Approval = &export.Approval{By: last.By, At: last.At}
	}
	s.Record = r
	return s, nil
}

// objects reads the namespace's objects: a run's Env, or a store.
type objects interface {
	object(ctx context.Context, key string) (json.RawMessage, bool, error)
	list(ctx context.Context, prefix string) ([]string, error)
}

// StoreObjects reads a store's namespace, for a host or a UI.
type StoreObjects struct {
	Reader    store.Reader
	Namespace string
}

func (s StoreObjects) object(ctx context.Context, key string) (json.RawMessage, bool, error) {
	o, ok, err := s.Reader.Object(ctx, s.Namespace, key)
	return o.JSON, ok, err //nolint:wrapcheck // the caller names the key
}

func (s StoreObjects) list(ctx context.Context, prefix string) ([]string, error) {
	infos, err := s.Reader.ListObjects(ctx, s.Namespace, prefix)
	keys := make([]string, len(infos))
	for i, in := range infos {
		keys[i] = in.Key
	}
	return keys, err //nolint:wrapcheck // the caller names the prefix
}

func readJSON(ctx context.Context, o objects, key string, v any) (bool, error) {
	raw, ok, err := o.object(ctx, key)
	if err != nil || !ok {
		return ok, err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return true, fmt.Errorf("review: decode %s: %w", key, err)
	}
	return true, nil
}

// errNoRecord is load's error for a record that does not exist.
var errNoRecord = errors.New("review: no such record")

// load reads a record and its log, and folds them.
func load(ctx context.Context, o objects, id string) (State, []Entry, error) {
	var base export.Record
	ok, err := readJSON(ctx, o, intake.RecordKey(id), &base)
	switch {
	case err != nil:
		return State{}, nil, err
	case !ok:
		return State{}, nil, fmt.Errorf("%w: %s", errNoRecord, id)
	}
	keys, err := o.list(ctx, EntryPrefix(id))
	if err != nil {
		return State{}, nil, err
	}
	entries := make([]Entry, 0, len(keys))
	for _, k := range keys {
		var e Entry
		if _, err = readJSON(ctx, o, k, &e); err != nil {
			return State{}, nil, err
		}
		if k != EntryKey(id, e.N) {
			return State{}, nil, fmt.Errorf("%w: %s holds entry %d", ErrCorrupt, k, e.N)
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].N < entries[j].N })
	s, err := Current(&base, entries)
	return s, entries, err
}

// Load returns a record's current state from a store.
func Load(ctx context.Context, o StoreObjects, id string) (State, error) {
	s, _, err := load(ctx, o, id)
	return s, err
}

// LoadLog returns a record's current state and its whole review log, in
// order: the log keeps every entry, approvals a later action voided
// included, which the state's exporter-shaped record does not.
func LoadLog(ctx context.Context, o StoreObjects, id string) (State, []Entry, error) {
	return load(ctx, o, id)
}

// Records lists every record's ID, in key order.
func Records(ctx context.Context, o StoreObjects) ([]string, error) {
	keys, err := o.list(ctx, "records/")
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(keys))
	for _, k := range keys {
		if id := strings.TrimPrefix(k, "records/"); id != k {
			if _, _, ok := SplitRecordID(id); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}

// Exports reads every export, in key order.
func Exports(ctx context.Context, o StoreObjects) ([]Exported, error) {
	keys, err := o.list(ctx, ExportPrefix)
	if err != nil {
		return nil, err
	}
	out := make([]Exported, 0, len(keys))
	for _, k := range keys {
		var e Exported
		if _, err := readJSON(ctx, o, k, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// ExportedApprovals maps each record ID to the approval entries an export
// holds: a record is exported when its current approval is among them.
func ExportedApprovals(exports []Exported) map[string]map[int]bool {
	out := map[string]map[int]bool{}
	for _, e := range exports {
		for _, r := range e.Records {
			if out[r.ID] == nil {
				out[r.ID] = map[int]bool{}
			}
			out[r.ID][r.Approval] = true
		}
	}
	return out
}
