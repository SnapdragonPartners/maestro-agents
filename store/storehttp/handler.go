package storehttp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"runtime/debug"
	"slices"
	"strconv"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/strictjson"
)

// Defaults for [HandlerOptions].
const (
	// DefaultMaxArtifactBytes is the largest artifact a commit may carry
	// unless the host sets another (ADR 0024, item 9).
	DefaultMaxArtifactBytes int64 = 64 << 20
	// DefaultMaxBodyBytes bounds a request's whole body unless the host
	// sets another: a JSON body, or a commit's JSON part and its artifact
	// parts together.
	DefaultMaxBodyBytes int64 = 128 << 20
)

// modulePath is this library's module, whose version healthz reports.
const modulePath = "github.com/SnapdragonPartners/maestro-agents"

// healthProbe is the instance whose ownership record healthz reads when
// the store has no Ping. It need not exist: an instance never claimed
// has an empty record.
const healthProbe = "storehttp-health-probe"

// errTooLarge is a body or an artifact over its limit: invalid, and
// answered with HTTP 413.
var errTooLarge = fmt.Errorf("%w: request too large", store.ErrInvalid)

// HandlerOptions configures [NewHandler].
type HandlerOptions struct {
	// Authorizer decides every call but healthz. Required: a host whose
	// authentication alone decides passes [AllowAll].
	Authorizer Authorizer
	// Logger receives each error response's inner error, with its
	// request ID, operation and class; nil uses slog.Default().
	Logger *slog.Logger
	// Version overrides the module version healthz reports, which is
	// otherwise read from the binary's build information.
	Version string
	// MaxArtifactBytes is the largest artifact content a commit may carry;
	// zero means DefaultMaxArtifactBytes. Larger content is refused with
	// store.ErrInvalid before the store is called.
	MaxArtifactBytes int64
	// MaxBodyBytes bounds a request's whole body; zero means
	// DefaultMaxBodyBytes. A larger body is refused with store.ErrInvalid.
	MaxBodyBytes int64
}

// handler serves a store.
type handler struct {
	s           store.Store
	auth        Authorizer
	log         *slog.Logger
	mux         *http.ServeMux
	version     string
	maxArtifact int64
	maxBody     int64
}

