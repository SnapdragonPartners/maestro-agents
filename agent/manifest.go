package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// Trigger kinds MODE-4 names. The set is open: an agent type may declare,
// and a host may set, any non-empty kind (ADR 0011, item 5).
const (
	// TriggerOneShot: one input, processed once, as from a command line.
	TriggerOneShot = "one_shot"
	// TriggerFolder: a new file in a watched folder.
	TriggerFolder = "folder"
	// TriggerSchedule: a schedule.
	TriggerSchedule = "schedule"
)

// Approval is what follows when the policy hook asks for approval (TOOL-4):
// the run proposes the action and finishes, or suspends until a decision.
// Approval is not built yet (MVP.md): a type declaring either mode is
// refused at registration (ADR 0011, item 9).
type Approval string

// The approval modes.
const (
	// ApprovalPropose: the pending action is part of the run's output,
	// and the run can succeed; approving and carrying it out happen
	// outside the agent.
	ApprovalPropose Approval = "propose"
	// ApprovalSuspend: the run waits for the decision, then executes the
	// approved action if its digest still matches.
	ApprovalSuspend Approval = "suspend"
)

// ManifestFormat is the version of Manifest's JSON form.
const ManifestFormat = 1

// Manifest is an agent type's manifest (MAN-1, ADR 0011): what it declares,
// as an orchestrator reads it. The registry computes it when the type is
// registered; its JSON form's SHA-256 is recorded with each run's start.
type Manifest struct {
	// Budgets are the type's default budgets, nil if it sets none.
	Budgets *Budgets `json:"budgets,omitempty"`
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Initial string   `json:"initial"`
	// Config is the instance configuration's JSON Schema, nil if the
	// type takes any configuration.
	Config json.RawMessage `json:"config,omitempty"`
	States []StateManifest `json:"states"`
	Tools  []ToolManifest  `json:"tools,omitempty"`
	// Skills are the type's skills, by name (ADR 0013, item 4).
	Skills []SkillManifest `json:"skills,omitempty"`
	// Templates are the type's prompt templates, by path (ADR 0013,
	// item 5).
	Templates []TemplateManifest `json:"templates,omitempty"`
	Triggers  []string           `json:"triggers"`
	Format    int                `json:"format"`
}

// The kinds of state a manifest lists.
const (
	StateKindCode     = "code"
	StateKindModel    = "model"
	StateKindTerminal = "terminal"
)

// StateManifest is one state in a manifest.
type StateManifest struct {
	Name string `json:"name"`
	// Kind is StateKindCode, StateKindModel or StateKindTerminal.
	Kind string `json:"kind"`
	// End is a terminal state's ending.
	End End `json:"end,omitempty"`
	// Approval is a model state's approval mode.
	Approval Approval `json:"approval,omitempty"`
	Next     []string `json:"next,omitempty"`
	// Tools names the tools a model state declares, in order.
	Tools []string `json:"tools,omitempty"`
	// Skills names the skills a model state grants, in order. A state
	// granting any is also offered ReadSkillTool, which Tools does not
	// list: its definition is derived from these skills (ADR 0013, item
	// 4).
	Skills []string `json:"skills,omitempty"`
	// WrapUp names a model state's wrap-up tools (ADR 0010).
	WrapUp []string `json:"wrap_up,omitempty"`
}

