package storehttp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// DefaultMaxResponseBytes bounds a response body the client reads unless
// [ClientOptions] sets another.
const DefaultMaxResponseBytes int64 = 256 << 20

// ClientOptions configures [NewClient].
type ClientOptions struct {
	// HTTPClient sends the requests; nil uses http.DefaultClient. The
	// host's authentication goes in its transport.
	HTTPClient *http.Client
	// Timeout, if positive, bounds each request beside the caller's
	// context.
	Timeout time.Duration
	// MaxResponseBytes bounds a response body; zero means
	// DefaultMaxResponseBytes. A larger one is an indeterminate error.
	MaxResponseBytes int64
}

// Client is a [store.Store] over a handler [NewHandler] serves (ADR 0026),
// and a [store.Telemetry], which answers [store.ErrInvalid] when the
// server's store keeps no telemetry. Every error the server classifies
// arrives as the store returned it ([RemoteError] unwraps to it); anything
// else, a dropped connection, an unparsable body or a status without a
// class included, is indeterminate, and the host rereads before retrying
// (ADR 0003, item 14). The client never retries.
//
// It is safe for concurrent use. Close only stops the client: it returns
// [store.ErrUnusable] from every method afterwards, and leaves the server's
// store open.
type Client struct {
	hc      *http.Client
	base    string
	timeout time.Duration
	maxResp int64
	closed  atomic.Bool
}

var (
	_ store.Store     = (*Client)(nil)
	_ store.Telemetry = (*Client)(nil)
)

// NewClient returns a client for the handler at baseURL, an absolute http
// or https URL under which /v1/ is served.
func NewClient(baseURL string, o ClientOptions) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("storehttp: base URL %q is not an absolute http(s) URL without query or fragment", baseURL)
	}
	if o.Timeout < 0 || o.MaxResponseBytes < 0 {
		return nil, errors.New("storehttp: negative client option")
	}
	c := &Client{hc: o.HTTPClient, base: strings.TrimSuffix(u.String(), "/"), timeout: o.Timeout, maxResp: o.MaxResponseBytes}
	if c.hc == nil {
		c.hc = http.DefaultClient
	}
	if c.maxResp == 0 {
		c.maxResp = DefaultMaxResponseBytes
	}
	return c, nil
}

// Close stops the client.
func (c *Client) Close() error {
	c.closed.Store(true)
	return nil
}

// request is one call: its method, path (already escaped), body and
// content type.
type request struct {
	body        io.Reader
	method      string
	path        string
	contentType string
}

// pathOf builds a path from literal parts and identifiers, escaping each
// identifier as one segment. An empty identifier is refused with
// store.ErrInvalid, as the store refuses one: it cannot be a segment.
func pathOf(parts ...string) (string, error) {
	var b strings.Builder
	b.WriteString("/v1")
	for i := 0; i < len(parts); i += 2 {
		b.WriteString("/")
		b.WriteString(parts[i])
		if i+1 < len(parts) {
			if parts[i+1] == "" {
				return "", fmt.Errorf("%w: empty identifier in %s", store.ErrInvalid, parts[i])
			}
			b.WriteString("/")
			b.WriteString(escape(parts[i+1]))
		}
	}
	return b.String(), nil
}

// do sends a request and returns the response's body and headers for a
// 200, or the error.
func (c *Client) do(ctx context.Context, r request) ([]byte, http.Header, error) {
	if c.closed.Load() {
		return nil, nil, fmt.Errorf("storehttp: client closed: %w", store.ErrUnusable)
	}
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, r.method, c.base+r.path, r.body)
	if err != nil {
		return nil, nil, fmt.Errorf("storehttp: building the request: %w", err)
	}
	if r.contentType != "" {
		req.Header.Set("Content-Type", r.contentType)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("storehttp: %s %s: %w", r.method, r.path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResp+1))
	if err != nil {
		return nil, nil, fmt.Errorf("storehttp: %s %s: reading the response: %w", r.method, r.path, err)
	}
	if int64(len(data)) > c.maxResp {
		return nil, nil, fmt.Errorf("storehttp: %s %s: response over %d bytes", r.method, r.path, c.maxResp)
	}
	if resp.StatusCode == http.StatusOK {
		return data, resp.Header, nil
	}
	var eb errorBody
	if json.Unmarshal(data, &eb) == nil {
		if s := sentinel(eb); s != nil {
			return nil, nil, &RemoteError{err: s, Class: eb.Class, RequestID: eb.RequestID, StatusCode: resp.StatusCode}
		}
	}
	return nil, nil, fmt.Errorf("storehttp: %s %s: unclassified HTTP %d response", r.method, r.path, resp.StatusCode)
}