// NewHandler serves s under /v1/ (ADR 0026). It authenticates nothing: the
// host wraps it with its own authentication and decides each call with
// o.Authorizer. Every route but GET /v1/healthz is authorized.
func NewHandler(s store.Store, o HandlerOptions) (http.Handler, error) {
	if s == nil {
		return nil, errors.New("storehttp: NewHandler needs a store")
	}
	if o.Authorizer == nil {
		return nil, errors.New("storehttp: NewHandler needs an Authorizer (pass AllowAll{} to allow every call)")
	}
	if o.MaxArtifactBytes < 0 || o.MaxBodyBytes < 0 {
		return nil, errors.New("storehttp: negative size limit")
	}
	h := &handler{
		s: s, auth: o.Authorizer, log: o.Logger, version: o.Version,
		maxArtifact: o.MaxArtifactBytes, maxBody: o.MaxBodyBytes, mux: http.NewServeMux(),
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	if h.version == "" {
		h.version = buildVersion()
	}
	if h.maxArtifact == 0 {
		h.maxArtifact = DefaultMaxArtifactBytes
	}
	if h.maxBody == 0 {
		h.maxBody = DefaultMaxBodyBytes
	}
	routes := []struct {
		fn      func(*http.Request) (any, error)
		pattern string
		op      Operation
	}{
		{h.commit, "POST /v1/instances/{instance}/commit", OpCommit},
		{h.claim, "POST /v1/instances/{instance}/claim", OpClaim},
		{h.heartbeat, "POST /v1/instances/{instance}/heartbeat", OpHeartbeat},
		{h.release, "POST /v1/instances/{instance}/release", OpRelease},
		{h.enqueueTrigger, "POST /v1/instances/{instance}/triggers", OpEnqueueTrigger},
		{h.removeTrigger, "DELETE /v1/instances/{instance}/triggers/{id}", OpRemoveTrigger},
		{h.createRun, "POST /v1/instances/{instance}/runs", OpCreateRun},
		{h.requestCancel, "POST /v1/runs/{run}/cancel", OpRequestCancel},
		{h.cancelRequest, "GET /v1/runs/{run}/cancel", OpCancelRequest},
		{h.ownership, "GET /v1/instances/{instance}/ownership", OpOwnership},
		{h.activeRun, "GET /v1/instances/{instance}/active-run", OpActiveRun},
		{h.triggers, "GET /v1/instances/{instance}/triggers", OpTriggers},
		{h.runs, "GET /v1/instances/{instance}/runs", OpRuns},
		{h.latestCheckpoint, "GET /v1/instances/{instance}/checkpoints/latest", OpLatestCheckpoint},
		{h.checkpoint, "GET /v1/instances/{instance}/checkpoints/{seq}", OpCheckpoint},
		{h.run, "GET /v1/runs/{run}", OpRun},
		{h.entries, "GET /v1/runs/{run}/entries", OpEntries},
		{h.object, "GET /v1/namespaces/{namespace}/object", OpObject},
		{h.listObjects, "GET /v1/namespaces/{namespace}/objects", OpListObjects},
		{h.claimable, "GET /v1/claimable", OpClaimable},
		{h.appendEvents, "POST /v1/events", OpAppendEvents},
		{h.events, "GET /v1/runs/{run}/events", OpEvents},
		{h.status, "GET /v1/runs/{run}/status", OpStatus},
		{h.instanceStatus, "GET /v1/instances/{instance}/status", OpInstanceStatus},
	}
	for _, rt := range routes {
		h.mux.Handle(rt.pattern, h.serve(rt.op, rt.fn))
	}
	h.mux.HandleFunc("GET /v1/instances/{instance}/artifacts/{digest}", h.artifact)
	h.mux.HandleFunc("GET /v1/healthz", h.health)
	return h.mux, nil
}

// buildVersion is the module's version from the binary's build
// information.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(unknown)"
	}
	if info.Main.Path == modulePath {
		return info.Main.Version
	}
	for _, d := range info.Deps {
		if d.Path == modulePath {
			if d.Replace != nil {
				return d.Replace.Version
			}
			return d.Version
		}
	}
	return "(unknown)"
}

// newRequestID draws a request's ID.
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// serve adapts a route that answers one JSON value.
func (h *handler) serve(op Operation, fn func(*http.Request) (any, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		r.Body = http.MaxBytesReader(w, r.Body, h.maxBody)
		v, err := fn(r)
		if err != nil {
			h.fail(w, r, id, op, err)
			return
		}
		body, err := json.Marshal(v)
		if err != nil {
			h.fail(w, r, id, op, fmt.Errorf("encoding the response: %w", err))
			return
		}
		header(w, id, contentJSON)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}

// header sets the headers every response carries.
func header(w http.ResponseWriter, id, contentType string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(headerRequestID, id)
}

// fail answers an error: its class, a fixed message and the request ID,
// never the error's text, which goes to the log.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, id string, op Operation, err error) {
	class, status, conflict := classify(err)
	level := slog.LevelDebug
	switch class {
	case classUnavailable:
		level = slog.LevelError
	case classInvalid, classForbidden:
		level = slog.LevelInfo
	}
	h.log.Log(r.Context(), level, "storehttp: call failed",
		"request_id", id, "op", string(op), "class", class, "error", err.Error())
	body, _ := json.Marshal(errorBody{Class: class, Message: message(class), RequestID: id, Conflict: conflict})
	header(w, id, contentJSON)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// authorize asks the Authorizer, turning its answer into the error the
// call returns: nil, one wrapping ErrUnavailable, or one wrapping
// ErrForbidden that carries the refusal's text for the log.
func (h *handler) authorize(ctx context.Context, c Call) error {
	err := h.auth.Authorize(ctx, c)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrUnavailable):
		// The Authorizer's error is text for the log, never part of the
		// chain, so it cannot change the call's class.
		return fmt.Errorf("authorizer could not decide: %v: %w", err, ErrUnavailable) //nolint:errorlint // see above
	}
	return fmt.Errorf("%w: authorizer refused: %v", ErrForbidden, err) //nolint:errorlint // the refusal is text for the log, never part of the chain
}

