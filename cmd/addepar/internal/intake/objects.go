package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/document"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/export"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// The object keys, in the run's namespace (ADR 0018). Every key holds the
// document's SHA-256, and every object is written once (expected version
// zero), so a write that would replace one conflicts instead.

// DocumentKey is the stored document's key.
func DocumentKey(sha string) string { return "documents/" + sha }

// PDFKey is the key of the document's PDF bytes, as received (ADR 0019).
func PDFKey(sha string) string { return "pdfs/" + sha }

// OutcomeKey is the document's outcome's key.
func OutcomeKey(sha string) string { return "outcomes/" + sha }

// AttemptPrefix lists the document's extraction attempts.
func AttemptPrefix(sha string) string { return "attempts/" + sha + "/" }

// AttemptKey is attempt n's key, n counted from 1.
func AttemptKey(sha string, n int) string { return fmt.Sprintf("%s%02d", AttemptPrefix(sha), n) }

// RecordPrefix lists the document's valuation records.
func RecordPrefix(sha string) string { return "records/" + sha + "/" }

// RecordID is the document's record k's identifier and key suffix, k counted
// from 1.
func RecordID(sha string, k int) string { return fmt.Sprintf("%s/%02d", sha, k) }

// RecordKey is a record's key.
func RecordKey(id string) string { return "records/" + id }

// Document is the stored document: what document.Load read, with each page's
// layout-mode text, which Document's JSON leaves out.
type Document struct {
	// Received is when the run loaded it.
	Received time.Time          `json:"received"`
	Document *document.Document `json:"document"`
	// Run is the run that loaded it.
	Run string `json:"run"`
	// Reading is how get_document gives the model the statement (ADR
	// 0019): ReadingPDFAndText, or ReadingTextOnly with ReadingNote saying
	// why.
	Reading     string `json:"reading"`
	ReadingNote string `json:"reading_note,omitempty"`
	// Text is each page's text in layout mode, in page order; empty when
	// the text was not read.
	Text []string `json:"text,omitempty"`
}

// How the model reads a statement (ADR 0019).
const (
	// ReadingPDFAndText: get_document returns the PDF itself beside its
	// layout-mode text.
	ReadingPDFAndText = "pdf_and_text"
	// ReadingTextOnly: the PDF is over the size the model is sent, so
	// get_document returns the layout-mode text alone.
	ReadingTextOnly = "text_only"
)

// The largest PDF sent to the model (ADR 0019): within the providers' PDF
// limits (Anthropic's 100 pages and 32 MB request) with room for the
// base64 encoding, the text and the conversation.
const (
	MaxPDFPages = 100
	MaxPDFBytes = 20 << 20
)

// reading decides how the model reads a PDF of the given page count and
// size.
func reading(pages, size int) (mode, note string) {
	switch {
	case pages > MaxPDFPages:
		return ReadingTextOnly, fmt.Sprintf("the PDF has %d pages, over the %d sent to the model", pages, MaxPDFPages)
	case size > MaxPDFBytes:
		return ReadingTextOnly, fmt.Sprintf("the PDF is %d bytes, over the %d sent to the model", size, MaxPDFBytes)
	}
	return ReadingPDFAndText, ""
}

// Doc returns the document with its pages' text restored.
func (d *Document) Doc() (*document.Document, error) {
	if d.Document == nil {
		return nil, errors.New("intake: stored document has no document")
	}
	if len(d.Text) != len(d.Document.Pages) {
		return nil, fmt.Errorf("intake: stored document has %d pages of text for %d pages", len(d.Text), len(d.Document.Pages))
	}
	doc := *d.Document
	doc.Pages = make([]document.Page, len(d.Document.Pages))
	for i, p := range d.Document.Pages {
		p.Text = d.Text[i]
		doc.Pages[i] = p
	}
	return &doc, nil
}

// The dispositions an outcome records (intake: a run ends when every
// statement is verified, set aside or sent to manual review).
const (
	DispositionVerified = "verified"
	DispositionSetAside = "set_aside"
	DispositionManual   = "manual_review"
)

// Outcome is what became of a document: its disposition, why, and the
// records it produced.
type Outcome struct {
	At          time.Time `json:"at"`
	Disposition string    `json:"disposition"`
	// DocumentType is a set-aside document's type.
	DocumentType string `json:"document_type,omitempty"`
	// Reason is the extractor's reason for a set-aside or manual review,
	// or the code's (no text layer, attempts spent).
	Reason string `json:"reason,omitempty"`
	Run    string `json:"run"`
	// Records are the verified records' IDs.
	Records []string `json:"records,omitempty"`
	// Attempts is how many submissions were made.
	Attempts int `json:"attempts"`
}

// Attempt is one submission, stored as submitted with what code found
// (intake N-2): the model, prompt and transcript behind it are in the run's
// execution record at Run, Step and Call.
type Attempt struct {
	At  time.Time `json:"at"`
	Run string    `json:"run"`
	// Submitted is the tool call's arguments, as the model sent them.
	Submitted json.RawMessage `json:"submitted"`
	// Problem is why the submission as a whole could not be read.
	Problem string        `json:"problem,omitempty"`
	Verdict verify.Status `json:"verdict"`
	// Reading is how the model read the statement (ADR 0019).
	Reading string          `json:"reading"`
	Records []AttemptRecord `json:"records,omitempty"`
	Step    uint64          `json:"step"`
	Call    int             `json:"call"`
	N       int             `json:"n"`
	WrapUp  bool            `json:"wrap_up,omitempty"`
}

// AttemptRecord is one record of a submission: decoded and verified, or the
// problem that kept it from being verified.
type AttemptRecord struct {
	Record  *statement.Record `json:"record,omitempty"`
	Result  *verify.Result    `json:"result,omitempty"`
	Problem string            `json:"problem,omitempty"`
}

// readJSON reads key into v, reporting whether it exists.
func readJSON(ctx context.Context, env *agent.Env, key string, v any) (bool, error) {
	obj, ok, err := env.Object(ctx, key)
	if err != nil || !ok {
		return ok, err //nolint:wrapcheck // a *agent.ReadError, which the runtime classifies
	}
	if err := json.Unmarshal(obj.JSON, v); err != nil {
		return true, fmt.Errorf("intake: decode %s: %w", key, err)
	}
	return true, nil
}

// putNew builds an operation writing v at key, which must not exist.
func putNew(env *agent.Env, key string, v any) (store.Op, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return store.Op{}, fmt.Errorf("intake: encode %s: %w", key, err)
	}
	return env.PutJSON(key, 0, raw), nil
}

// storedRecord is the valuation record as the exporter reads it.
func storedRecord(id string, src export.Source, rec *statement.Record, res verify.Result) export.Record {
	return export.Record{ID: id, Source: src, Record: rec, Result: res}
}
