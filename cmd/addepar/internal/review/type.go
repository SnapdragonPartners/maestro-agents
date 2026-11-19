package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/document"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/export"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/intake"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
	"github.com/SnapdragonPartners/maestro-agents/standalone"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// The agent type's name and version.
const (
	TypeName    = "addepar-review"
	TypeVersion = "1"
)

// The type's states.
const (
	stateApply   = "apply"
	stateApplied = "applied"
	stateRefused = "refused"
)

// Data is the type's state data: it keeps none.
type Data struct{}

// Type is the review agent type (ADR 0021, item 5): one code state, apply,
// which writes the action's objects and ends applied (changed), or writes
// the refusal and ends refused (no change). It runs on a submitted trigger
// (standalone.Program.Submitted).
func Type() *agent.Type[Data] {
	return &agent.Type[Data]{
		Name:     TypeName,
		Version:  TypeVersion,
		Triggers: []string{standalone.SubmittedKind},
		Initial:  stateApply,
		States: []agent.State[Data]{
			{Name: stateApply, Next: []string{stateApplied, stateRefused}, Code: apply},
			{Name: stateApplied, End: agent.EndChanged},
			{Name: stateRefused, End: agent.EndNoChange},
		},
	}
}

// The refusal codes: a refused action is the person's to fix, never the
// system's failure.
const (
	RefuseMalformed      = "malformed"
	RefuseIncomplete     = "incomplete"
	RefuseWrongInstance  = "wrong_instance"
	RefuseNoRecord       = "no_record"
	RefuseNoDocument     = "no_document"
	RefuseStale          = "stale"
	RefuseClock          = "clock"
	RefuseNoChange       = "no_change"
	RefuseNoSuchWarning  = "no_such_warning"
	RefuseAccepted       = "already_accepted"
	RefuseApproved       = "already_approved"
	RefuseNotApprovable  = "not_approvable"
	RefuseNotApproved    = "not_approved"
	RefuseInvalidEntity  = "invalid_entity"
	RefuseExportRefused  = "export_refused"
	RefuseUnknownKind    = "unknown_kind"
	maxRefusalMessageLen = 2000
)

// refusal is an action apply refuses.
type refusal struct{ code, message string }

func (r *refusal) Error() string { return r.code + ": " + r.message }

func refuse(code, format string, args ...any) error {
	return &refusal{code: code, message: fmt.Sprintf(format, args...)}
}

// envObjects reads the run's namespace.
type envObjects struct{ env *agent.Env }

func (e envObjects) object(ctx context.Context, key string) (json.RawMessage, bool, error) {
	o, ok, err := e.env.Object(ctx, key)
	return o.JSON, ok, err //nolint:wrapcheck // a *agent.ReadError, which the runtime classifies
}

func (e envObjects) list(ctx context.Context, prefix string) ([]string, error) {
	infos, err := e.env.ListObjects(ctx, prefix)
	keys := make([]string, len(infos))
	for i, in := range infos {
		keys[i] = in.Key
	}
	return keys, err //nolint:wrapcheck // a *agent.ReadError, which the runtime classifies
}

func apply(ctx context.Context, env *agent.Env, d Data) (agent.Transition[Data], error) {
	a, err := DecodeAction(env.Trigger())
	var ops []store.Op
	if err != nil {
		err = refuse(RefuseMalformed, "%v", err)
		// The refusal still records who tried what, as far as a lenient
		// reading shows; nothing else is taken from it.
		var lenient Action
		_ = json.Unmarshal(env.Trigger(), &lenient)
		a = &lenient
	} else {
		ops, err = applyAction(ctx, env, a)
	}
	var r *refusal
	switch {
	case errors.As(err, &r):
		ref := Refusal{Code: r.code, Message: truncate(r.message), Run: env.RunID(), At: a.At, Kind: a.Kind, By: a.By, Record: a.Record}
		op, perr := putNew(env, RefusalKey(env.RunID()), ref)
		if perr != nil {
			return agent.Transition[Data]{}, perr
		}
		return agent.Goto(stateRefused, d, op), nil
	case err != nil:
		return agent.Transition[Data]{}, err
	}
	return agent.Goto(stateApplied, d, ops...), nil
}

func truncate(s string) string {
	if len(s) <= maxRefusalMessageLen {
		return s
	}
	return s[:maxRefusalMessageLen] + "…"
}

