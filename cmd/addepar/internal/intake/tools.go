package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/decimal"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/document"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/export"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/strictjson"
)

// prompt renders the extraction state's request from the type's templates.
func (a *intake) prompt(_ context.Context, env *agent.Env, d Data) (llms.ChatRequest, error) {
	sys, err := env.Render("system.tmpl", d)
	if err != nil {
		return llms.ChatRequest{}, fmt.Errorf("intake: system prompt: %w", err)
	}
	task, err := env.Render("task.tmpl", d)
	if err != nil {
		return llms.ChatRequest{}, fmt.Errorf("intake: task prompt: %w", err)
	}
	return llms.ChatRequest{
		System:    []llms.ContentPart{llms.Text(sys)},
		Messages:  []llms.Message{llms.UserText(task)},
		MaxTokens: 16_000,
	}, nil
}

// readDocument reads the run's stored document.
func readDocument(ctx context.Context, env *agent.Env, sha string) (*document.Document, error) {
	var stored Document
	ok, err := readJSON(ctx, env, DocumentKey(sha), &stored)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("intake: document %s is not stored", sha)
	}
	return stored.Doc()
}

func (a *intake) getDocument() agent.Tool[Data] {
	return agent.Tool[Data]{
		Definition: llms.ToolDefinition{
			Name:        toolGetDocument,
			Description: "Read the statement: the PDF itself, to see its pages, and its page count and each page's text in layout mode, which keeps each printed line, and each label beside its numbers, on one line. Copy names, labels and amounts from the text.",
			InputSchema: json.RawMessage(emptySchema),
		},
		// An atomic local transaction whose one write is the marker that
		// the PDF was sent, so the conversation carries it once (ADR 0019,
		// item 2).
		Effect:   agent.EffectWrite,
		Contract: agent.ContractAtomicLocal,
		Execute: func(ctx context.Context, c *agent.Call[Data]) (agent.Result[Data], error) {
			var stored Document
			ok, err := readJSON(ctx, c.Env, DocumentKey(c.Data.Document), &stored)
			if err != nil {
				return agent.Result[Data]{}, err
			}
			if !ok {
				return agent.Result[Data]{}, fmt.Errorf("intake: document %s is not stored", c.Data.Document)
			}
			doc, err := stored.Doc()
			if err != nil {
				return agent.Result[Data]{}, err
			}
			text := documentText(doc, &stored)
			if stored.Reading != ReadingPDFAndText {
				return agent.TextResult[Data](text), nil
			}
			// The PDF goes into the conversation once: a later call, in the
			// same turn or after recovery, finds the marker its first call
			// committed with its result, and returns the text alone.
			var sent PDFSent
			ok, err = readJSON(ctx, c.Env, PDFSentKey(c.Data.Document), &sent)
			if err != nil {
				return agent.Result[Data]{}, err
			}
			if ok {
				return agent.TextResult[Data](pdfAttachedEarlier + text), nil
			}
			pdf, ok, err := c.Env.Object(ctx, PDFKey(c.Data.Document))
			if err != nil {
				return agent.Result[Data]{}, err //nolint:wrapcheck // a *agent.ReadError, which the runtime classifies
			}
			if !ok || len(pdf.Bytes) == 0 {
				return agent.Result[Data]{}, fmt.Errorf("intake: PDF %s is not stored", c.Data.Document)
			}
			if document.Hash(pdf.Bytes) != c.Data.Document {
				return agent.Result[Data]{}, fmt.Errorf("intake: stored PDF %s does not match its hash", c.Data.Document)
			}
			sentOp, err := putNew(c.Env, PDFSentKey(c.Data.Document), &PDFSent{
				At: a.now(), Run: c.Env.RunID(), Step: c.Env.Step(), Call: c.Action.Call,
			})
			if err != nil {
				return agent.Result[Data]{}, err
			}
			return agent.Result[Data]{Parts: []llms.ContentPart{
				llms.Document(pdfMediaType, pdf.Bytes),
				llms.Text(text),
			}, Ops: []store.Op{sentOp}}, nil
		},
	}
}

