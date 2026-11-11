package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/SnapdragonPartners/maestro-agents/internal/jsonschema"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Type is an agent type: a state machine whose state data is the Go type D
// (STM-1, STM-5). D is serialized as JSON at every step boundary, so it
// must round-trip through encoding/json.
type Type[D any] struct {
	// Init returns the initial state data from the run's trigger and
	// configuration, which env exposes. It runs when the run starts, and
	// its result is checkpointed with the run's start; once that commit
	// lands it never runs again for the run. Until then it may run more
	// than once (after a crash, or a commit whose result is unknown), so,
	// like a code state, it must have no durable effect of its own: only
	// its committed result is kept. Nil starts from D's zero value. An
	// error fails the run with ReasonStateError, unless, as for a code
	// state, it wraps a *ReadError (a store failure, ADR 0003 item 15) or
	// the context has ended (the context's cause decides). A panic is
	// recovered and treated as an error.
	Init func(ctx context.Context, env *Env) (D, error)

	// Migrate converts state data checkpointed by another version of this
	// agent type (RUN-5). Its result is checkpointed by the next step
	// boundary; until that commit lands it may run again for the same
	// checkpoint (after a crash, an indeterminate commit, or a shutdown
	// before the next step boundary), so it must have no durable effect of
	// its own. Nil refuses such a checkpoint: the run fails with
	// ReasonStateError. An error or panic fails the run the same way, with
	// the exceptions Init's has.
	Migrate func(ctx context.Context, fromVersion string, data json.RawMessage) (D, error)

	// Name is the agent type's registered name, matched against a run
	// record's AgentType.
	Name string

	// Version is recorded in every checkpoint; a checkpoint from another
	// version goes through Migrate.
	Version string

	// Initial names the state a run starts in. It must not be terminal.
	Initial string

	// States declares every state. Names are unique.
	States []State[D]

	// Triggers names the trigger kinds the type accepts (MAN-1, MODE-4):
	// at least one, each non-empty and named once. A run whose
	// store.Run.TriggerKind is not among them fails at its start with
	// ReasonConfigError (MAN-2, ADR 0011 item 5). TriggerOneShot,
	// TriggerFolder and TriggerSchedule name MODE-4's kinds; the set is
	// open.
	Triggers []string

	// Config is the instance configuration's JSON Schema (MAN-1), in the
	// subset ADR 0011 item 6 lists; nil takes any configuration. A schema
	// using any other keyword is refused at registration. At the run's
	// start the configuration (JSON null if absent) is validated against
	// it, and one it rejects fails the run with ReasonConfigError, the
	// detail listing every problem (MAN-2).
	Config json.RawMessage

	// Budgets are default budgets (MAN-1, ADR 0011 item 7): each of the
	// token, turn and active-time budgets that ActivationInput.Budgets
	// leaves zero takes the value here when the run starts. A default
	// deadline or cost budget is refused at registration.
	Budgets Budgets
}

// State is one state of an agent type. Exactly one of Code, Model and End
// is set: a code state runs Go code that chooses the next state, a model
// state runs a tool loop that one of its tools leaves (STM-2, STM-3), and a
// terminal state ends the run with its End (STM-4).
type State[D any] struct {
	// Code runs the state's work and returns the transition it chooses. It
	// must have no durable effect of its own (ADR 0003, item 5): its only
	// durable output is the transition, with the object operations it
	// carries, committed atomically. It may run more than once for the
	// same step: after a crash, and when its object operations conflict
	// with another instance's commit, it runs again against fresh reads.
	// Each run receives the step's starting data, never a value an earlier
	// run modified.
	//
	// An error fails the run with ReasonStateError, unless it wraps a
	// *ReadError (a store failure, ADR 0003 item 15) or the context has
	// ended (the context's cause decides, ADR 0003 item 17). A panic is
	// recovered and treated as an error.
	Code func(ctx context.Context, env *Env, data D) (Transition[D], error)

	// Model makes the state a model state (ADR 0008).
	Model *ModelState[D]

	// End makes the state terminal: a run entering it ends with this
	// status and reason.
	End End

	// Name is the state's name, unique within the agent type.
	Name string

	// Next declares the states a code or model state may transition to
	// (STM-1). A code state's transition to any other state fails the run
	// with ReasonStateError, a tool's with ReasonToolError.
	Next []string
}

// Transition is a code state's result: the state to enter, its data, and
// the object-store operations committed with it (ADR 0005, item 3).
type Transition[D any] struct {
	Data D
	To   string
	// Ops are object operations only, in the run's namespace (Env.PutJSON,
	// Env.PutBytes, Env.Delete; ADR 0012), conditional on the versions the
	// state read. Any other kind, or a namespace other than the run's,
	// fails the run with ReasonStateError.
	Ops []store.Op
}

// Goto returns a transition to the state named to.
func Goto[D any](to string, data D, ops ...store.Op) Transition[D] {
	return Transition[D]{To: to, Data: data, Ops: ops}
}

