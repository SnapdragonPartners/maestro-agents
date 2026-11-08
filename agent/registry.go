package agent

import (
	"fmt"
	"sync"
)

// Registry holds the agent types a host runs, by name (MAN-3). The host
// constructs it and registers each type explicitly; the library has no
// fixed set. It is safe for concurrent use.
type Registry struct {
	types map[string]*machine
	mu    sync.RWMutex
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{types: map[string]*machine{}}
}

// Register checks an agent type's declaration (STM-1) and adds it. A
// malformed declaration, or a name already registered, returns an error
// wrapping ErrInvalidType.
func (r *Registry) Register(d Definition) error {
	if d == nil {
		return fmt.Errorf("%w: nil agent type", ErrInvalidType)
	}
	m, err := d.compile()
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.types[m.name]; ok {
		return fmt.Errorf("%w: agent type %q is already registered", ErrInvalidType, m.name)
	}
	r.types[m.name] = m
	return nil
}

func (r *Registry) lookup(name string) (*machine, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.types[name]
	return m, ok
}
