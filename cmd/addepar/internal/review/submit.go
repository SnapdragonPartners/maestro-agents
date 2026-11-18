package review

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/export"
	"github.com/SnapdragonPartners/maestro-agents/standalone"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Submitter runs a submitted trigger to its end: a *standalone.Host.
type Submitter interface {
	Submit(ctx context.Context, s standalone.Submission) (standalone.Report, error)
}

// RefusedError is an action the review run refused: the person's to act
// on, never the system's failure (ADR 0021, item 5).
type RefusedError struct {
	Refusal Refusal
}

func (e *RefusedError) Error() string {
	return "refused: " + e.Refusal.Code + ": " + e.Refusal.Message
}

// Instance is the instance an action runs on: its document's, or for an
// export a new export instance.
func Instance(a *Action) (string, error) {
	switch {
	case a.Kind == Export:
		var b [8]byte
		_, _ = rand.Read(b[:])
		return ExportInstancePrefix + hex.EncodeToString(b[:]), nil
	case a.Record != "":
		sha, _, ok := SplitRecordID(a.Record)
		if !ok {
			return "", fmt.Errorf("review: record %q is not <sha256>/<NN>", a.Record)
		}
		return DocumentInstance(sha), nil
	case sha256Pattern.MatchString(a.Document):
		return DocumentInstance(a.Document), nil
	}
	return "", fmt.Errorf("review: the action names no record or document")
}

// Submit runs the action on its instance and returns its run: a
// *RefusedError when the review run refused it, standalone.ErrBusy (wrapped)
// when the instance is busy, and an error when the run has no confirmed
// outcome.
func Submit(ctx context.Context, sub Submitter, objs StoreObjects, a *Action) (string, error) {
	instance, err := Instance(a)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(a)
	if err != nil {
		return "", fmt.Errorf("review: encode the action: %w", err)
	}
	rep, err := sub.Submit(ctx, standalone.Submission{AgentType: TypeName, Instance: instance, Payload: payload})
	if err != nil {
		return "", err //nolint:wrapcheck // standalone names it
	}
	return rep.Run, Outcome(ctx, objs, &rep)
}

// Outcome turns a review run's report into nil when it applied, a
// *RefusedError when it refused, and an error otherwise.
func Outcome(ctx context.Context, objs StoreObjects, rep *standalone.Report) error {
	switch {
	case rep.Error != "":
		return fmt.Errorf("run %s has no confirmed outcome: %s", rep.Run, rep.Error)
	case rep.Status == store.StatusSucceeded && rep.Reason.Code == agent.ReasonChanged:
		return nil
	case rep.Status == store.StatusSucceeded && rep.Reason.Code == agent.ReasonNoChange:
		var r Refusal
		if ok, err := readJSON(ctx, objs, RefusalKey(rep.Run), &r); err != nil || !ok {
			return &RefusedError{Refusal: Refusal{Run: rep.Run, Code: "unknown", Message: "its refusal cannot be read"}}
		}
		return &RefusedError{Refusal: r}
	}
	return fmt.Errorf("run %s ended %s (%s)", rep.Run, rep.Status, rep.Reason.Code)
}

// SubmitExport exports the records' current approvals (ADR 0021, item 9)
// and returns what the run stored; the caller writes the files.
func SubmitExport(ctx context.Context, sub Submitter, objs StoreObjects, by string, at time.Time, ids []string) (*Exported, error) {
	run, err := Submit(ctx, sub, objs, &Action{Kind: Export, By: by, At: at.UTC(), Records: ids})
	if err != nil {
		return nil, err
	}
	return StoredExport(ctx, objs, run)
}

// ErrNoExport is StoredExport's error for a run that stored no export.
var ErrNoExport = errors.New("review: no such export")

// StoredExport reads the export a run stored.
func StoredExport(ctx context.Context, objs StoreObjects, run string) (*Exported, error) {
	var exp Exported
	ok, err := readJSON(ctx, objs, ExportKey(run), &exp)
	switch {
	case err != nil:
		return nil, fmt.Errorf("read the export of run %s: %w", run, err)
	case !ok:
		return nil, fmt.Errorf("%w: run %s", ErrNoExport, run)
	case exp.Export == nil:
		return nil, fmt.Errorf("decode the export of run %s: no export", run)
	}
	return &exp, nil
}

// Unexported lists the approved records whose current approval no export
// holds, in key order.
func Unexported(ctx context.Context, objs StoreObjects) ([]string, error) {
	all, err := Records(ctx, objs)
	if err != nil {
		return nil, fmt.Errorf("list records: %w", err)
	}
	exports, err := Exports(ctx, objs)
	if err != nil {
		return nil, fmt.Errorf("list exports: %w", err)
	}
	done := ExportedApprovals(exports)
	var ids []string
	for _, id := range all {
		s, err := Load(ctx, objs, id)
		if err != nil {
			return nil, fmt.Errorf("record %s: %w", id, err)
		}
		if s.Approved() && !done[id][s.Approval] {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// Blockers returns why the record cannot be approved as it stands (the
// exporter's gate, as approve applies it), or nothing when it can, or is
// approved.
func Blockers(s *State) []string {
	if s.Approved() {
		return nil
	}
	snap := s.Record
	snap.Approval = &export.Approval{By: "check", At: time.Unix(0, 0).UTC()}
	if len(snap.Reviews) > 0 {
		snap.Approval.At = snap.Reviews[len(snap.Reviews)-1].At
	}
	return export.Gate(&snap)
}

// Busy reports whether err is a busy instance's: retry later.
func Busy(err error) bool { return errors.Is(err, standalone.ErrBusy) }

// Refused returns the refusal err carries, if any.
func Refused(err error) (*Refusal, bool) {
	var r *RefusedError
	if errors.As(err, &r) {
		return &r.Refusal, true
	}
	return nil, false
}