// call sends a JSON request (in, if not nil) and decodes a JSON response
// into out (if not nil).
func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	r := request{method: method, path: path}
	if in != nil {
		if err := checkStrings(reflect.ValueOf(in)); err != nil {
			return err
		}
		body, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("%w: encoding the request: %w", store.ErrInvalid, err)
		}
		r.body, r.contentType = bytes.NewReader(body), contentJSON
	}
	data, _, err := c.do(ctx, r)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err = json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("storehttp: %s %s: decoding the response: %w", method, path, err)
	}
	return nil
}

// get is a JSON GET of path with query q.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return c.call(ctx, http.MethodGet, path, nil, out)
}

// Commit sends the operations, as one multipart/mixed request when they
// store artifacts: the JSON operations without content, then one part
// per artifact in operation order (ADR 0026, item 3).
func (c *Client) Commit(ctx context.Context, f store.Fence, ops []store.Op) error {
	path, err := pathOf("instances", f.Instance, "commit")
	if err != nil {
		return err
	}
	var contents [][]byte
	var digests []string
	wire := make([]store.Op, len(ops))
	for i, op := range ops {
		wire[i] = op
		if a := op.PutArtifact; a != nil {
			contents = append(contents, a.Content)
			digests = append(digests, a.Digest)
			stripped := *a
			stripped.Content = nil
			wire[i].PutArtifact = &stripped
		}
	}
	req := commitRequest{Fence: f, Ops: wire}
	if err = checkStrings(reflect.ValueOf(req)); err != nil {
		return err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("%w: encoding the commit: %w", store.ErrInvalid, err)
	}
	if len(contents) == 0 {
		_, _, err = c.do(ctx, request{method: http.MethodPost, path: path, body: bytes.NewReader(body), contentType: contentJSON})
		return err
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	pw, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {contentJSON}})
	if err == nil {
		_, err = pw.Write(body)
	}
	for i := 0; err == nil && i < len(contents); i++ {
		pw, err = mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":       {"application/octet-stream"},
			headerArtifactDigest: {digests[i]},
		})
		if err == nil {
			_, err = pw.Write(contents[i])
		}
	}
	if err == nil {
		err = mw.Close()
	}
	if err != nil {
		return fmt.Errorf("%w: encoding the commit: %w", store.ErrInvalid, err)
	}
	_, _, err = c.do(ctx, request{
		method: http.MethodPost, path: path, body: &buf,
		contentType: "multipart/mixed; boundary=" + mw.Boundary(),
	})
	return err
}

// Claim takes ownership of an instance.
func (c *Client) Claim(ctx context.Context, instance, owner string, lease time.Duration) (store.Ownership, error) {
	path, err := pathOf("instances", instance, "claim")
	if err != nil {
		return store.Ownership{}, err
	}
	var own store.Ownership
	err = c.call(ctx, http.MethodPost, path, claimRequest{Owner: owner, Lease: int64(lease)}, &own)
	return own, err
}

// Heartbeat extends the fenced owner's lease.
func (c *Client) Heartbeat(ctx context.Context, f store.Fence, lease time.Duration) (store.Ownership, *store.CancelRequest, error) {
	path, err := pathOf("instances", f.Instance, "heartbeat")
	if err != nil {
		return store.Ownership{}, nil, err
	}
	var out heartbeatResponse
	if err = c.call(ctx, http.MethodPost, path, heartbeatRequest{Fence: f, Lease: int64(lease)}, &out); err != nil {
		return store.Ownership{}, nil, err
	}
	return out.Ownership, out.CancelRequest, nil
}

