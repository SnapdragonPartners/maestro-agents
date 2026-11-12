package agent

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/ratelimit"
)

// ModelClient is the only model client an activation accepts
// (ActivationInput.Client, ADR 0014): a maestro-llms client built by
// NewModelClient, whose middleware chain carries the runtime's attempt layer
// inside the retry loop, so every provider attempt, each retry included, is
// admitted against the run's budgets and recorded before it is sent (BUD-3,
// ADR 0004 item 6). A host builds one per model route and shares it across
// runs: the layer finds the run it serves in the call's context, and a call
// outside any activation passes through it unrecorded.
type ModelClient struct {
	chain llms.ChatClient
	est   middleware.TokenEstimator
}

// ModelClientConfig is what a host tunes in a ModelClient's chain. The zero
// value is usable: maestro-llms' default retry and circuit policies, no
// timeout, no rate limiter and no observer.
type ModelClientConfig struct {
	// Limiter is the host's rate limiter, shared across runs; nil adds
	// none.
	Limiter ratelimit.Limiter
	// Estimator sizes requests for the rate limiter and the budgets; nil
	// selects maestro-llms' DefaultEstimator. A hard token budget needs it
	// to implement middleware.InputBounder (DefaultEstimator does).
	Estimator middleware.TokenEstimator
	// Observer, if non-nil, is the innermost metrics layer: it sees each
	// provider attempt.
	Observer middleware.Observer
	// Retry is the retry policy; nil selects middleware.DefaultRetryConfig.
	Retry *middleware.RetryConfig
	// Outer are the host's own layers, outermost first, placed outside the
	// rest of the chain (aggregate metrics, error mapping, attribution). A
	// layer may observe a request, retry it and map its errors, but must
	// pass it on unchanged: the runtime records the request the model
	// state built, and refuses an attempt whose request a layer changed
	// (ADR 0014, item 3).
	Outer []middleware.ChatMiddleware
	// Circuit is the circuit-breaker policy; the zero value selects
	// middleware.DefaultCircuitConfig.
	Circuit middleware.CircuitConfig
	// Timeout bounds each attempt; zero or less sets no timeout.
	Timeout time.Duration
}

// NewModelClient builds a ModelClient over base, a maestro-llms provider
// client, in a fixed order (ADR 0014, item 2):
//
//	Outer -> validation -> retry -> timeout -> circuit -> rate limit -> attempt -> metrics -> base
//
// The attempt layer is the runtime's: it sits inside the retry loop and the
// host's rate limiter, so a rate-limit wait or refusal happens before an
// attempt is admitted or recorded. The order is not configurable.
func NewModelClient(base llms.ChatClient, cfg ModelClientConfig) (*ModelClient, error) { //nolint:gocritic // hugeParam: a configuration struct, passed once
	if isNil(base) {
		return nil, errors.New("agent: NewModelClient: base client is nil")
	}
	est := cfg.Estimator
	if est == nil {
		est = middleware.DefaultEstimator{}
	}
	retry := middleware.DefaultRetryConfig()
	if cfg.Retry != nil {
		retry = *cfg.Retry
	}
	mws := make([]middleware.ChatMiddleware, 0, len(cfg.Outer)+7)
	for _, m := range cfg.Outer {
		if m == nil {
			return nil, errors.New("agent: NewModelClient: an outer layer is nil")
		}
		mws = append(mws, m)
	}
	mws = append(mws, middleware.ValidationChat(), middleware.RetryChat(retry))
	if cfg.Timeout > 0 {
		mws = append(mws, middleware.TimeoutChat(cfg.Timeout))
	}
	mws = append(mws, middleware.CircuitChat(cfg.Circuit))
	if cfg.Limiter != nil {
		if isNil(cfg.Limiter) {
			return nil, errors.New("agent: NewModelClient: limiter is a typed nil")
		}
		mws = append(mws, middleware.RateLimitChat(cfg.Limiter, est))
	}
	mws = append(mws, attemptLayer)
	if cfg.Observer != nil {
		mws = append(mws, middleware.MetricsChat(cfg.Observer))
	}
	return &ModelClient{chain: middleware.ChainChat(base, mws...), est: est}, nil
}

// Model reports the base client's model route.
func (c *ModelClient) Model() llms.ModelRef { return c.chain.Model() }

// Complete sends req through the chain. Inside an activation the runtime
// calls it; a host may call it outside one, unrecorded and unbudgeted.
func (c *ModelClient) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) { //nolint:gocritic // hugeParam: llms.ChatClient's signature
	return c.chain.Complete(ctx, req) //nolint:wrapcheck // the chain's errors pass through for classification
}

// isNil reports whether v is nil or holds a nil pointer, map, slice, func or
// channel, which would panic on first use.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() { //nolint:exhaustive // only nillable kinds can hold nil
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return rv.IsNil()
	default:
		return false
	}
}

// attemptKey carries the model run a call serves through the chain's
// context to the attempt layer.
type attemptKey struct{}

// attemptLayer is the runtime's layer in every ModelClient: for a call made
// by an activation's model state it admits and records each provider attempt
// (modelRun.attempt); any other call passes through.
func attemptLayer(next llms.ChatClient) llms.ChatClient { return &attemptClient{next: next} }

type attemptClient struct{ next llms.ChatClient }

func (c *attemptClient) Model() llms.ModelRef { return c.next.Model() }

func (c *attemptClient) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) { //nolint:gocritic // hugeParam: llms.ChatClient's signature
	r, ok := ctx.Value(attemptKey{}).(*modelRun)
	if !ok || r == nil {
		return c.next.Complete(ctx, req) //nolint:wrapcheck // passed through unchanged
	}
	if err := r.attempt(ctx, &req); err != nil {
		return llms.ChatResponse{}, err
	}
	return c.next.Complete(ctx, req) //nolint:wrapcheck // the provider's errors pass through for retry and classification
}
