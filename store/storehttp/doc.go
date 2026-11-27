// Package storehttp is the store's wire form (ADR 0024, item 9; ADR 0026):
// [NewHandler] serves any [store.Store] over HTTP under /v1/, and
// [NewClient] returns a [Client] that is itself a store.Store (and a
// [store.Telemetry]) over that handler. The conformance suite
// (store/storetest) runs against the client over each reference store, so
// the wire proves the same contract as the store behind it.
//
// A host serves the handler behind its own authentication and decides each
// call with an [Authorizer], which sees the call's operation and the
// instance, run and namespaces it concerns ([Call]):
//
//	h, err := storehttp.NewHandler(pg, storehttp.HandlerOptions{
//		Authorizer: storehttp.AuthorizerFunc(func(ctx context.Context, c storehttp.Call) error {
//			return tokenFrom(ctx).Allows(c) // the host's rule
//		}),
//		Logger: logger,
//	})
//	mux.Handle("/v1/", authenticate(h))
//
// and a worker reaches it with its credentials in the HTTP client:
//
//	s, err := storehttp.NewClient("https://morris.internal/store", storehttp.ClientOptions{HTTPClient: authed})
//
// # Routes
//
// One route per store method, each instance, run or namespace a path
// segment (every byte outside ALPHA, DIGIT, "-", "_" and "~" percent-encoded,
// "." included), object keys and prefixes and the run query in the query
// string:
//
//	POST   /v1/instances/{instance}/commit          Commit (JSON, or multipart/mixed with artifacts)
//	POST   /v1/instances/{instance}/claim           Claim
//	POST   /v1/instances/{instance}/heartbeat       Heartbeat
//	POST   /v1/instances/{instance}/release         Release
//	POST   /v1/instances/{instance}/triggers        EnqueueTrigger
//	DELETE /v1/instances/{instance}/triggers/{id}   RemoveTrigger
//	POST   /v1/instances/{instance}/runs            CreateRun
//	POST   /v1/runs/{run}/cancel                    RequestCancel
//	GET    /v1/runs/{run}/cancel                    CancelRequest
//	GET    /v1/instances/{instance}/ownership       Ownership
//	GET    /v1/instances/{instance}/active-run      ActiveRun
//	GET    /v1/instances/{instance}/triggers        Triggers
//	GET    /v1/instances/{instance}/runs            Runs (?after=&status=&limit=&newest=)
//	GET    /v1/instances/{instance}/checkpoints/latest   LatestCheckpoint
//	GET    /v1/instances/{instance}/checkpoints/{seq}    Checkpoint
//	GET    /v1/instances/{instance}/artifacts/{digest}   Artifact (raw bytes)
//	GET    /v1/runs/{run}                           Run
//	GET    /v1/runs/{run}/entries                   Entries (?after=)
//	GET    /v1/namespaces/{namespace}/object        Object (?key=)
//	GET    /v1/namespaces/{namespace}/objects       ListObjects (?prefix=)
//	GET    /v1/claimable                            Claimable
//	POST   /v1/events                               AppendEvents
//	GET    /v1/runs/{run}/events                    Events (?after=)
//	GET    /v1/runs/{run}/status                    agent.Status
//	GET    /v1/instances/{instance}/status          agent.InstanceStatus
//	GET    /v1/healthz                              health, not authorized
//
// Bodies are the store's own JSON types, wrapped in an envelope only where a
// method takes or returns more than one value. The handler decodes request
// bodies strictly (exact keys, no repeats, no trailing data); the client
// ignores response fields it does not know, so a server may add some within
// /v1/.
//
// # Errors
//
// An error response is {"class", "message", "request_id"}, with a conflict's
// kind, operation and current version for a conflict. The message is fixed
// per class: the store's text never crosses the wire, because it may name
// other instances and runs; the handler logs it with the request ID. On the
// client every class arrives as the store returned it ([RemoteError]
// unwraps to it), a refusal as [ErrForbidden] (definite, wrapping
// store.ErrInvalid) and a server that could not answer as [ErrUnavailable]
// (indeterminate). A 200 is believed only with the handler's request ID
// and media type, and a call that returns nothing only on the handler's
// {"applied": true}. Anything else is indeterminate. The client never
// retries: the host rereads before retrying (ADR 0003, item 14).
package storehttp