// Release clears the fenced owner.
func (c *Client) Release(ctx context.Context, f store.Fence) error {
	path, err := pathOf("instances", f.Instance, "release")
	if err != nil {
		return err
	}
	return c.call(ctx, http.MethodPost, path, releaseRequest{Fence: f}, nil)
}

// EnqueueTrigger queues a trigger.
func (c *Client) EnqueueTrigger(ctx context.Context, t store.Trigger) (store.Trigger, error) {
	path, err := pathOf("instances", t.Instance, "triggers")
	if err != nil {
		return store.Trigger{}, err
	}
	var out store.Trigger
	err = c.call(ctx, http.MethodPost, path, t, &out)
	return out, err
}

// RemoveTrigger removes a queued trigger.
func (c *Client) RemoveTrigger(ctx context.Context, instance, id string) error {
	path, err := pathOf("instances", instance, "triggers", id)
	if err != nil {
		return err
	}
	return c.call(ctx, http.MethodDelete, path, nil, nil)
}

// CreateRun creates a run record.
func (c *Client) CreateRun(ctx context.Context, r store.Run) (store.Run, bool, error) {
	path, err := pathOf("instances", r.Instance, "runs")
	if err != nil {
		return store.Run{}, false, err
	}
	var out createRunResponse
	if err = c.call(ctx, http.MethodPost, path, r, &out); err != nil {
		return store.Run{}, false, err
	}
	return out.Run, out.Created, nil
}

// RequestCancel records a cancel request for a run.
func (c *Client) RequestCancel(ctx context.Context, run string, cause store.CancelCause) (store.CancelRequest, bool, error) {
	path, err := pathOf("runs", run, "cancel")
	if err != nil {
		return store.CancelRequest{}, false, err
	}
	var out requestCancelResponse
	if err = c.call(ctx, http.MethodPost, path, cancelRequestBody{Cause: cause}, &out); err != nil {
		return store.CancelRequest{}, false, err
	}
	return out.Request, out.Recorded, nil
}

// Ownership returns an instance's ownership record.
func (c *Client) Ownership(ctx context.Context, instance string) (store.Ownership, error) {
	path, err := pathOf("instances", instance, "ownership")
	if err != nil {
		return store.Ownership{}, err
	}
	var out store.Ownership
	err = c.get(ctx, path, nil, &out)
	return out, err
}

// Claimable returns the ready runs.
func (c *Client) Claimable(ctx context.Context) ([]store.Claimable, error) {
	var out []store.Claimable
	err := c.get(ctx, "/v1/claimable", nil, &out)
	return out, err
}

// Run returns a run record.
func (c *Client) Run(ctx context.Context, id string) (store.Run, error) {
	path, err := pathOf("runs", id)
	if err != nil {
		return store.Run{}, err
	}
	var out store.Run
	err = c.get(ctx, path, nil, &out)
	return out, err
}

// CancelRequest returns a run's cancel request, if it has one.
func (c *Client) CancelRequest(ctx context.Context, run string) (store.CancelRequest, bool, error) {
	path, err := pathOf("runs", run, "cancel")
	if err != nil {
		return store.CancelRequest{}, false, err
	}
	var out cancelReadResponse
	if err = c.get(ctx, path, nil, &out); err != nil || out.Request == nil {
		return store.CancelRequest{}, false, err
	}
	return *out.Request, true, nil
}

// ActiveRun returns an instance's non-terminal run, if it has one.
func (c *Client) ActiveRun(ctx context.Context, instance string) (store.Run, bool, error) {
	path, err := pathOf("instances", instance, "active-run")
	if err != nil {
		return store.Run{}, false, err
	}
	var out activeRunResponse
	if err = c.get(ctx, path, nil, &out); err != nil || out.Run == nil {
		return store.Run{}, false, err
	}
	return *out.Run, true, nil
}