// masked is a refusal answered as a missing record (ADR 0024, item 9):
// ErrNotFound, with the refusal's text kept for the log only.
func masked(err error) error {
	if errors.Is(err, ErrForbidden) {
		return fmt.Errorf("%w (masked: %v)", store.ErrNotFound, err) //nolint:errorlint // ErrForbidden must leave the chain: the call answers ErrNotFound
	}
	return err
}

// authorizeRun authorizes a call that names only a run on the run's
// instance, read from its run record; a refusal is masked.
func (h *handler) authorizeRun(ctx context.Context, op Operation, run string) error {
	rec, err := h.s.Run(ctx, run)
	if err != nil {
		return wrapStore(err)
	}
	return masked(h.authorize(ctx, Call{Op: op, Instance: rec.Instance, Run: run}))
}

// authorizeTelemetryRun authorizes a telemetry call for one run: on its
// instance if its run record exists, else as a store-wide call.
func (h *handler) authorizeTelemetryRun(ctx context.Context, op Operation, run string) error {
	rec, err := h.s.Run(ctx, run)
	switch {
	case err == nil:
		return h.authorize(ctx, Call{Op: op, Instance: rec.Instance, Run: run})
	case errors.Is(err, store.ErrNotFound):
		return h.authorize(ctx, Call{Op: op, Run: run, StoreWide: true})
	}
	return wrapStore(err)
}

// readBody reads the request's body, within the body limit.
func readBody(r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, errTooLarge
		}
		return nil, fmt.Errorf("reading the request body: %w", err)
	}
	return data, nil
}

// decodeBody strictly decodes a JSON request body into v.
func decodeBody(r *http.Request, v any) error {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != contentJSON {
		return fmt.Errorf("%w: request body must be application/json", store.ErrInvalid)
	}
	data, err := readBody(r)
	if err != nil {
		return err
	}
	if err := strictjson.Decode(data, v); err != nil {
		return fmt.Errorf("%w: request body: %w", store.ErrInvalid, err)
	}
	return nil
}

// query returns the request's query parameters, refusing a name not in
// allowed and a repeated one unless repeatable.
func query(r *http.Request, allowed []string, repeatable ...string) (url.Values, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("%w: query: %w", store.ErrInvalid, err)
	}
	for name, vs := range q {
		if !slices.Contains(allowed, name) {
			return nil, fmt.Errorf("%w: unknown query parameter %q", store.ErrInvalid, name)
		}
		if len(vs) > 1 && !slices.Contains(repeatable, name) {
			return nil, fmt.Errorf("%w: query parameter %q repeated", store.ErrInvalid, name)
		}
	}
	return q, nil
}

// uintParam parses an optional unsigned query parameter, zero when absent.
func uintParam(q url.Values, name string) (uint64, error) {
	if !q.Has(name) {
		return 0, nil
	}
	n, err := strconv.ParseUint(q.Get(name), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: query parameter %q is not an unsigned integer", store.ErrInvalid, name)
	}
	return n, nil
}

// afterParam reads a route's only query parameter, after.
func afterParam(r *http.Request) (uint64, error) {
	q, err := query(r, []string{"after"})
	if err != nil {
		return 0, err
	}
	return uintParam(q, "after")
}

// noQuery refuses any query parameter.
func noQuery(r *http.Request) error {
	_, err := query(r, nil)
	return err
}

