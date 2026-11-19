package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/export"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/intake"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/review"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// The states a document or record shows, as CSS classes.
const (
	kindWorking   = "working"
	kindAttention = "attention"
	kindReady     = "ready"
	kindDone      = "done"
	kindFailed    = "failed"
)

// The record states (ADR 0022, item 10).
const (
	StateNeedsReview = "needs_review"
	StateApprovable  = "approvable"
	StateApproved    = "approved"
	StateExported    = "exported"
)

// DocumentView is a document as the UI shows it.
type DocumentView struct {
	Received time.Time    `json:"received,omitzero"`
	Outcome  *OutcomeView `json:"outcome,omitempty"`
	// SHA256 is empty for a file in the inbox the agent has not read.
	SHA256 string `json:"sha256,omitempty"`
	Name   string `json:"name"`
	// State is the document's state: queued, reading, verified, set_aside,
	// manual_review or failed; Label says it for a person.
	State   string       `json:"state"`
	Label   string       `json:"label"`
	Kind    string       `json:"-"`
	Detail  string       `json:"detail,omitempty"`
	Records []RecordView `json:"records"`
	Pages   int          `json:"pages,omitempty"`
	// TextRead reports whether the document's text was read, which
	// grounding and the evidence boxes need.
	TextRead bool `json:"text_read"`
}