// End is the ending a terminal state declares (STM-4, RUN-3): a reason, and
// with it a status. An agent type chooses among the endings that are its to
// choose; the others are the runtime's and the host's. The empty End marks
// a state that is not terminal.
type End string

// The endings a terminal state can declare. An agent type that reports
// either success disposition declares one terminal state for each.
const (
	// EndChanged succeeds with reason changed: the run made its change.
	EndChanged End = ReasonChanged
	// EndNoChange succeeds with reason no_change: nothing needed doing.
	EndNoChange End = ReasonNoChange
	// EndInvalidOutput fails with reason invalid_output.
	EndInvalidOutput End = ReasonInvalidOutput
)

// status returns the status an End ends a run with, and whether the End is
// one an agent type may declare.
func (e End) status() (store.Status, bool) {
	switch e {
	case EndChanged, EndNoChange:
		return store.StatusSucceeded, true
	case EndInvalidOutput:
		return store.StatusFailed, true
	}
	return "", false
}

func (e End) outcome() OutcomeEntry {
	st, _ := e.status()
	return OutcomeEntry{Status: st, Reason: store.Reason{Code: string(e)}}
}

// Definition is a registered agent type, whatever its state data's type.
// Only Type implements it.
type Definition interface {
	TypeName() string
	compile() (*machine, error)
}

// TypeName returns the agent type's name.
func (t *Type[D]) TypeName() string { return t.Name }

// machine is an agent type with its state data erased to JSON, as the
// controller drives it.
type machine struct {
	init     func(ctx context.Context, env *Env) (json.RawMessage, error)
	migrate  func(ctx context.Context, from string, data json.RawMessage) (json.RawMessage, error)
	states   map[string]*erasedState
	triggers map[string]bool
	config   *jsonschema.Schema
	manifest encodedManifest
	name     string
	version  string
	initial  string
	defaults Budgets
}

type erasedState struct {
	code  func(ctx context.Context, env *Env, data json.RawMessage) (erasedTransition, error)
	model *erasedModel
	end   End
	next  map[string]bool
	name  string
}

type erasedTransition struct {
	to   string
	data json.RawMessage
	ops  []store.Op
}

func (t *Type[D]) compile() (*machine, error) {
	if t == nil {
		return nil, fmt.Errorf("%w: nil agent type", ErrInvalidType)
	}
	if err := t.check(); err != nil {
		return nil, err
	}
	// Every function is captured now, so changing t after registration
	// (to register another version, say) does not change this machine.
	initFn, migrate := t.Init, t.Migrate
	m := &machine{
		states:   make(map[string]*erasedState, len(t.States)),
		triggers: make(map[string]bool, len(t.Triggers)),
		name:     t.Name,
		version:  t.Version,
		initial:  t.Initial,
		defaults: t.Budgets,
	}
	if err := t.compileManifest(m); err != nil {
		return nil, err
	}
	m.init = func(ctx context.Context, env *Env) (json.RawMessage, error) {
		var d D
		if initFn != nil {
			var err error
			if d, err = initFn(ctx, env); err != nil {
				return nil, err
			}
		}
		return marshalData(d)
	}
	if migrate != nil {
		m.migrate = func(ctx context.Context, from string, data json.RawMessage) (json.RawMessage, error) {
			d, err := migrate(ctx, from, data)
			if err != nil {
				return nil, err
			}
			return marshalData(d)
		}
	}
	for i := range t.States {
		s := &t.States[i]
		es := &erasedState{name: s.Name, end: s.End, next: map[string]bool{}}
		if s.Model != nil {
			es.model = eraseModel(s.Model, t.Version)
		}
		for _, n := range s.Next {
			es.next[n] = true
		}
		if code := s.Code; code != nil {
			es.code = func(ctx context.Context, env *Env, raw json.RawMessage) (erasedTransition, error) {
				var d D
				if err := json.Unmarshal(raw, &d); err != nil {
					return erasedTransition{}, fmt.Errorf("agent: decode state data: %w", err)
				}
				tr, err := code(ctx, env, d)
				if err != nil {
					return erasedTransition{}, err
				}
				data, err := marshalData(tr.Data)
				if err != nil {
					return erasedTransition{}, err
				}
				return erasedTransition{to: tr.To, data: data, ops: tr.Ops}, nil
			}
		}
		m.states[s.Name] = es
	}
	return m, nil
}

// compileManifest sets what the machine enforces of the type's manifest
// (ADR 0011): its trigger kinds, its compiled configuration schema and the
// manifest's encoding.
func (t *Type[D]) compileManifest(m *machine) error {
	for _, k := range t.Triggers {
		m.triggers[k] = true
	}
	var err error
	if t.Config != nil {
		// check has compiled it once already, so this cannot fail.
		if m.config, err = jsonschema.Compile(t.Config); err != nil {
			return t.invalid("configuration schema: %v", err)
		}
	}
	m.manifest, err = encodeManifest(t.manifest())
	return err
}