// sameInstance refuses a body that names another instance than the path.
func sameInstance(path, body string) error {
	if path != body {
		return fmt.Errorf("%w: the request names instance %q, its path %q", store.ErrInvalid, body, path)
	}
	return nil
}

// lease converts a wire lease, in nanoseconds.
func lease(n int64) time.Duration { return time.Duration(n) }

// wrapStore wraps the store's error for the log; its class survives.
func wrapStore(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("storehttp: %w", err)
}

// answer pairs a store call's result with its error, wrapped.
func answer[T any](v T, err error) (any, error) {
	if err != nil {
		return nil, wrapStore(err)
	}
	return v, nil
}

// empty is the body of a call that returns nothing.
type empty struct{}

// The routes.

func (h *handler) commit(r *http.Request) (any, error) {
	if err := noQuery(r); err != nil {
		return nil, err
	}
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("%w: commit content type: %w", store.ErrInvalid, err)
	}
	var req commitRequest
	var parts *multipart.Reader
	switch mt {
	case contentJSON:
		if err = decodeBody(r, &req); err != nil {
			return nil, err
		}
	case "multipart/mixed":
		parts = multipart.NewReader(r.Body, params["boundary"])
		if err = h.commitJSONPart(parts, &req); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: commit must be application/json or multipart/mixed", store.ErrInvalid)
	}
	inst := r.PathValue("instance")
	if err = sameInstance(inst, req.Fence.Instance); err != nil {
		return nil, err
	}
	f := req.Fence
	if err = h.authorize(r.Context(), Call{Op: OpCommit, Instance: inst, Fence: &f, Namespaces: namespaces(req.Ops)}); err != nil {
		return nil, err
	}
	if err = h.artifactParts(parts, req.Ops); err != nil {
		return nil, err
	}
	return empty{}, wrapStore(h.s.Commit(r.Context(), req.Fence, req.Ops))
}

// commitJSONPart reads a multipart commit's first part, the operations.
func (h *handler) commitJSONPart(parts *multipart.Reader, req *commitRequest) error {
	p, err := parts.NextPart()
	if err != nil {
		return partError(err, "the commit's first part")
	}
	if mt, _, perr := mime.ParseMediaType(p.Header.Get("Content-Type")); perr != nil || mt != contentJSON {
		return fmt.Errorf("%w: a multipart commit's first part must be application/json", store.ErrInvalid)
	}
	data, err := io.ReadAll(p)
	if err != nil {
		return partError(err, "the commit's first part")
	}
	if err = strictjson.Decode(data, req); err != nil {
		return fmt.Errorf("%w: commit: %w", store.ErrInvalid, err)
	}
	return nil
}

// artifactParts fills each put_artifact operation's content from its part
// (ADR 0026, item 3): one part per put_artifact operation, in operation
// order, each naming its digest, within the artifact limit. Content never
// travels in the JSON. A put_artifact with no part is the extension
// reserved for #28 and is refused today.
func (h *handler) artifactParts(parts *multipart.Reader, ops []store.Op) error {
	for i, op := range ops {
		a := op.PutArtifact
		if a == nil {
			continue
		}
		if a.Content != nil {
			return fmt.Errorf("%w: operation %d: artifact content travels in its own part, never in the JSON", store.ErrInvalid, i)
		}
		if parts == nil {
			return fmt.Errorf("%w: operation %d: a commit with artifacts is multipart/mixed, one part per artifact", store.ErrInvalid, i)
		}
		p, err := parts.NextPart()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: operation %d: artifact %s has no part", store.ErrInvalid, i, a.Digest)
		}
		if err != nil {
			return partError(err, "an artifact part")
		}
		if got := p.Header.Get(headerArtifactDigest); got != a.Digest {
			return fmt.Errorf("%w: operation %d: part names artifact %q, the operation %q", store.ErrInvalid, i, got, a.Digest)
		}
		content, err := io.ReadAll(io.LimitReader(p, h.maxArtifact+1))
		if err != nil {
			return partError(err, "an artifact part")
		}
		if int64(len(content)) > h.maxArtifact {
			return fmt.Errorf("%w: artifact %s is over %d bytes", errTooLarge, a.Digest, h.maxArtifact)
		}
		a.Content = content
	}
	if parts == nil {
		return nil
	}
	if _, err := parts.NextPart(); !errors.Is(err, io.EOF) {
		if err != nil {
			return partError(err, "the commit's parts")
		}
		return fmt.Errorf("%w: the commit has more parts than artifacts", store.ErrInvalid)
	}
	return nil
}