// ToolManifest is one tool in a manifest: its declaration and its content
// identity (MAN-1, ADR 0011 item 3).
type ToolManifest struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Effect      Effect        `json:"effect"`
	Contract    WriteContract `json:"contract,omitempty"`
	// Version is the version of the code that implements the tool; empty
	// marks an unversioned tool, whose identity covers its definition
	// alone (ADR 0011, item 4).
	Version  string   `json:"version,omitempty"`
	Approval Approval `json:"approval,omitempty"`
	// Identity is the tool's content identity: a SHA-256, in hex.
	Identity    string          `json:"identity"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// identityFormat tags the encoding a tool identity hashes, so a change to
// the encoding changes every identity rather than colliding.
const identityFormat = "maestro-agents/tool-identity/1"

// toolIdentity returns a tool's content identity (ADR 0011, item 3): the
// SHA-256 of a fixed JSON encoding of its definition, effect, write
// contract and version. The schema is compacted, so formatting alone does
// not change it.
func toolIdentity(def *llms.ToolDefinition, effect Effect, contract WriteContract, version string) string {
	b, err := json.Marshal(struct {
		Format      string          `json:"format"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Effect      Effect          `json:"effect"`
		Contract    WriteContract   `json:"contract"`
		Version     string          `json:"version"`
		InputSchema json.RawMessage `json:"input_schema,omitempty"`
	}{identityFormat, def.Name, def.Description, effect, contract, version, compactJSON(def.InputSchema)})
	if err != nil {
		// Every field is a string or JSON registration has validated.
		panic(fmt.Sprintf("agent: encode tool identity: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// manifest returns the type's manifest. The type has passed check.
func (t *Type[D]) manifest(l *loaded) *Manifest {
	m := &Manifest{
		Format: ManifestFormat, Name: t.Name, Version: t.Version, Initial: t.Initial,
		Triggers: append([]string(nil), t.Triggers...), Config: compactJSON(t.Config),
	}
	for _, s := range l.ordered {
		m.Skills = append(m.Skills, s.manifest())
	}
	if l.templates != nil {
		m.Templates = append([]TemplateManifest(nil), l.templates.manifest...)
	}
	if t.Budgets != (Budgets{}) {
		b := t.Budgets
		m.Budgets = &b
	}
	listed := map[string]bool{}
	for i := range t.States {
		s := &t.States[i]
		sm := StateManifest{Name: s.Name, End: s.End, Next: append([]string(nil), s.Next...)}
		switch {
		case s.Code != nil:
			sm.Kind = StateKindCode
		case s.Model != nil:
			sm.Kind = StateKindModel
			sm.Approval = s.Model.Approval
			sm.WrapUp = append([]string(nil), s.Model.WrapUp.Tools...)
			sm.Skills = append([]string(nil), s.Model.Skills...)
			for j := range s.Model.Tools {
				tool := &s.Model.Tools[j]
				name := tool.Definition.Name
				sm.Tools = append(sm.Tools, name)
				if !listed[name] {
					listed[name] = true
					m.Tools = append(m.Tools, t.toolManifest(tool))
				}
			}
		default:
			sm.Kind = StateKindTerminal
		}
		m.States = append(m.States, sm)
	}
	return m
}

// toolVersion returns the version a tool's identity covers: its own, or
// the agent type's (ADR 0011, item 3).
func (t *Type[D]) toolVersion(tool *Tool[D]) string {
	if tool.Version != "" {
		return tool.Version
	}
	return t.Version
}

func (t *Type[D]) toolManifest(tool *Tool[D]) ToolManifest {
	v := t.toolVersion(tool)
	return ToolManifest{
		Name: tool.Definition.Name, Description: tool.Definition.Description,
		InputSchema: compactJSON(tool.Definition.InputSchema), Effect: tool.Effect, Contract: tool.Contract,
		Version: v, Approval: tool.Approval, Identity: toolIdentity(&tool.Definition, tool.Effect, tool.Contract, v),
	}
}

// encodedManifest is a manifest's JSON form and its digest.
type encodedManifest struct {
	digest string
	json   []byte
}

func encodeManifest(m *Manifest) (encodedManifest, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return encodedManifest{}, fmt.Errorf("%w: agent type %q: encode manifest: %w", ErrInvalidType, m.Name, err)
	}
	sum := sha256.Sum256(b)
	return encodedManifest{json: b, digest: hex.EncodeToString(sum[:])}, nil
}

// Manifest returns the manifest of the registered agent type named name,
// and whether one is registered. The result is the caller's own copy.
func (r *Registry) Manifest(name string) (Manifest, bool) {
	m, ok := r.lookup(name)
	if !ok {
		return Manifest{}, false
	}
	var out Manifest
	if err := json.Unmarshal(m.manifest.json, &out); err != nil {
		// The registry encoded it; it decodes.
		panic(fmt.Sprintf("agent: decode manifest: %v", err))
	}
	return out, true
}

// ManifestDigest returns the SHA-256, in hex, of the registered agent
// type's manifest as recorded with each run's start, and whether one is
// registered.
func (r *Registry) ManifestDigest(name string) (string, bool) {
	m, ok := r.lookup(name)
	if !ok {
		return "", false
	}
	return m.manifest.digest, true
}