func applyAction(ctx context.Context, env *agent.Env, a *Action) ([]store.Op, error) {
	if strings.TrimSpace(a.By) == "" || a.At.IsZero() {
		return nil, refuse(RefuseIncomplete, "an action records who and when")
	}
	if a.Kind != Approve && a.Kind != Export && strings.TrimSpace(a.Reason) == "" {
		return nil, refuse(RefuseIncomplete, "a %s records why", a.Kind)
	}
	o := envObjects{env}
	switch a.Kind {
	case Export:
		return applyExport(ctx, env, o, a)
	case Manual:
		if a.Record == "" {
			return applyNewManual(ctx, env, o, a)
		}
	case Correct, AcceptWarning, SetEntities, Approve:
	default:
		return nil, refuse(RefuseUnknownKind, "unknown action %q", a.Kind)
	}
	return applyToRecord(ctx, env, o, a)
}

// applyToRecord applies an action to an existing record: one new entry of
// its log, at the number after the person's view (ADR 0021, item 6).
func applyToRecord(ctx context.Context, env *agent.Env, o objects, a *Action) ([]store.Op, error) {
	sha, _, ok := SplitRecordID(a.Record)
	if !ok {
		return nil, refuse(RefuseMalformed, "record %q is not <sha256>/<NN>", a.Record)
	}
	if env.Instance() != DocumentInstance(sha) {
		return nil, refuse(RefuseWrongInstance, "record %s is not this instance's document", a.Record)
	}
	s, entries, err := load(ctx, o, a.Record)
	switch {
	case errors.Is(err, errNoRecord):
		return nil, refuse(RefuseNoRecord, "record %s does not exist", a.Record)
	case err != nil:
		return nil, err
	}
	if a.Seen != s.Entries {
		return nil, refuse(RefuseStale, "the record has %d review entries, the action saw %d: reload it", s.Entries, a.Seen)
	}
	if n := len(entries); n > 0 && a.At.Before(entries[n-1].At) {
		return nil, refuse(RefuseClock, "the action's time is before the record's last entry's")
	}
	e := Entry{N: s.Entries + 1, At: a.At.UTC(), Kind: a.Kind, By: a.By, Reason: a.Reason, Run: env.RunID()}
	res := s.Record.Result
	e.Result = &res
	if err = fillEntry(ctx, o, sha, a, &s, &e); err != nil {
		return nil, err
	}
	op, err := putNew(env, EntryKey(a.Record, e.N), e)
	if err != nil {
		return nil, err
	}
	return []store.Op{op}, nil
}

// fillEntry completes the entry for the action's kind, or refuses it.
func fillEntry(ctx context.Context, o objects, sha string, a *Action, s *State, e *Entry) error {
	switch a.Kind {
	case Correct, Manual:
		if err := reverify(ctx, o, sha, a, s.Record.Origin, e); err != nil {
			return err
		}
		if e.Change = diff(s.Record.Record, e.Record); e.Change == "" && a.Kind == Correct {
			return refuse(RefuseNoChange, "the correction changes nothing")
		}
		if e.Change == "" {
			e.Change = "entered by hand, unchanged"
		}
	case AcceptWarning:
		if err := checkAcceptance(s, a.Finding); err != nil {
			return err
		}
		e.Finding = a.Finding
	case SetEntities:
		return setEntities(s, a, e)
	case Approve:
		return approve(s, a, e)
	}
	return nil
}

// reverify decodes correct's or manual's record and verifies it against the
// stored document (ADR 0021, items 7 and 10): a person's record, or a
// correction of one, has its failed checks as warnings.
func reverify(ctx context.Context, o objects, sha string, a *Action, origin string, e *Entry) error {
	rec, err := statement.DecodeBytes(a.Extraction)
	if err != nil {
		return refuse(RefuseMalformed, "%v", err)
	}
	doc, err := storedDocument(ctx, o, sha)
	if err != nil {
		return err
	}
	res := verify.Verify(doc, rec)
	if a.Kind == Manual || origin == export.OriginManual {
		res = res.AsWarnings()
	}
	e.Record, e.Result = rec, &res
	return nil
}

// storedDocument reads the document with its pages' text; a document whose
// text was not read keeps pages without text, which grounding reports.
func storedDocument(ctx context.Context, o objects, sha string) (*document.Document, error) {
	var d intake.Document
	ok, err := readJSON(ctx, o, intake.DocumentKey(sha), &d)
	switch {
	case err != nil:
		return nil, err
	case !ok || d.Document == nil:
		return nil, refuse(RefuseNoDocument, "document %s is not in the store", sha)
	}
	if doc, err := d.Doc(); err == nil {
		return doc, nil
	}
	if len(d.Text) != 0 {
		return nil, fmt.Errorf("review: document %s: its text does not match its pages", sha)
	}
	return d.Document, nil
}