// documentText is get_document's text: the page count, the text layer, why
// the PDF is not attached when it is not, and each page's layout text.
func documentText(doc *document.Document, stored *Document) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d page(s); text layer %s.\n", doc.PageCount, doc.TextLayer)
	if stored.Reading != ReadingPDFAndText {
		fmt.Fprintf(&b, "The PDF itself is not attached: %s. Read the text alone.\n", stored.ReadingNote)
	}
	for _, p := range doc.Pages {
		fmt.Fprintf(&b, "\n=== page %d ===\n", p.Number)
		if !p.Usable {
			b.WriteString("(no usable text on this page)\n")
			continue
		}
		b.WriteString(p.Text)
		if !strings.HasSuffix(p.Text, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// pdfAttachedEarlier heads a repeat get_document result.
const pdfAttachedEarlier = "The PDF itself was attached to your first get_document result, earlier in this conversation, and is not attached again.\n"

func (a *intake) submit() agent.Tool[Data] {
	return agent.Tool[Data]{
		Definition: llms.ToolDefinition{
			Name:        toolSubmit,
			Description: "Submit every extraction record in the document, one per investor and fund. Code verifies each against the PDF's text and returns the results; on any error it returns the reasons, and you may correct and resubmit all of them.",
			InputSchema: json.RawMessage(submitSchema),
		},
		Effect:   agent.EffectWrite,
		Contract: agent.ContractAtomicLocal,
		Execute:  a.submitExec,
	}
}

// errOutcome reports a document that already has an outcome: the run has
// ended its work, so nothing more may be written for it.
var errOutcome = errors.New("intake: the document already has an outcome")

// checkOpen fails when the document already has an outcome.
func checkOpen(ctx context.Context, env *agent.Env, sha string) error {
	_, ok, err := env.Object(ctx, OutcomeKey(sha))
	if err != nil {
		return err //nolint:wrapcheck // a *agent.ReadError, which the runtime classifies
	}
	if ok {
		return errOutcome
	}
	return nil
}

// submitExec verifies a submission and stores it as one atomic local
// transaction: the attempt always; with no error in any record, the records
// and the verified outcome, ending the run; with an error on the last
// attempt allowed, or in a wrap-up turn, the manual-review outcome, ending
// the run; otherwise the reasons, for the model to correct.
func (a *intake) submitExec(ctx context.Context, c *agent.Call[Data]) (agent.Result[Data], error) {
	d, env := c.Data, c.Env
	if err := checkOpen(ctx, env, d.Document); err != nil {
		return agent.Result[Data]{}, err
	}
	doc, err := readDocument(ctx, env, d.Document)
	if err != nil {
		return agent.Result[Data]{}, err
	}
	prior, err := env.ListObjects(ctx, AttemptPrefix(d.Document))
	if err != nil {
		return agent.Result[Data]{}, err //nolint:wrapcheck // a *agent.ReadError, which the runtime classifies
	}
	now := a.now()
	att := Attempt{
		N: len(prior) + 1, Run: env.RunID(), Step: env.Step(), Call: c.Action.Call, At: now,
		Submitted: c.ToolCall.Parameters, WrapUp: c.WrapUp, Reading: d.Reading,
	}
	verifySubmission(doc, &att)
	attOp, err := putNew(env, AttemptKey(d.Document, att.N), &att)
	if err != nil {
		return agent.Result[Data]{}, err
	}
	ops := []store.Op{attOp}
	report := describeAttempt(&att, d.MaxAttempts)
	switch {
	case att.Verdict != verify.Fail:
		out := Outcome{At: now, Disposition: DispositionVerified, Run: env.RunID(), Attempts: att.N}
		src := export.SourceOf(doc)
		for k, r := range att.Records {
			id := RecordID(d.Document, k+1)
			op, err := putNew(env, RecordKey(id), storedRecord(id, src, r.Record, *r.Result))
			if err != nil {
				return agent.Result[Data]{}, err
			}
			ops = append(ops, op)
			out.Records = append(out.Records, id)
		}
		outOp, err := putNew(env, OutcomeKey(d.Document), &out)
		if err != nil {
			return agent.Result[Data]{}, err
		}
		return agent.Result[Data]{Content: report, To: stateVerified, Data: d, Ops: append(ops, outOp)}, nil
	case att.N >= d.MaxAttempts || c.WrapUp:
		why := fmt.Sprintf("verification failed on attempt %d of %d", att.N, d.MaxAttempts)
		if c.WrapUp && att.N < d.MaxAttempts {
			why = fmt.Sprintf("verification failed on attempt %d, in the wrap-up near the run's budget", att.N)
		}
		out := Outcome{At: now, Disposition: DispositionManual, Reason: why, Run: env.RunID(), Attempts: att.N}
		outOp, err := putNew(env, OutcomeKey(d.Document), &out)
		if err != nil {
			return agent.Result[Data]{}, err
		}
		return agent.Result[Data]{Content: report + "\nThe statement is sent to manual review: " + why + ".", To: stateManual, Data: d, Ops: append(ops, outOp)}, nil
	}
	return agent.Result[Data]{Content: report, IsError: true, Ops: ops}, nil
}

// verifySubmission decodes and verifies the attempt's submission, filling
// in its records and verdict: fail if the submission or any record could not
// be read, any record may denote the same position as an earlier one
// (samePosition), or any has a verification error; else warn if any record
// has a warning; else pass.
func verifySubmission(doc *document.Document, att *Attempt) {
	recs, problem := decodeSubmission(att.Submitted)
	att.Problem = problem
	fail, warn := problem != "", false
	kept := map[int]*statement.Record{}
	for n, r := range recs {
		ar := AttemptRecord{Record: r.rec, Problem: r.problem}
		if r.rec != nil {
			for i := range n {
				if k := kept[i]; k != nil {
					if why := samePosition(k, r.rec); why != "" {
						ar.Problem = fmt.Sprintf("may be the same position as record %d (%s in %s): %s; submit one record per investor and fund", i+1, k.Investor.Text, k.Fund.Text, why)
						break
					}
				}
			}
			if ar.Problem == "" {
				kept[n] = r.rec
				res := verify.Verify(doc, r.rec)
				ar.Result = &res
			}
		}
		switch {
		case ar.Problem != "" || ar.Result.Verdict == verify.Fail:
			fail = true
		case ar.Result.Verdict == verify.Warn:
			warn = true
		}
		att.Records = append(att.Records, ar)
	}
	switch {
	case fail:
		att.Verdict = verify.Fail
	case warn:
		att.Verdict = verify.Warn
	default:
		att.Verdict = verify.Pass
	}
}

// samePosition reports why two records of one submission may denote the
// same position, or "" if they cannot (ADR 0018, item 4). It errs toward
// "same": two distinct positions it cannot tell apart are refused, failing
// closed into a correction or manual review, never stored twice.
//
// Names are no identity: grounding (check 1) accepts any contiguous run of
// a printed name's words, compared by verify.NameKey, so "Example Holdings
// IV" grounds wherever "Example Holdings IV, LLC" is printed. Two records
// are therefore the same position when each one's investor words run
// inside the other's, or contain them, and likewise their funds. Even that
// is not enough: on a table repeated across pages, disjoint fragments
// ("Example", "Holdings IV") each ground against the one printed name, and
// the page's words do not say where a name starts or ends, so a fragment
// cannot be resolved to a full identity. The value decides instead: it is
// what a record exports, so two records whose endings are within one unit
// of each other (a repeat may be printed rounded) are one balance, whatever
// names, labels and pages they claim.
func samePosition(a, b *statement.Record) string {
	if wordsOverlap(a.Investor.Text, b.Investor.Text) && wordsOverlap(a.Fund.Text, b.Fund.Text) {
		return "their investor and fund names are the same printed words, or one runs inside the other"
	}
	// One unit: the rounding a repeated balance may carry.
	if a.Ending.Amount.Sub(b.Ending.Amount).Abs().Cmp(decimal.MustParse("1")) <= 0 {
		return "their endings are the same balance, within rounding"
	}
	return ""
}

// wordsOverlap reports whether one name's normalized words are a
// contiguous run of the other's.
func wordsOverlap(a, b string) bool {
	ka, kb := " "+verify.NameKey(a)+" ", " "+verify.NameKey(b)+" "
	return strings.Contains(ka, kb) || strings.Contains(kb, ka)
}

type submitted struct {
	rec     *statement.Record
	problem string
}

// decodeSubmission reads the submission's records, each decoded and
// validated on its own so one malformed record does not hide the others'
// results. A problem with the submission as a whole is returned instead.
func decodeSubmission(raw json.RawMessage) ([]submitted, string) {
	var args struct {
		Records []json.RawMessage `json:"records"`
	}
	if msg := decodeArgs(raw, &args); msg != "" {
		return nil, msg
	}
	if len(args.Records) == 0 {
		return nil, "no records: submit one record per investor and fund"
	}
	out := make([]submitted, len(args.Records))
	for i, r := range args.Records {
		rec, err := statement.DecodeBytes(r)
		if err != nil {
			out[i].problem = err.Error()
			continue
		}
		out[i].rec = rec
	}
	return out, ""
}

// describeAttempt is the model's view of an attempt: each record's verdict,
// its errors with their evidence, and the warnings a person will review.
func describeAttempt(att *Attempt, maxAttempts int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Attempt %d of %d: %s.\n", att.N, maxAttempts, att.Verdict)
	if att.Problem != "" {
		fmt.Fprintf(&b, "The submission could not be read: %s\n", att.Problem)
	}
	for i, r := range att.Records {
		name := ""
		if r.Record != nil {
			name = fmt.Sprintf(" (%s in %s)", r.Record.Investor.Text, r.Record.Fund.Text)
		}
		if r.Problem != "" {
			fmt.Fprintf(&b, "Record %d%s is malformed: %s\n", i+1, name, r.Problem)
			continue
		}
		fmt.Fprintf(&b, "Record %d%s: %s.\n", i+1, name, r.Result.Verdict)
		for _, f := range r.Result.Findings(verify.Error) {
			b.WriteString("  error " + findingLine(&f) + "\n")
		}
		for _, f := range r.Result.Findings(verify.Warning) {
			b.WriteString("  warning (a person reviews it; not yours to fix unless it is wrong) " + findingLine(&f) + "\n")
		}
	}
	switch att.Verdict {
	case verify.Fail:
		if att.N < maxAttempts {
			b.WriteString("Correct the errors and resubmit every record, or call request_manual_review if you cannot.")
		}
	default:
		b.WriteString("The records are stored for a person's review.")
	}
	return b.String()
}

func findingLine(f *verify.Finding) string {
	s := fmt.Sprintf("[check %d %s, %s", f.Check, verify.CheckName(f.Check), f.Code)
	if f.Subject != "" {
		s += ", " + f.Subject
	}
	if f.Page > 0 {
		s += fmt.Sprintf(", page %d", f.Page)
	}
	return s + "] " + f.Message
}

func (a *intake) setAside() agent.Tool[Data] {
	return agent.Tool[Data]{
		Definition: llms.ToolDefinition{
			Name:        toolSetAside,
			Description: "Set the document aside because it is not a capital account statement (a capital call, distribution notice, K-1, cover letter or other). Ends the work on it.",
			InputSchema: json.RawMessage(setAsideSchema),
		},
		Effect:   agent.EffectWrite,
		Contract: agent.ContractAtomicLocal,
		Execute: func(ctx context.Context, c *agent.Call[Data]) (agent.Result[Data], error) {
			var args struct {
				Type   string `json:"document_type"`
				Reason string `json:"reason"`
			}
			if msg := decodeArgs(c.ToolCall.Parameters, &args); msg != "" {
				return agent.ErrorResult[Data](msg), nil
			}
			switch args.Type {
			case "capital_call", "distribution_notice", "k1", "cover_letter", "other":
			default:
				return agent.ErrorResult[Data](fmt.Sprintf("document_type %q is not one of capital_call, distribution_notice, k1, cover_letter, other", args.Type)), nil
			}
			if strings.TrimSpace(args.Reason) == "" {
				return agent.ErrorResult[Data]("give a reason"), nil
			}
			return a.finish(ctx, c, &Outcome{Disposition: DispositionSetAside, DocumentType: args.Type, Reason: args.Reason}, stateSetAside, "The document is set aside.")
		},
	}
}

func (a *intake) manualReview() agent.Tool[Data] {
	return agent.Tool[Data]{
		Definition: llms.ToolDefinition{
			Name:        toolManualReview,
			Description: "Send the statement to manual review because you cannot extract it with confidence. Ends the work on it.",
			InputSchema: json.RawMessage(manualReviewSchema),
		},
		Effect:   agent.EffectWrite,
		Contract: agent.ContractAtomicLocal,
		Execute: func(ctx context.Context, c *agent.Call[Data]) (agent.Result[Data], error) {
			var args struct {
				Reason string `json:"reason"`
			}
			if msg := decodeArgs(c.ToolCall.Parameters, &args); msg != "" {
				return agent.ErrorResult[Data](msg), nil
			}
			if strings.TrimSpace(args.Reason) == "" {
				return agent.ErrorResult[Data]("give a reason"), nil
			}
			return a.finish(ctx, c, &Outcome{Disposition: DispositionManual, Reason: args.Reason}, stateManual, "The statement is sent to manual review.")
		},
	}
}

// finish stores the document's outcome and ends the run in state to.
func (a *intake) finish(ctx context.Context, c *agent.Call[Data], out *Outcome, to, msg string) (agent.Result[Data], error) {
	d, env := c.Data, c.Env
	if err := checkOpen(ctx, env, d.Document); err != nil {
		return agent.Result[Data]{}, err
	}
	prior, err := env.ListObjects(ctx, AttemptPrefix(d.Document))
	if err != nil {
		return agent.Result[Data]{}, err //nolint:wrapcheck // a *agent.ReadError, which the runtime classifies
	}
	out.At, out.Run, out.Attempts = a.now(), env.RunID(), len(prior)
	op, err := putNew(env, OutcomeKey(d.Document), out)
	if err != nil {
		return agent.Result[Data]{}, err
	}
	return agent.Result[Data]{Content: msg, To: to, Data: d, Ops: []store.Op{op}}, nil
}

// decodeArgs decodes a tool's arguments strictly (ADR 0021, item 13: no
// repeated or case-mismatched keys, no unknown fields), returning a message for
// the model when they do not fit.
func decodeArgs(raw json.RawMessage, v any) string {
	if err := strictjson.Decode(raw, v); err != nil {
		return "the arguments do not match the tool's schema: " + err.Error()
	}
	return ""
}