func marshalData(d any) (json.RawMessage, error) {
	data, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("agent: encode state data: %w", err)
	}
	if err := store.ValidJSON(data); err != nil {
		return nil, fmt.Errorf("agent: encode state data: %w", err)
	}
	return data, nil
}

// check validates the declaration (STM-1): unique, non-empty names; each
// state either code or terminal; declared transitions name declared
// states; a non-terminal initial state.
func (t *Type[D]) check() error {
	if t.Name == "" || !utf8.ValidString(t.Name) {
		return fmt.Errorf("%w: agent type name %q is empty or not valid UTF-8", ErrInvalidType, t.Name)
	}
	if !utf8.ValidString(t.Version) {
		return t.invalid("version is not valid UTF-8")
	}
	byName := make(map[string]*State[D], len(t.States))
	for i := range t.States {
		s := &t.States[i]
		if s.Name == "" || !utf8.ValidString(s.Name) {
			return t.invalid("state %d has an empty or invalid name", i)
		}
		if byName[s.Name] != nil {
			return t.invalid("state %q is declared twice", s.Name)
		}
		byName[s.Name] = s
	}
	for i := range t.States {
		if err := t.checkState(&t.States[i], byName); err != nil {
			return err
		}
	}
	init := byName[t.Initial]
	switch {
	case init == nil:
		return t.invalid("initial state %q is not declared", t.Initial)
	case init.End != "":
		return t.invalid("initial state %q is terminal", t.Initial)
	}
	if err := t.checkTools(); err != nil {
		return err
	}
	return t.checkManifest()
}

// checkManifest checks what the type declares beyond its states (MAN-1,
// ADR 0011): its trigger kinds, configuration schema and default budgets.
func (t *Type[D]) checkManifest() error {
	if len(t.Triggers) == 0 {
		return t.invalid("declares no trigger kind (MAN-1)")
	}
	for _, k := range t.Triggers {
		if k == "" || !utf8.ValidString(k) {
			return t.invalid("trigger kind %q is empty or not valid UTF-8", k)
		}
	}
	if dup := duplicate(t.Triggers); dup != "" {
		return t.invalid("declares trigger kind %q twice", dup)
	}
	if t.Config != nil {
		if _, err := jsonschema.Compile(t.Config); err != nil {
			return t.invalid("configuration schema: %v", err)
		}
	}
	b := &t.Budgets
	switch {
	case !b.Deadline.IsZero():
		return t.invalid("default budgets set a deadline, which is the host's to set (ADR 0011, item 7)")
	case b.Cost != 0:
		return t.invalid("default budgets set a cost budget, which is not built yet (MVP.md, BUD-1)")
	}
	if err := b.validate(); err != nil {
		return t.invalid("default budgets: %v", err)
	}
	return nil
}

// checkTools checks that a tool name names one tool across the type's
// states: tools of one name have one identity (ADR 0011, item 3).
func (t *Type[D]) checkTools() error {
	identities := map[string]string{}
	for i := range t.States {
		ms := t.States[i].Model
		if ms == nil {
			continue
		}
		for j := range ms.Tools {
			tool := &ms.Tools[j]
			name := tool.Definition.Name
			id := toolIdentity(&tool.Definition, tool.Effect, tool.Contract, t.toolVersion(tool))
			if prev, ok := identities[name]; ok && prev != id {
				return t.invalid("tool %q is declared with different definitions, effects, contracts or versions in two states (ADR 0011, item 3)", name)
			}
			identities[name] = id
		}
	}
	return nil
}

func (t *Type[D]) checkState(s *State[D], byName map[string]*State[D]) error {
	_, validEnd := s.End.status()
	kinds := 0
	for _, set := range []bool{s.Code != nil, s.Model != nil, s.End != ""} {
		if set {
			kinds++
		}
	}
	switch {
	case kinds != 1:
		return t.invalid("state %q must set exactly one of Code, Model and End", s.Name)
	case s.End != "" && len(s.Next) > 0:
		return t.invalid("terminal state %q declares transitions", s.Name)
	case s.End != "" && !validEnd:
		return t.invalid("terminal state %q has End %q, which an agent type cannot declare", s.Name, s.End)
	case s.End == "" && len(s.Next) == 0:
		return t.invalid("state %q declares no transitions", s.Name)
	}
	if s.Model != nil {
		if err := t.checkModel(s); err != nil {
			return err
		}
	}
	for _, n := range s.Next {
		if byName[n] == nil {
			return t.invalid("state %q declares a transition to undeclared state %q", s.Name, n)
		}
	}
	if dup := duplicate(s.Next); dup != "" {
		return t.invalid("state %q declares the transition to %q twice", s.Name, dup)
	}
	return nil
}

func (t *Type[D]) invalid(format string, args ...any) error {
	return fmt.Errorf("%w: agent type %q: %s", ErrInvalidType, t.Name, fmt.Sprintf(format, args...))
}

func duplicate(names []string) string {
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1] {
			return sorted[i]
		}
	}
	return ""
}
