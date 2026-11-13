package agent

import "time"

// WithTelemetryTimeout shortens the bound on each Sink call, so a test can
// exercise TelemetryTimeout without waiting for it.
func WithTelemetryTimeout(d time.Duration) Option {
	return func(c *Controller) { c.telemetryTimeout = d }
}