// Runs returns a page of an instance's runs.
func (c *Client) Runs(ctx context.Context, instance string, q store.RunQuery) ([]store.Run, bool, error) {
	path, err := pathOf("instances", instance, "runs")
	if err != nil {
		return nil, false, err
	}
	v := url.Values{}
	if q.After != "" {
		v.Set("after", q.After)
	}
	for _, s := range q.Status {
		v.Add("status", string(s))
	}
	if q.Limit != 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Newest {
		v.Set("newest", "true")
	}
	var out runsResponse
	if err = c.get(ctx, path, v, &out); err != nil {
		return nil, false, err
	}
	return out.Runs, out.More, nil
}

// Triggers returns an instance's queued triggers.
func (c *Client) Triggers(ctx context.Context, instance string) ([]store.Trigger, error) {
	path, err := pathOf("instances", instance, "triggers")
	if err != nil {
		return nil, err
	}
	var out []store.Trigger
	err = c.get(ctx, path, nil, &out)
	return out, err
}

// LatestCheckpoint returns an instance's latest checkpoint, if it has
// any.
func (c *Client) LatestCheckpoint(ctx context.Context, instance string) (store.Checkpoint, bool, error) {
	path, err := pathOf("instances", instance, "checkpoints")
	if err != nil {
		return store.Checkpoint{}, false, err
	}
	var out checkpointResponse
	if err = c.get(ctx, path+"/latest", nil, &out); err != nil || out.Checkpoint == nil {
		return store.Checkpoint{}, false, err
	}
	return *out.Checkpoint, true, nil
}

// Checkpoint returns one checkpoint.
func (c *Client) Checkpoint(ctx context.Context, instance string, seq uint64) (store.Checkpoint, error) {
	path, err := pathOf("instances", instance, "checkpoints")
	if err != nil {
		return store.Checkpoint{}, err
	}
	var out store.Checkpoint
	err = c.get(ctx, path+"/"+strconv.FormatUint(seq, 10), nil, &out)
	return out, err
}

// Entries returns a run's entries after a sequence number.
func (c *Client) Entries(ctx context.Context, run string, after uint64) ([]store.Entry, error) {
	path, err := pathOf("runs", run, "entries")
	if err != nil {
		return nil, err
	}
	var out []store.Entry
	err = c.get(ctx, path, afterQuery(after), &out)
	return out, err
}

// afterQuery is the query of a read after a sequence number.
func afterQuery(after uint64) url.Values {
	if after == 0 {
		return nil
	}
	return url.Values{"after": {strconv.FormatUint(after, 10)}}
}

// Artifact returns an artifact linked to the instance. The content's
// digest and size are checked against the metadata; a mismatch is an
// indeterminate error.
func (c *Client) Artifact(ctx context.Context, instance, digest string) (store.ArtifactMeta, []byte, error) {
	path, err := pathOf("instances", instance, "artifacts", digest)
	if err != nil {
		return store.ArtifactMeta{}, nil, err
	}
	content, h, err := c.do(ctx, request{method: http.MethodGet, path: path})
	if err != nil {
		return store.ArtifactMeta{}, nil, err
	}
	var meta store.ArtifactMeta
	raw, err := base64.StdEncoding.DecodeString(h.Get(headerArtifactMeta))
	if err == nil {
		err = json.Unmarshal(raw, &meta)
	}
	if err != nil {
		return store.ArtifactMeta{}, nil, fmt.Errorf("storehttp: artifact %s: metadata header: %w", digest, err)
	}
	sum := sha256.Sum256(content)
	if hex.EncodeToString(sum[:]) != meta.Digest || meta.Digest != digest || int64(len(content)) != meta.Size {
		return store.ArtifactMeta{}, nil, fmt.Errorf("storehttp: artifact %s: content does not match its metadata", digest)
	}
	return meta, content, nil
}

