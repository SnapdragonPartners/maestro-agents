// Package intake is the capital-statement intake agent type (ADR 0018): one
// statement PDF in; its verified records, or its disposition, out.
//
// A run loads its input (the standalone host's file trigger) in a code state,
// storing the PDF, the document and its layout-mode text, then extracts in a model
// state whose four tools are the intake requirements' own: get_document,
// submit_extraction, set_aside and request_manual_review. The model only
// proposes. get_document gives it the PDF itself beside the layout-mode text
// (ADR 0019), so it sees the page while copying what code verifies against.
// submit_extraction verifies every record in code (ADR 0015) and,
// as an atomic local transaction, stores the attempt and, when no record has
// an error, the records and the document's outcome, ending the run. Every
// object lives in the run's namespace, keyed by the document's SHA-256, and
// is written once: a key that exists is never overwritten.
package intake

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
)

// TypeName is the agent type's registered name.
const TypeName = "addepar-intake"

// Version is the agent type's version, recorded in every checkpoint and part
// of every tool's content identity.
const Version = "2"

// MaxAttempts is the most submissions a run accepts (intake E-6): when the
// last fails, the statement goes to manual review.
const MaxAttempts = 3

// The states.
const (
	stateLoad      = "load"
	stateExtract   = "extract"
	stateVerified  = "verified"
	stateSetAside  = "set_aside"
	stateManual    = "manual_review"
	stateDuplicate = "duplicate"
)

// The tools.
const (
	toolGetDocument   = "get_document"
	toolSubmit        = "submit_extraction"
	toolSetAside      = "set_aside"
	toolManualReview  = "request_manual_review"
	pdfMediaType      = "application/pdf"
	implicitAcceptor  = "operator"
	implicitReason    = "implicit acceptance: running the intake agent reads copy-restricted statements and sends them to the model until per-document acceptance is built (maestro-agents#17)"
	configSchema      = `{"type":["object","null"],"properties":{"max_attempts":{"type":"integer","minimum":1,"maximum":3}},"additionalProperties":false}`
	defaultTurns      = 16
	defaultActiveTime = 10 * time.Minute
	defaultTokens     = 600_000
)

//go:embed prompts
var prompts embed.FS

// Data is the run's state data.
type Data struct {
	// Document is the input's SHA-256, the key of everything the run
	// stores.
	Document string `json:"document,omitempty"`
	// Name is the input's display name.
	Name string `json:"name,omitempty"`
	// Reading is how the model reads the statement (ADR 0019).
	Reading string `json:"reading,omitempty"`
	// Pages is the document's page count.
	Pages int `json:"pages,omitempty"`
	// MaxAttempts is the run's submission cap.
	MaxAttempts int `json:"max_attempts,omitempty"`
}

// Config is the instance configuration.
type Config struct {
	// MaxAttempts lowers the submission cap; zero is MaxAttempts.
	MaxAttempts int `json:"max_attempts,omitempty"`
}

// Options configures the agent type.
type Options struct {
	// Now is the clock for the times the agent records; nil is time.Now.
	Now func() time.Time
	// Load reads a document; nil is document.Load with Poppler. Tests
	// replace it.
	Load LoadFunc
}

// Type returns the intake agent type.
func Type(o Options) *agent.Type[Data] {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Load == nil {
		o.Load = defaultLoad
	}
	a := &intake{now: func() time.Time { return o.Now().UTC() }, load: o.Load}
	tmpl, err := fs.Sub(prompts, "prompts")
	if err != nil {
		panic(fmt.Sprintf("intake: prompts: %v", err)) // the embedded directory exists
	}
	exits := []string{toolSubmit, toolSetAside, toolManualReview}
	return &agent.Type[Data]{
		Name:      TypeName,
		Version:   Version,
		Triggers:  []string{agent.TriggerOneShot, agent.TriggerFolder},
		Config:    json.RawMessage(configSchema),
		Templates: tmpl,
		Budgets: agent.Budgets{
			Turns:      defaultTurns,
			ActiveTime: defaultActiveTime,
			Tokens:     agent.TokenBudget{Limit: defaultTokens, Estimated: true},
		},
		Init:    a.init,
		Initial: stateLoad,
		States: []agent.State[Data]{
			{Name: stateLoad, Code: a.loadState, Next: []string{stateExtract, stateManual, stateDuplicate}},
			{Name: stateExtract, Next: []string{stateVerified, stateSetAside, stateManual}, Model: &agent.ModelState[Data]{
				Prompt: a.prompt,
				Tools:  []agent.Tool[Data]{a.getDocument(), a.submit(), a.setAside(), a.manualReview()},
				WrapUp: agent.WrapUp{Tools: exits},
			}},
			{Name: stateVerified, End: agent.EndChanged},
			{Name: stateSetAside, End: agent.EndChanged},
			{Name: stateManual, End: agent.EndChanged},
			{Name: stateDuplicate, End: agent.EndNoChange},
		},
	}
}

type intake struct {
	now  func() time.Time
	load LoadFunc
}

var errConfig = errors.New("intake: configuration")

func (a *intake) init(_ context.Context, env *agent.Env) (Data, error) {
	d := Data{MaxAttempts: MaxAttempts}
	raw := env.Config()
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return d, nil
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Data{}, fmt.Errorf("%w: %w", errConfig, err)
	}
	if c.MaxAttempts > 0 {
		d.MaxAttempts = min(c.MaxAttempts, MaxAttempts)
	}
	return d, nil
}