// partError classifies a failure reading a multipart body.
func partError(err error, what string) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return errTooLarge
	}
	return fmt.Errorf("%w: reading %s: %w", store.ErrInvalid, what, err)
}

// namespaces returns the namespaces a commit's operations touch, each
// once, in first-use order.
func namespaces(ops []store.Op) []string {
	var out []string
	add := func(ns string) {
		if !slices.Contains(out, ns) {
			out = append(out, ns)
		}
	}
	for _, op := range ops {
		if p := op.PutObject; p != nil {
			add(p.Namespace)
		}
		if d := op.DeleteObject; d != nil {
			add(d.Namespace)
		}
	}
	return out
}

func (h *handler) claim(r *http.Request) (any, error) {
	var req claimRequest
	if err := noQuery(r); err != nil {
		return nil, err
	}
	if err := decodeBody(r, &req); err != nil {
		return nil, err
	}
	inst := r.PathValue("instance")
	if err := h.authorize(r.Context(), Call{Op: OpClaim, Instance: inst, Owner: req.Owner}); err != nil {
		return nil, err
	}
	return answer(h.s.Claim(r.Context(), inst, req.Owner, lease(req.Lease)))
}

func (h *handler) heartbeat(r *http.Request) (any, error) {
	var req heartbeatRequest
	if err := noQuery(r); err != nil {
		return nil, err
	}
	if err := decodeBody(r, &req); err != nil {
		return nil, err
	}
	inst := r.PathValue("instance")
	if err := sameInstance(inst, req.Fence.Instance); err != nil {
		return nil, err
	}
	f := req.Fence
	if err := h.authorize(r.Context(), Call{Op: OpHeartbeat, Instance: inst, Fence: &f}); err != nil {
		return nil, err
	}
	own, cancel, err := h.s.Heartbeat(r.Context(), req.Fence, lease(req.Lease))
	if err != nil {
		return nil, wrapStore(err)
	}
	return heartbeatResponse{Ownership: own, CancelRequest: cancel}, nil
}

func (h *handler) release(r *http.Request) (any, error) {
	var req releaseRequest
	if err := noQuery(r); err != nil {
		return nil, err
	}
	if err := decodeBody(r, &req); err != nil {
		return nil, err
	}
	inst := r.PathValue("instance")
	if err := sameInstance(inst, req.Fence.Instance); err != nil {
		return nil, err
	}
	f := req.Fence
	if err := h.authorize(r.Context(), Call{Op: OpRelease, Instance: inst, Fence: &f}); err != nil {
		return nil, err
	}
	return empty{}, wrapStore(h.s.Release(r.Context(), req.Fence))
}

func (h *handler) enqueueTrigger(r *http.Request) (any, error) {
	var t store.Trigger
	if err := noQuery(r); err != nil {
		return nil, err
	}
	if err := decodeBody(r, &t); err != nil {
		return nil, err
	}
	inst := r.PathValue("instance")
	if err := sameInstance(inst, t.Instance); err != nil {
		return nil, err
	}
	if err := h.authorize(r.Context(), Call{Op: OpEnqueueTrigger, Instance: inst}); err != nil {
		return nil, err
	}
	return answer(h.s.EnqueueTrigger(r.Context(), t))
}

func (h *handler) removeTrigger(r *http.Request) (any, error) {
	if err := noQuery(r); err != nil {
		return nil, err
	}
	inst := r.PathValue("instance")
	if err := h.authorize(r.Context(), Call{Op: OpRemoveTrigger, Instance: inst}); err != nil {
		return nil, err
	}
	return empty{}, wrapStore(h.s.RemoveTrigger(r.Context(), inst, r.PathValue("id")))
}