// Object returns an object.
func (c *Client) Object(ctx context.Context, namespace, key string) (store.Object, bool, error) {
	path, err := pathOf("namespaces", namespace, "object")
	if err != nil {
		return store.Object{}, false, err
	}
	var out objectResponse
	if err = c.get(ctx, path, url.Values{"key": {key}}, &out); err != nil {
		return store.Object{}, false, err
	}
	return out.Object, out.OK, nil
}

// ListObjects returns the keys in a namespace that start with prefix.
func (c *Client) ListObjects(ctx context.Context, namespace, prefix string) ([]store.ObjectInfo, error) {
	path, err := pathOf("namespaces", namespace, "objects")
	if err != nil {
		return nil, err
	}
	var q url.Values
	if prefix != "" {
		q = url.Values{"prefix": {prefix}}
	}
	var out []store.ObjectInfo
	err = c.get(ctx, path, q, &out)
	return out, err
}

// AppendEvents stores telemetry events.
func (c *Client) AppendEvents(ctx context.Context, events []store.Event) error {
	return c.call(ctx, http.MethodPost, "/v1/events", eventsRequest{Events: events}, nil)
}

// Events returns a run's telemetry events after a sequence number.
func (c *Client) Events(ctx context.Context, run string, after uint64) ([]store.Event, error) {
	path, err := pathOf("runs", run, "events")
	if err != nil {
		return nil, err
	}
	var out []store.Event
	err = c.get(ctx, path, afterQuery(after), &out)
	return out, err
}

// Status returns a run's derived status ([agent.Status]) from the server
// in one call, rather than the several reads agent.Status makes through
// the client.
func (c *Client) Status(ctx context.Context, run string) (agent.RunStatus, error) {
	path, err := pathOf("runs", run, "status")
	if err != nil {
		return agent.RunStatus{}, err
	}
	var out agent.RunStatus
	err = c.get(ctx, path, nil, &out)
	return out, err
}

// InstanceStatus returns an instance's derived status
// ([agent.InstanceStatus]) from the server in one call.
func (c *Client) InstanceStatus(ctx context.Context, instance string) (agent.InstanceReport, error) {
	path, err := pathOf("instances", instance, "status")
	if err != nil {
		return agent.InstanceReport{}, err
	}
	var out agent.InstanceReport
	err = c.get(ctx, path, nil, &out)
	return out, err
}

// Health returns the server's health. A server whose store does not
// answer replies 503 with the same body, which Health returns with no
// error.
func (c *Client) Health(ctx context.Context) (Health, error) {
	if c.closed.Load() {
		return Health{}, fmt.Errorf("storehttp: client closed: %w", store.ErrUnusable)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/healthz", http.NoBody)
	if err != nil {
		return Health{}, fmt.Errorf("storehttp: building the request: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return Health{}, fmt.Errorf("storehttp: healthz: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return Health{}, fmt.Errorf("storehttp: healthz: HTTP %d", resp.StatusCode)
	}
	var h Health
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err == nil {
		err = json.Unmarshal(data, &h)
	}
	if err != nil {
		return Health{}, fmt.Errorf("storehttp: healthz: HTTP %d: %w", resp.StatusCode, err)
	}
	return h, nil
}

// checkStrings refuses a request holding a string that is not valid UTF-8
// with store.ErrInvalid, as the store refuses it: encoding/json would
// replace the invalid bytes with U+FFFD, so the server would see, and the
// store keep, another string than the caller's. Raw JSON values travel
// byte for byte and the handler refuses invalid UTF-8 in them; identifiers
// in a path or query are percent-encoded byte for byte and the store
// refuses them.
func checkStrings(v reflect.Value) error {
	switch v.Kind() {
	case reflect.String:
		if !utf8.ValidString(v.String()) {
			return fmt.Errorf("%w: a string in the request is not valid UTF-8", store.ErrInvalid)
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			return checkStrings(v.Elem())
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				if err := checkStrings(v.Field(i)); err != nil {
					return err
				}
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		for i := range v.Len() {
			if err := checkStrings(v.Index(i)); err != nil {
				return err
			}
		}
	}
	return nil
}
