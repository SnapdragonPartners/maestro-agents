package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/document"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/export"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
	"github.com/SnapdragonPartners/maestro-agents/store"
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
			Description: "Read the statement: its page count and each page's text in layout mode, which keeps each printed line, and each label beside its numbers, on one line.",
			InputSchema: json.RawMessage(emptySchema),
		},
		Effect: agent.EffectRead,
		Execute: func(ctx context.Context, c *agent.Call[Data]) (agent.Result[Data], error) {
			doc, err := readDocument(ctx, c.Env, c.Data.Document)
			if err != nil {
				return agent.Result[Data]{}, err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%d page(s); text layer %s.\n", doc.PageCount, doc.TextLayer)
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
			return agent.TextResult[Data](b.String()), nil
		},
	}
}

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
		Submitted: c.ToolCall.Parameters, WrapUp: c.WrapUp,
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
// be read, any record repeats another's investor and fund, or any has a
// verification error; else warn if any record has a warning; else pass.
func verifySubmission(doc *document.Document, att *Attempt) {
	recs, problem := decodeSubmission(att.Submitted)
	att.Problem = problem
	fail, warn := problem != "", false
	seen := map[[2]string]bool{}
	for _, r := range recs {
		ar := AttemptRecord{Record: r.rec, Problem: r.problem}
		if r.rec != nil {
			who := [2]string{r.rec.Investor.Text, r.rec.Fund.Text}
			if seen[who] {
				ar.Problem = "a second record for the same investor and fund"
			} else {
				seen[who] = true
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
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return nil, "the arguments are not {\"records\": [...]}: " + err.Error()
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

// decodeArgs decodes a tool's arguments strictly, returning a message for
// the model when they do not fit.
func decodeArgs(raw json.RawMessage, v any) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return "the arguments do not match the tool's schema: " + err.Error()
	}
	return ""
}