func (h *handler) createRun(r *http.Request) (any, error) {
	var run store.Run
	if err := noQuery(r); err != nil {
		return nil, err
	}
	if err := decodeBody(r, &run); err != nil {
		return nil, err
	}
	inst := r.PathValue("instance")
	if err := sameInstance(inst, run.Instance); err != nil {
		return nil, err
	}
	if err := h.authorize(r.Context(), Call{Op: OpCreateRun, Instance: inst, Run: run.ID}); err != nil {
		return nil, err
	}
	got, created, err := h.s.CreateRun(r.Context(), run)
	if err != nil {
		return nil, wrapStore(err)
	}
	return createRunResponse{Run: got, Created: created}, nil
}

func (h *handler) requestCancel(r *http.Request) (any, error) {
	var req cancelRequestBody
	if err := noQuery(r); err != nil {
		return nil, err
	}
	if err := decodeBody(r, &req); err != nil {
		return nil, err
	}
	run := r.PathValue("run")
	if err := h.authorizeRun(r.Context(), OpRequestCancel, run); err != nil {
		return nil, err
	}
	got, recorded, err := h.s.RequestCancel(r.Context(), run, req.Cause)
	if err != nil {
		return nil, wrapStore(err)
	}
	return requestCancelResponse{Request: got, Recorded: recorded}, nil
}

func (h *handler) cancelRequest(r *http.Request) (any, error) {
	if err := noQuery(r); err != nil {
		return nil, err
	}
	run := r.PathValue("run")
	if err := h.authorizeRun(r.Context(), OpCancelRequest, run); err != nil {
		return nil, err
	}
	req, ok, err := h.s.CancelRequest(r.Context(), run)
	if err != nil || !ok {
		return cancelReadResponse{}, wrapStore(err)
	}
	return cancelReadResponse{Request: &req}, nil
}

// instanceRead authorizes a read keyed by the path's instance.
func (h *handler) instanceRead(r *http.Request, op Operation) (string, error) {
	if err := noQuery(r); err != nil {
		return "", err
	}
	inst := r.PathValue("instance")
	return inst, h.authorize(r.Context(), Call{Op: op, Instance: inst})
}

func (h *handler) ownership(r *http.Request) (any, error) {
	inst, err := h.instanceRead(r, OpOwnership)
	if err != nil {
		return nil, err
	}
	return answer(h.s.Ownership(r.Context(), inst))
}

func (h *handler) activeRun(r *http.Request) (any, error) {
	inst, err := h.instanceRead(r, OpActiveRun)
	if err != nil {
		return nil, err
	}
	run, ok, err := h.s.ActiveRun(r.Context(), inst)
	if err != nil || !ok {
		return activeRunResponse{}, wrapStore(err)
	}
	return activeRunResponse{Run: &run}, nil
}

func (h *handler) triggers(r *http.Request) (any, error) {
	inst, err := h.instanceRead(r, OpTriggers)
	if err != nil {
		return nil, err
	}
	return answer(h.s.Triggers(r.Context(), inst))
}

