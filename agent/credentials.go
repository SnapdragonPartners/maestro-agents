package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// RunContext is the opaque context every tool call carries (TOOL-8, ADR
// 0016 item 12): the run and the principal it acts for. The credential
// provider and the policy hook are both keyed by it (HOST-5).
type RunContext struct {
	Scope     store.Scope
	Instance  string
	AgentType string
	// Principal is the host's name for whose authority the run acts under,
	// fixed when the run started (ActivationInput.Principal). Empty means
	// the host's own.
	Principal string
}

// CredentialProvider returns the credential a tool names, for the run
// context's principal, when the tool executes (TOOL-8, HOST-5). The runtime
// never records or logs what it returns. Tools of every run the controller
// serves call it concurrently, so it must be safe for concurrent use.
type CredentialProvider func(ctx context.Context, rc *RunContext, name string) (Secret, error)

// ErrNoCredential is returned for a credential the host does not provide,
// and for every credential when no provider is set.
var ErrNoCredential = errors.New("agent: no such credential")

func noCredentials(_ context.Context, _ *RunContext, name string) (Secret, error) {
	return Secret{}, fmt.Errorf("%w: %q (no credential provider)", ErrNoCredential, name)
}

func atomicLocalNoCredentials(_ context.Context, _ *RunContext, name string) (Secret, error) {
	return Secret{}, fmt.Errorf("%w: %q (an atomic_local tool receives no credential provider, ADR 0005 item 9)", ErrNoCredential, name)
}

// WithCredentials sets the credential provider (TOOL-8). Nil, the default,
// provides none: every request fails with ErrNoCredential.
func WithCredentials(p CredentialProvider) Option {
	return func(c *Controller) {
		if p == nil {
			p = noCredentials
		}
		c.creds = p
	}
}

// redacted is how a Secret renders everywhere but Reveal.
const redacted = "[redacted]"

// Secret holds a credential (ADR 0016, item 14). Its value is reached only
// through Reveal: printing, formatting, logging or encoding it as JSON
// renders "[redacted]". The value sits behind a pointer, so even where fmt
// bypasses these methods (a Secret in an unexported struct field, printed
// by reflection) it prints an address, never the value.
type Secret struct {
	p *secretValue
}

type secretValue struct{ v string }

// NewSecret wraps a credential's value.
func NewSecret(value string) Secret { return Secret{p: &secretValue{v: value}} }

// Reveal returns the credential's value. A tool passes it to the
// destination and nowhere else: a tool result is recorded.
func (s Secret) Reveal() string {
	if s.p == nil {
		return ""
	}
	return s.p.v
}

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s.Reveal() == "" }

// String implements fmt.Stringer.
func (Secret) String() string { return redacted }

// GoString implements fmt.GoStringer.
func (Secret) GoString() string { return redacted }

// Format implements fmt.Formatter, so no verb or flag shows the value.
func (Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON implements json.Marshaler.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// runContext is the activation's run context.
func (a *activation) runContext() RunContext {
	return RunContext{Scope: a.scope, Instance: a.run.Instance, AgentType: a.run.AgentType, Principal: a.principal}
}
