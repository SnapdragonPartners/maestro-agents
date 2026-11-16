package intake

import (
	"context"
	"fmt"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/document"
	"github.com/SnapdragonPartners/maestro-agents/standalone"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// LoadFunc reads a statement PDF: document.Load's signature.
type LoadFunc func(ctx context.Context, name string, data []byte, opts document.Options) (*document.Document, error)

func defaultLoad(ctx context.Context, name string, data []byte, opts document.Options) (*document.Document, error) {
	return document.Load(ctx, name, data, opts) //nolint:wrapcheck // document's errors name the step
}

// loadState reads the input named by the run's file trigger, loads it, and
// stores the document with the transition. A document already stored (the
// same file run again under another instance) is flagged as a duplicate and
// not processed (intake I-2); one with no usable text goes to manual review
// (I-3).
func (a *intake) loadState(ctx context.Context, env *agent.Env, d Data) (agent.Transition[Data], error) {
	t, err := standalone.ParseFileTrigger(env.Trigger())
	if err != nil {
		return agent.Transition[Data]{}, fmt.Errorf("intake: trigger: %w", err)
	}
	data, err := t.Read()
	if err != nil {
		return agent.Transition[Data]{}, fmt.Errorf("intake: read input: %w", err)
	}
	d.Document, d.Name = document.Hash(data), t.Name
	var prior Document
	exists, err := readJSON(ctx, env, DocumentKey(d.Document), &prior)
	if err != nil {
		return agent.Transition[Data]{}, err
	}
	if exists {
		return agent.Goto(stateDuplicate, d), nil
	}
	now := a.now()
	// Running the agent is the operator's acceptance of reading a
	// copy-restricted statement's text, until acceptance per document is
	// built (ADR 0018; maestro-agents#17). Load applies it only to a
	// restricted PDF, and the document and manifest record it.
	by := env.RunContext().Principal
	if by == "" {
		by = implicitAcceptor
	}
	ov := &document.Override{By: by, At: now, Reason: implicitReason}
	doc, err := a.load(ctx, t.Name, data, document.Options{Override: ov})
	if err != nil {
		return agent.Transition[Data]{}, fmt.Errorf("intake: load: %w", err)
	}
	d.Pages = doc.PageCount
	stored := Document{Document: doc, Received: now, Run: env.RunID()}
	for _, p := range doc.Pages {
		stored.Text = append(stored.Text, p.Text)
	}
	docOp, err := putNew(env, DocumentKey(d.Document), &stored)
	if err != nil {
		return agent.Transition[Data]{}, err
	}
	if doc.TextLayer == document.TextNone || doc.TextLayer == document.TextNotRead {
		out := Outcome{At: now, Disposition: DispositionManual, Reason: "the PDF has no usable text layer", Run: env.RunID()}
		outOp, err := putNew(env, OutcomeKey(d.Document), &out)
		if err != nil {
			return agent.Transition[Data]{}, err
		}
		return agent.Goto(stateManual, d, docOp, outOp), nil
	}
	return agent.Goto(stateExtract, d, []store.Op{docOp}...), nil
}