// OutcomeView is the intake run's outcome.
type OutcomeView struct {
	Disposition  string `json:"disposition"`
	DocumentType string `json:"document_type,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Attempts     int    `json:"attempts"`
}

// RecordView is a record's summary.
type RecordView struct {
	ID       string `json:"id"`
	SHA256   string `json:"sha256"`
	N        string `json:"n"`
	Investor string `json:"investor"`
	Fund     string `json:"fund"`
	Period   string `json:"period"`
	Value    string `json:"value"`
	Currency string `json:"currency"`
	Origin   string `json:"origin"`
	State    string `json:"state"`
	Label    string `json:"label"`
	Kind     string `json:"-"`
}

// recordState names a record's state and its label.
func recordState(s *review.State, exported bool) (state, label, kind string) {
	switch {
	case s.Approved() && exported:
		return StateExported, "Exported", kindDone
	case s.Approved():
		return StateApproved, "Approved", kindReady
	case len(review.Blockers(s)) == 0:
		return StateApprovable, "Ready to approve", kindAttention
	}
	return StateNeedsReview, "Needs review", kindAttention
}

func summarize(id string, s *review.State, exported bool) RecordView {
	sha, n, _ := review.SplitRecordID(id)
	v := RecordView{ID: id, SHA256: sha, N: fmt.Sprintf("%02d", n), Origin: s.Record.Origin}
	if v.Origin == "" {
		v.Origin = export.OriginExtracted
	}
	if r := s.Record.Record; r != nil {
		v.Investor, v.Fund, v.Period = r.Investor.Text, r.Fund.Text, r.Period.End
		v.Value, v.Currency = formatAmount(r.Ending.Amount.String()), r.Currency.Code
	}
	v.State, v.Label, v.Kind = recordState(s, exported)
	return v
}

// formatAmount groups a decimal string's integer digits by thousands.
func formatAmount(s string) string {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, hasFrac := strings.Cut(s, ".")
	var b strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := b.String()
	if hasFrac {
		out += "." + frac
	}
	if neg {
		out = "(" + out + ")"
	}
	return out
}

// errNotFound is a request for a document or record that does not exist.
var errNotFound = errors.New("not found")

// catalog reads what the documents and records pages show.
type catalog struct {
	reader   store.Reader
	exported map[string]map[int]bool
	objs     review.StoreObjects
}

func (s *Server) catalog(ctx context.Context) (*catalog, error) {
	objs := s.objects()
	exports, err := review.Exports(ctx, objs)
	if err != nil {
		return nil, err //nolint:wrapcheck // review names it
	}
	return &catalog{objs: objs, reader: objs.Reader, exported: review.ExportedApprovals(exports)}, nil
}

// document reads one stored document, its outcome and records.
func (c *catalog) document(ctx context.Context, sha string) (*DocumentView, *intake.Document, error) {
	var d intake.Document
	ok, err := readObject(ctx, c.objs, intake.DocumentKey(sha), &d)
	switch {
	case err != nil:
		return nil, nil, err
	case !ok || d.Document == nil:
		return nil, nil, errNotFound
	}
	v := &DocumentView{SHA256: sha, Name: d.Document.Name, Received: d.Received, Pages: d.Document.PageCount, TextRead: len(d.Text) > 0}
	var o intake.Outcome
	if ok, err = readObject(ctx, c.objs, intake.OutcomeKey(sha), &o); err != nil {
		return nil, nil, err
	}
	if ok {
		v.Outcome = &OutcomeView{Disposition: o.Disposition, DocumentType: o.DocumentType, Reason: o.Reason, Attempts: o.Attempts}
	}
	run, err := c.reader.Run(ctx, review.DocumentInstance(sha))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, nil, err //nolint:wrapcheck // the store names it
	}
	documentState(v, &run, err == nil)
	keys, err := c.reader.ListObjects(ctx, c.objs.Namespace, intake.RecordPrefix(sha))
	if err != nil {
		return nil, nil, err //nolint:wrapcheck // the store names it
	}
	v.Records = []RecordView{}
	for _, k := range keys {
		id := strings.TrimPrefix(k.Key, "records/")
		if _, _, ok := review.SplitRecordID(id); !ok {
			continue
		}
		st, err := review.Load(ctx, c.objs, id)
		if err != nil {
			return nil, nil, err //nolint:wrapcheck // review names it
		}
		v.Records = append(v.Records, summarize(id, &st, c.exported[id][st.Approval]))
	}
	return v, &d, nil
}

// documentState sets a stored document's state from its outcome, or its
// run's status while it has none.
func documentState(v *DocumentView, run *store.Run, haveRun bool) {
	switch {
	case v.Outcome != nil && v.Outcome.Disposition == intake.DispositionVerified:
		v.State, v.Label, v.Kind = intake.DispositionVerified, "Read and verified", kindReady
	case v.Outcome != nil && v.Outcome.Disposition == intake.DispositionSetAside:
		v.State, v.Label, v.Kind = intake.DispositionSetAside, "Set aside", kindDone
		v.Detail = strings.TrimSpace(v.Outcome.DocumentType + ": " + v.Outcome.Reason)
	case v.Outcome != nil:
		v.State, v.Label, v.Kind = intake.DispositionManual, "Needs manual entry", kindAttention
		v.Detail = v.Outcome.Reason
	case haveRun && run.Status == store.StatusFailed:
		v.State, v.Label, v.Kind = stateFailed, "The agent could not finish", kindFailed
		v.Detail = failureDetail(run)
	case haveRun && run.Status.Terminal():
		v.State, v.Label, v.Kind = stateFailed, "Stopped", kindFailed
	default:
		v.State, v.Label, v.Kind = stateReading, "Being read", kindWorking
	}
}

// The states of a document without an outcome.
const (
	stateQueued  = "queued"
	stateReading = "reading"
	stateFailed  = "failed"
)

func failureDetail(run *store.Run) string {
	d := run.Reason.Code
	if run.Reason.Budget != "" {
		d += " (" + run.Reason.Budget + ")"
	}
	return d
}

// documents lists every document, newest first: the stored ones, and the
// inbox's files the agent has not stored yet.
func (s *Server) documents(ctx context.Context) ([]DocumentView, error) {
	c, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	keys, err := c.reader.ListObjects(ctx, c.objs.Namespace, "documents/")
	if err != nil {
		return nil, err //nolint:wrapcheck // the store names it
	}
	stored := map[string]bool{}
	out := make([]DocumentView, 0, len(keys))
	for _, k := range keys {
		sha := strings.TrimPrefix(k.Key, "documents/")
		v, _, derr := c.document(ctx, sha)
		if errors.Is(derr, errNotFound) {
			continue
		}
		if derr != nil {
			return nil, derr
		}
		stored[sha] = true
		out = append(out, *v)
	}
	waiting, err := s.inboxWaiting(ctx, c.reader, stored)
	if err != nil {
		return nil, err
	}
	out = append(out, waiting...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Received.Equal(out[j].Received) {
			return out[i].Name < out[j].Name
		}
		return out[i].Received.After(out[j].Received)
	})
	return out, nil
}

// working reports whether any document is still queued or being read.
func working(docs []DocumentView) bool {
	for i := range docs {
		if docs[i].Kind == kindWorking {
			return true
		}
	}
	return false
}

// RecordDetail is one record as its page shows it (R-1).
type RecordDetail struct {
	Document *DocumentView
	Record   *statement.Record
	Summary  RecordView
	Owner    export.Entity
	Asset    export.Entity
	// Blockers are why it cannot be approved yet.
	Blockers []string
	Findings []FindingView
	Checks   []verify.CheckResult
	Fields   []FieldView
	Pages    []PageView
	Log      []review.Entry
	Form     RecordForm
	State    review.State
	// CorrectSeen is the correction form's seen: the log's length, or a
	// draft's own; CorrectOpen shows the form open.
	CorrectSeen int
	CorrectOpen bool
}

// FindingView is a finding with whether a person has accepted it.
type FindingView struct {
	ID string
	verify.Finding
	Accepted bool
}

// accepted returns the warnings accepted since the latest correction or
// manual entry (ADR 0021, item 8).
func accepted(r *export.Record) map[string]bool {
	out := map[string]bool{}
	for i := len(r.Reviews) - 1; i >= 0; i-- {
		a := r.Reviews[i]
		if a.Kind == export.Correct || a.Kind == export.Manual {
			break
		}
		if a.Kind == export.AcceptWarning {
			out[a.Finding] = true
		}
	}
	return out
}

func (s *Server) record(ctx context.Context, sha, nn string) (*RecordDetail, error) {
	id := sha + "/" + nn
	if _, _, ok := review.SplitRecordID(id); !ok {
		return nil, errNotFound
	}
	c, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	doc, stored, err := c.document(ctx, sha)
	if err != nil {
		return nil, err
	}
	st, log, err := review.LoadLog(ctx, c.objs, id)
	if err != nil {
		if _, ok, rerr := c.reader.Object(ctx, c.objs.Namespace, intake.RecordKey(id)); rerr == nil && !ok {
			return nil, errNotFound
		}
		return nil, err //nolint:wrapcheck // review names it
	}
	d := &RecordDetail{
		Summary: summarize(id, &st, c.exported[id][st.Approval]), Document: doc, State: st,
		Record: st.Record.Record, Blockers: review.Blockers(&st), Checks: st.Record.Result.Checks, Log: log,
	}
	if st.Record.Owner != nil {
		d.Owner = *st.Record.Owner
	}
	if st.Record.Asset != nil {
		d.Asset = *st.Record.Asset
	}
	acc := accepted(&st.Record)
	for _, sev := range []verify.Severity{verify.Error, verify.Warning} {
		for _, f := range st.Record.Result.Findings(sev) {
			d.Findings = append(d.Findings, FindingView{Finding: f, ID: f.ID(), Accepted: sev == verify.Warning && acc[f.ID()]})
		}
	}
	d.Fields = fields(d.Record)
	d.Form, d.CorrectSeen = formOf(d.Record), st.Entries
	d.Pages = s.evidencePages(ctx, sha, stored, d.Fields)
	return d, nil
}

func readObject(ctx context.Context, objs review.StoreObjects, key string, v any) (bool, error) {
	o, ok, err := objs.Reader.Object(ctx, objs.Namespace, key)
	if err != nil || !ok {
		return ok, err //nolint:wrapcheck // the store names it
	}
	if err := json.Unmarshal(o.JSON, v); err != nil {
		return true, fmt.Errorf("decode %s: %w", key, err)
	}
	return true, nil
}
