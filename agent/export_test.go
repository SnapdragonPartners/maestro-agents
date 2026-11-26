package agent

import "time"

// WithTelemetryTimeout shortens the bound on each Sink call, so a test can
// exercise TelemetryTimeout without waiting for it.
func WithTelemetryTimeout(d time.Duration) Option {
	return func(c *Controller) { c.telemetryTimeout = d }
}

// StateKinds returns the kind a checkpoint records for each state of a
// registered agent type, so a test can hold it to the manifest's.
func StateKinds(r *Registry, agentType string) map[string]string {
	m, ok := r.lookup(agentType)
	if !ok {
		return nil
	}
	out := map[string]string{}
	for name, st := range m.states {
		out[name] = st.kind()
	}
	return out
}