func (h *handler) runs(r *http.Request) (any, error) {
	q, err := query(r, []string{"after", "status", "limit", "newest"}, "status")
	if err != nil {
		return nil, err
	}
	var rq store.RunQuery
	rq.After = q.Get("after")
	for _, s := range q["status"] {
		rq.Status = append(rq.Status, store.Status(s))
	}
	if q.Has("limit") {
		if rq.Limit, err = strconv.Atoi(q.Get("limit")); err != nil {
			return nil, fmt.Errorf("%w: query parameter \"limit\" is not an integer", store.ErrInvalid)
		}
	}
	if q.Has("newest") {
		if rq.Newest, err = strconv.ParseBool(q.Get("newest")); err != nil {
			return nil, fmt.Errorf("%w: query parameter \"newest\" is not a boolean", store.ErrInvalid)
		}
	}
	inst := r.PathValue("instance")
	if err = h.authorize(r.Context(), Call{Op: OpRuns, Instance: inst}); err != nil {
		return nil, err
	}
	if q.Has("after") {
		// The cursor names a run: authorized on its own instance, a refusal
		// answering as a missing cursor (ADR 0026, item 4).
		if err = h.authorizeRun(r.Context(), OpRuns, rq.After); err != nil {
			return nil, err
		}
	}
	runs, more, err := h.s.Runs(r.Context(), inst, rq)
	if err != nil {
		return nil, wrapStore(err)
	}
	return runsResponse{Runs: runs, More: more}, nil
}

func (h *handler) latestCheckpoint(r *http.Request) (any, error) {
	inst, err := h.instanceRead(r, OpLatestCheckpoint)
	if err != nil {
		return nil, err
	}
	c, ok, err := h.s.LatestCheckpoint(r.Context(), inst)
	if err != nil || !ok {
		return checkpointResponse{}, wrapStore(err)
	}
	return checkpointResponse{Checkpoint: &c}, nil
}

func (h *handler) checkpoint(r *http.Request) (any, error) {
	seq, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: checkpoint sequence %q is not an unsigned integer", store.ErrInvalid, r.PathValue("seq"))
	}
	inst, err := h.instanceRead(r, OpCheckpoint)
	if err != nil {
		return nil, err
	}
	return answer(h.s.Checkpoint(r.Context(), inst, seq))
}

func (h *handler) run(r *http.Request) (any, error) {
	if err := noQuery(r); err != nil {
		return nil, err
	}
	id := r.PathValue("run")
	rec, err := h.s.Run(r.Context(), id)
	if err != nil {
		return nil, wrapStore(err)
	}
	if err = masked(h.authorize(r.Context(), Call{Op: OpRun, Instance: rec.Instance, Run: id})); err != nil {
		return nil, err
	}
	return rec, nil
}

func (h *handler) entries(r *http.Request) (any, error) {
	after, err := afterParam(r)
	if err != nil {
		return nil, err
	}
	run := r.PathValue("run")
	if err = h.authorizeRun(r.Context(), OpEntries, run); err != nil {
		return nil, err
	}
	return answer(h.s.Entries(r.Context(), run, after))
}

func (h *handler) object(r *http.Request) (any, error) {
	q, err := query(r, []string{"key"})
	if err != nil {
		return nil, err
	}
	if !q.Has("key") {
		return nil, fmt.Errorf("%w: query parameter \"key\" is required", store.ErrInvalid)
	}
	ns := r.PathValue("namespace")
	if err = h.authorize(r.Context(), Call{Op: OpObject, Namespaces: []string{ns}}); err != nil {
		return nil, err
	}
	obj, ok, err := h.s.Object(r.Context(), ns, q.Get("key"))
	if err != nil {
		return nil, wrapStore(err)
	}
	return objectResponse{Object: obj, OK: ok}, nil
}

func (h *handler) listObjects(r *http.Request) (any, error) {
	q, err := query(r, []string{"prefix"})
	if err != nil {
		return nil, err
	}
	ns := r.PathValue("namespace")
	if err = h.authorize(r.Context(), Call{Op: OpListObjects, Namespaces: []string{ns}}); err != nil {
		return nil, err
	}
	return answer(h.s.ListObjects(r.Context(), ns, q.Get("prefix")))
}

func (h *handler) claimable(r *http.Request) (any, error) {
	if err := noQuery(r); err != nil {
		return nil, err
	}
	if err := h.authorize(r.Context(), Call{Op: OpClaimable, StoreWide: true}); err != nil {
		return nil, err
	}
	return answer(h.s.Claimable(r.Context()))
}

// errNoTelemetry answers telemetry on a store without it.
var errNoTelemetry = fmt.Errorf("%w: the store keeps no telemetry", store.ErrInvalid)