func checkAcceptance(s *State, finding string) error {
	var found bool
	for _, f := range s.Record.Result.Findings(verify.Warning) {
		if f.ID() == finding {
			found = true
		}
	}
	if !found {
		return refuse(RefuseNoSuchWarning, "the record has no current warning %q", finding)
	}
	for i := len(s.Record.Reviews) - 1; i >= 0; i-- {
		r := s.Record.Reviews[i]
		if r.Kind == export.Correct || r.Kind == export.Manual {
			break
		}
		if r.Kind == export.AcceptWarning && r.Finding == finding {
			return refuse(RefuseAccepted, "warning %q is already accepted", finding)
		}
	}
	return nil
}

func setEntities(s *State, a *Action, e *Entry) error {
	for _, ent := range []*export.Entity{a.Owner, a.Asset} {
		if ent == nil {
			continue
		}
		if ent.ID != strings.TrimSpace(ent.ID) || ent.Name != strings.TrimSpace(ent.Name) || ent.ID+ent.Name == "" {
			return refuse(RefuseInvalidEntity, "an owner or asset is an ID, an exact Addepar name or both, without surrounding spaces")
		}
	}
	if sameEntity(a.Owner, s.Record.Owner) && sameEntity(a.Asset, s.Record.Asset) {
		return refuse(RefuseNoChange, "the owner and asset are already these")
	}
	e.Owner, e.Asset = a.Owner, a.Asset
	e.Change = fmt.Sprintf("owner %s, asset %s", entityText(a.Owner), entityText(a.Asset))
	return nil
}

func sameEntity(a, b *export.Entity) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func entityText(e *export.Entity) string {
	switch {
	case e == nil:
		return "cleared"
	case e.ID != "" && e.Name != "":
		return fmt.Sprintf("%s (%s)", e.ID, e.Name)
	case e.ID != "":
		return e.ID
	}
	return fmt.Sprintf("%q", e.Name)
}

// approve stores the snapshot, the record as it stands with the approval,
// after the exporter's gate passes it (ADR 0021, item 7).
func approve(s *State, a *Action, e *Entry) error {
	if s.Approved() {
		return refuse(RefuseApproved, "the record is already approved")
	}
	snap := s.Record
	snap.Approval = &export.Approval{By: a.By, At: a.At.UTC()}
	if problems := export.Gate(&snap); len(problems) > 0 {
		return refuse(RefuseNotApprovable, "the record cannot be approved: %s", strings.Join(problems, "; "))
	}
	sum, err := export.SnapshotSHA256(&snap)
	if err != nil {
		return err //nolint:wrapcheck // export names it
	}
	e.Snapshot, e.SnapshotSHA256 = &snap, sum
	return nil
}