func (h *handler) appendEvents(r *http.Request) (any, error) {
	var req eventsRequest
	if err := noQuery(r); err != nil {
		return nil, err
	}
	if err := decodeBody(r, &req); err != nil {
		return nil, err
	}
	tel, ok := h.s.(store.Telemetry)
	if !ok {
		return nil, errNoTelemetry
	}
	// Every event's run is authorized before anything is written, so a
	// refusal refuses the whole call.
	seen := make([]string, 0, len(req.Events))
	for _, e := range req.Events {
		if slices.Contains(seen, e.Run) {
			continue
		}
		seen = append(seen, e.Run)
		if err := h.authorizeTelemetryRun(r.Context(), OpAppendEvents, e.Run); err != nil {
			return nil, err
		}
	}
	return empty{}, wrapStore(tel.AppendEvents(r.Context(), req.Events))
}

func (h *handler) events(r *http.Request) (any, error) {
	after, err := afterParam(r)
	if err != nil {
		return nil, err
	}
	tel, ok := h.s.(store.Telemetry)
	if !ok {
		return nil, errNoTelemetry
	}
	run := r.PathValue("run")
	if err = masked(h.authorizeTelemetryRun(r.Context(), OpEvents, run)); err != nil {
		return nil, err
	}
	return answer(tel.Events(r.Context(), run, after))
}

func (h *handler) status(r *http.Request) (any, error) {
	if err := noQuery(r); err != nil {
		return nil, err
	}
	run := r.PathValue("run")
	if err := h.authorizeRun(r.Context(), OpStatus, run); err != nil {
		return nil, err
	}
	return answer(agent.Status(r.Context(), h.s, run))
}

func (h *handler) instanceStatus(r *http.Request) (any, error) {
	inst, err := h.instanceRead(r, OpInstanceStatus)
	if err != nil {
		return nil, err
	}
	return answer(agent.InstanceStatus(r.Context(), h.s, inst))
}

// artifact serves an artifact's content raw, its metadata in a header.
func (h *handler) artifact(w http.ResponseWriter, r *http.Request) {
	id := newRequestID()
	meta, content, err := h.readArtifact(r)
	if err != nil {
		h.fail(w, r, id, OpArtifact, err)
		return
	}
	m, err := json.Marshal(meta)
	if err != nil {
		h.fail(w, r, id, OpArtifact, err)
		return
	}
	header(w, id, "application/octet-stream")
	w.Header().Set(headerArtifactMeta, base64.StdEncoding.EncodeToString(m))
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, bytes.NewReader(content))
}

func (h *handler) readArtifact(r *http.Request) (store.ArtifactMeta, []byte, error) {
	inst, err := h.instanceRead(r, OpArtifact)
	if err != nil {
		return store.ArtifactMeta{}, nil, err
	}
	meta, content, err := h.s.Artifact(r.Context(), inst, r.PathValue("digest"))
	return meta, content, wrapStore(err)
}

// health answers GET /v1/healthz, unauthorized: the module version, the
// store's schema version if it reports one, and whether it answers a read.
// It is 503 when the store does not.
func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	out := Health{Version: h.version, Store: "ok"}
	if sv, ok := h.s.(interface{ SchemaVersion() int }); ok {
		v := sv.SchemaVersion()
		out.SchemaVersion = &v
	}
	var err error
	if p, ok := h.s.(interface{ Ping(context.Context) error }); ok {
		err = p.Ping(r.Context())
	} else {
		_, err = h.s.Ownership(r.Context(), healthProbe)
	}
	status := http.StatusOK
	id := newRequestID()
	if err != nil {
		out.Store = "unavailable"
		status = http.StatusServiceUnavailable
		h.log.Log(r.Context(), slog.LevelError, "storehttp: health check failed", "request_id", id, "error", err.Error())
	}
	body, _ := json.Marshal(out)
	header(w, id, contentJSON)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