// applyNewManual adds a record a person read off the page to a document:
// the record, at the next number, and its log's first entry (ADR 0021,
// item 7).
func applyNewManual(ctx context.Context, env *agent.Env, o objects, a *Action) ([]store.Op, error) {
	sha := a.Document
	if !sha256Pattern.MatchString(sha) {
		return nil, refuse(RefuseMalformed, "document %q is not a SHA-256", sha)
	}
	if env.Instance() != DocumentInstance(sha) {
		return nil, refuse(RefuseWrongInstance, "document %s is not this instance's", sha)
	}
	keys, err := o.list(ctx, intake.RecordPrefix(sha))
	if err != nil {
		return nil, err
	}
	if a.Seen != len(keys) {
		return nil, refuse(RefuseStale, "the document has %d records, the action saw %d: reload it", len(keys), a.Seen)
	}
	var d intake.Document
	ok, err := readJSON(ctx, o, intake.DocumentKey(sha), &d)
	switch {
	case err != nil:
		return nil, err
	case !ok || d.Document == nil:
		// A manual record needs its document's source; never act on a
		// document that is not there (a well-formed but unknown SHA).
		return nil, refuse(RefuseNoDocument, "document %s is not in the store", sha)
	}
	id := intake.RecordID(sha, len(keys)+1)
	e := Entry{N: 1, At: a.At.UTC(), Kind: Manual, By: a.By, Reason: a.Reason, Run: env.RunID(), Change: "entered by hand"}
	if err = reverify(ctx, o, sha, a, export.OriginManual, &e); err != nil {
		return nil, err
	}
	base := export.Record{ID: id, Source: export.SourceOf(d.Document), Record: e.Record, Result: *e.Result, Origin: export.OriginManual}
	rop, err := putNew(env, intake.RecordKey(id), base)
	if err != nil {
		return nil, err
	}
	eop, err := putNew(env, EntryKey(id, 1), e)
	if err != nil {
		return nil, err
	}
	return []store.Op{rop, eop}, nil
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// applyExport exports the named records' current approvals' snapshots
// (ADR 0021, item 9).
func applyExport(ctx context.Context, env *agent.Env, o objects, a *Action) ([]store.Op, error) {
	if !strings.HasPrefix(env.Instance(), ExportInstancePrefix) {
		return nil, refuse(RefuseWrongInstance, "an export runs on its own %s instance", ExportInstancePrefix)
	}
	if len(a.Records) == 0 {
		return nil, refuse(RefuseIncomplete, "an export names its records")
	}
	seen := map[string]bool{}
	snaps := make([]export.Record, 0, len(a.Records))
	out := Exported{At: a.At.UTC(), By: a.By, Run: env.RunID()}
	var unapproved []string
	for _, id := range a.Records {
		if _, _, ok := SplitRecordID(id); !ok || seen[id] {
			return nil, refuse(RefuseMalformed, "record %q is not <sha256>/<NN>, or is named twice", id)
		}
		seen[id] = true
		s, _, err := load(ctx, o, id)
		switch {
		case errors.Is(err, errNoRecord):
			return nil, refuse(RefuseNoRecord, "record %s does not exist", id)
		case err != nil:
			return nil, err
		}
		if !s.Approved() {
			unapproved = append(unapproved, id)
			continue
		}
		sum, err := export.SnapshotSHA256(s.Snapshot)
		if err != nil {
			return nil, err //nolint:wrapcheck // export names it
		}
		snaps = append(snaps, *s.Snapshot)
		out.Records = append(out.Records, ExportedRecord{ID: id, Approval: s.Approval, SnapshotSHA256: sum})
	}
	if len(unapproved) > 0 {
		return nil, refuse(RefuseNotApproved, "not approved: %s", strings.Join(unapproved, ", "))
	}
	exp, err := export.Build(snaps, a.At)
	if errors.Is(err, export.ErrRefused) {
		return nil, refuse(RefuseExportRefused, "%v", err)
	}
	if err != nil {
		return nil, err //nolint:wrapcheck // export names it
	}
	out.Export = exp
	op, err := putNew(env, ExportKey(env.RunID()), out)
	if err != nil {
		return nil, err
	}
	return []store.Op{op}, nil
}

// putNew builds an operation writing v at key, which must not exist.
func putNew(env *agent.Env, key string, v any) (store.Op, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return store.Op{}, fmt.Errorf("review: encode %s: %w", key, err)
	}
	return env.PutJSON(key, 0, raw), nil
}

// maxChanges bounds the paths a change lists.
const maxChanges = 20

// diff describes how b differs from a, field by field ("ending.amount:
// 714760.65 -> 714760.66"), or "" when they are the same.
func diff(a, b *statement.Record) string {
	fa, fb := flatten(a), flatten(b)
	keys := map[string]bool{}
	for k := range fa {
		keys[k] = true
	}
	for k := range fb {
		keys[k] = true
	}
	var changed []string
	for k := range keys {
		if fa[k] != fb[k] {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	parts := make([]string, 0, min(len(changed), maxChanges+1))
	for i, k := range changed {
		if i == maxChanges {
			parts = append(parts, fmt.Sprintf("and %d more", len(changed)-maxChanges))
			break
		}
		parts = append(parts, fmt.Sprintf("%s: %s -> %s", k, show(fa, k), show(fb, k)))
	}
	return strings.Join(parts, "; ")
}

func show(m map[string]string, k string) string {
	if v, ok := m[k]; ok {
		return v
	}
	return "(none)"
}

// flatten maps each leaf of r's JSON to its value, by dotted path.
func flatten(r *statement.Record) map[string]string {
	out := map[string]string{}
	if r == nil {
		return out
	}
	b, err := json.Marshal(r)
	if err != nil {
		return out
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return out
	}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, c := range t {
				walk(join(prefix, k), c)
			}
		case []any:
			for i, c := range t {
				walk(join(prefix, fmt.Sprint(i+1)), c)
			}
		default:
			s, _ := json.Marshal(t)
			out[prefix] = string(s)
		}
	}
	walk("", v)
	return out
}

func join(prefix, k string) string {
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}
