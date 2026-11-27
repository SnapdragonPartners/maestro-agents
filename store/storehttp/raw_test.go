package storehttp_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/memstore"
	"github.com/SnapdragonPartners/maestro-agents/store/storehttp"
)

// rawEnv is a handler over a memory store with instance "inst" claimed,
// reached with raw HTTP requests.
type rawEnv struct {
	inner store.Store
	rec   *recorder
	srv   *httptest.Server
	fence store.Fence
}

func newRawEnv(t *testing.T, o storehttp.HandlerOptions) rawEnv {
	t.Helper()
	inner := memstore.New()
	ctx := context.Background()
	_, _, err := inner.CreateRun(ctx, newRun("run", "inst"))
	must(t, err)
	own, err := inner.Claim(ctx, "inst", "owner", time.Hour)
	must(t, err)
	rec := &recorder{}
	o.Authorizer, o.Logger = rec, quiet
	h, err := storehttp.NewHandler(inner, o)
	must(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return rawEnv{inner: inner, rec: rec, srv: srv, fence: own.Fence()}
}

// wireError is the error body as a test reads it.
type wireError struct {
	Class     string `json:"class"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// send makes a raw request and returns its status and error body.
func (e rawEnv) send(t testing.TB, method, path, contentType string, body []byte) (int, wireError) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, e.srv.URL+path, bytes.NewReader(body))
	must(t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := e.srv.Client().Do(req)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	must(t, err)
	var we wireError
	if resp.StatusCode != http.StatusOK {
		if err = json.Unmarshal(data, &we); err != nil {
			t.Fatalf("%s %s: HTTP %d body %q is not the error body", method, path, resp.StatusCode, data)
		}
		if we.RequestID == "" || resp.Header.Get("Maestro-Request-Id") != we.RequestID {
			t.Fatalf("%s %s: request ID %q, header %q", method, path, we.RequestID, resp.Header.Get("Maestro-Request-Id"))
		}
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("%s %s: no nosniff header", method, path)
	}
	return resp.StatusCode, we
}

// part is one part of a multipart commit.
type part struct {
	header textproto.MIMEHeader
	body   []byte
}

func multipartBody(t testing.TB, parts []part) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		w, err := mw.CreatePart(p.header)
		must(t, err)
		_, err = w.Write(p.body)
		must(t, err)
	}
	must(t, mw.Close())
	return buf.Bytes(), "multipart/mixed; boundary=" + mw.Boundary()
}

func jsonPart(t testing.TB, v any) part {
	t.Helper()
	b, err := json.Marshal(v)
	must(t, err)
	return part{header: textproto.MIMEHeader{"Content-Type": {"application/json"}}, body: b}
}

func artifactPart(d string, content []byte) part {
	return part{header: textproto.MIMEHeader{"Content-Type": {"application/octet-stream"}, "Maestro-Artifact-Digest": {d}}, body: content}
}

// commitBody is a commit's JSON with an object write (so a refused commit
// is seen to apply nothing) and put_artifact operations without content.
func commitBody(f store.Fence, digests ...string) map[string]any {
	ops := []any{store.PutObjectJSON("ns", "k", 0, json.RawMessage(`1`))}
	for _, d := range digests {
		ops = append(ops, map[string]any{"kind": "put_artifact", "put_artifact": map[string]any{"digest": d, "media_type": "text/plain", "content": nil}})
	}
	return map[string]any{"fence": f, "ops": ops}
}

// A commit's artifacts travel as parts, one per put_artifact in order,
// each within the limit (ADR 0026, item 3); every malformed shape is
// ErrInvalid with nothing applied.
func TestMultipartCommit(t *testing.T) {
	a, b := []byte("alpha"), []byte("bravo")
	da, db := digest(a), digest(b)
	big := bytes.Repeat([]byte("x"), 65)
	for _, tc := range []struct {
		parts  func(store.Fence) []part
		name   string
		status int
	}{
		{name: "part names another digest", status: 400, parts: func(f store.Fence) []part {
			return []part{jsonPart(t, commitBody(f, da, db)), artifactPart(db, b), artifactPart(da, a)}
		}},
		{name: "content does not match digest", status: 400, parts: func(f store.Fence) []part {
			return []part{jsonPart(t, commitBody(f, da)), artifactPart(da, b)}
		}},
		{name: "part missing", status: 400, parts: func(f store.Fence) []part {
			return []part{jsonPart(t, commitBody(f, da, db)), artifactPart(da, a)}
		}},
		{name: "extra part", status: 400, parts: func(f store.Fence) []part {
			return []part{jsonPart(t, commitBody(f, da)), artifactPart(da, a), artifactPart(db, b)}
		}},
		{name: "over the artifact limit", status: 413, parts: func(f store.Fence) []part {
			return []part{jsonPart(t, commitBody(f, digest(big))), artifactPart(digest(big), big)}
		}},
		{name: "content in the JSON", status: 400, parts: func(f store.Fence) []part {
			body := commitBody(f)
			ops, _ := body["ops"].([]any)
			body["ops"] = append(ops, store.PutArtifactOp(store.Artifact{Digest: da, MediaType: "text/plain", Content: a}))
			return []part{jsonPart(t, body), artifactPart(da, a)}
		}},
		{name: "first part not JSON", status: 400, parts: func(f store.Fence) []part {
			return []part{artifactPart(da, a), jsonPart(t, commitBody(f, da))}
		}},
		{name: "unknown field", status: 400, parts: func(f store.Fence) []part {
			body := commitBody(f, da)
			body["extra"] = true
			return []part{jsonPart(t, body), artifactPart(da, a)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRawEnv(t, storehttp.HandlerOptions{MaxArtifactBytes: 64})
			body, ct := multipartBody(t, tc.parts(e.fence))
			status, we := e.send(t, http.MethodPost, "/v1/instances/inst/commit", ct, body)
			if status != tc.status || we.Class != "invalid" {
				t.Fatalf("status %d class %q, want %d invalid", status, we.Class, tc.status)
			}
			if _, ok, _ := e.inner.Object(context.Background(), "ns", "k"); ok {
				t.Fatal("a refused commit applied its object write")
			}
		})
	}
	t.Run("well formed", func(t *testing.T) {
		e := newRawEnv(t, storehttp.HandlerOptions{})
		body, ct := multipartBody(t, []part{jsonPart(t, commitBody(e.fence, da, db)), artifactPart(da, a), artifactPart(db, b)})
		if status, we := e.send(t, http.MethodPost, "/v1/instances/inst/commit", ct, body); status != 200 {
			t.Fatalf("status %d %+v", status, we)
		}
		for d, want := range map[string][]byte{da: a, db: b} {
			if _, got, err := e.inner.Artifact(context.Background(), "inst", d); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("Artifact(%s) = %q, %v", d, got, err)
			}
		}
	})
	t.Run("artifact in a JSON commit", func(t *testing.T) {
		e := newRawEnv(t, storehttp.HandlerOptions{})
		for _, op := range []any{
			store.PutArtifactOp(store.Artifact{Digest: da, MediaType: "text/plain", Content: a}),
			// The extension reserved for #28: a put_artifact with no part.
			map[string]any{"kind": "put_artifact", "put_artifact": map[string]any{"digest": da, "media_type": "text/plain", "content": nil}},
		} {
			body, err := json.Marshal(map[string]any{"fence": e.fence, "ops": []any{op}})
			must(t, err)
			if status, we := e.send(t, http.MethodPost, "/v1/instances/inst/commit", "application/json", body); status != 400 || we.Class != "invalid" {
				t.Fatalf("status %d class %q, want 400 invalid", status, we.Class)
			}
		}
	})
}

// Raw requests the client never sends: each is refused before the store
// (and, where the refusal is the request's shape, before the Authorizer).
func TestRawRequests(t *testing.T) {
	e := newRawEnv(t, storehttp.HandlerOptions{MaxBodyBytes: 1 << 10})
	f := e.fence
	other := store.Fence{Instance: "other", Owner: f.Owner, Epoch: f.Epoch}
	enc := func(v any) []byte {
		b, err := json.Marshal(v)
		must(t, err)
		return b
	}
	for _, tc := range []struct {
		name, method, path, ct string
		body                   []byte
		status                 int
	}{
		{"commit names another instance", "POST", "/v1/instances/inst/commit", "application/json",
			enc(map[string]any{"fence": other, "ops": []any{store.ReleaseOp()}}), 400},
		{"heartbeat names another instance", "POST", "/v1/instances/inst/heartbeat", "application/json",
			enc(map[string]any{"fence": other, "lease": int64(time.Minute)}), 400},
		{"release names another instance", "POST", "/v1/instances/inst/release", "application/json",
			enc(map[string]any{"fence": other}), 400},
		{"run names another instance", "POST", "/v1/instances/inst/runs", "application/json", enc(newRun("r2", "other")), 400},
		{"trigger names another instance", "POST", "/v1/instances/inst/triggers", "application/json",
			enc(store.Trigger{Instance: "other", ID: "t"}), 400},
		{"unknown field", "POST", "/v1/instances/inst/claim", "application/json", []byte(`{"owner":"o","lease":1,"x":1}`), 400},
		{"key in another case", "POST", "/v1/instances/inst/claim", "application/json", []byte(`{"Owner":"o","lease":1}`), 400},
		{"repeated key", "POST", "/v1/instances/inst/claim", "application/json", []byte(`{"owner":"o","owner":"p","lease":1}`), 400},
		{"trailing data", "POST", "/v1/instances/inst/claim", "application/json", []byte(`{"owner":"o","lease":1} {}`), 400},
		{"not JSON content type", "POST", "/v1/instances/inst/claim", "text/plain", []byte(`{"owner":"o","lease":1}`), 400},
		{"commit content type", "POST", "/v1/instances/inst/commit", "text/plain", []byte(`{}`), 400},
		{"body over the limit", "POST", "/v1/instances/inst/claim", "application/json",
			[]byte(`{"owner":"` + strings.Repeat("o", 2<<10) + `","lease":1}`), 413},
		{"unknown query parameter", "GET", "/v1/instances/inst/ownership?x=1", "", nil, 400},
		{"repeated query parameter", "GET", "/v1/runs/run/entries?after=1&after=2", "", nil, 400},
		{"bad after", "GET", "/v1/runs/run/entries?after=-1", "", nil, 400},
		{"bad limit", "GET", "/v1/instances/inst/runs?limit=x", "", nil, 400},
		{"bad newest", "GET", "/v1/instances/inst/runs?newest=maybe", "", nil, 400},
		{"bad checkpoint sequence", "GET", "/v1/instances/inst/checkpoints/x", "", nil, 400},
		{"object without key", "GET", "/v1/namespaces/ns/object", "", nil, 400},
	} {
		status, we := e.send(t, tc.method, tc.path, tc.ct, tc.body)
		if status != tc.status || we.Class != "invalid" {
			t.Fatalf("%s: status %d class %q, want %d invalid", tc.name, status, we.Class, tc.status)
		}
		if calls := e.rec.take(); len(calls) != 0 {
			t.Fatalf("%s: reached the Authorizer: %+v", tc.name, calls)
		}
	}
	if own, _ := e.inner.Ownership(context.Background(), "inst"); own.Owner != f.Owner {
		t.Fatalf("ownership changed: %+v", own)
	}
}

// A server whose artifact content does not match its metadata is not
// believed: the client's error is indeterminate.
func TestArtifactMetadataChecked(t *testing.T) {
	content := []byte("content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		meta, _ := json.Marshal(store.ArtifactMeta{Digest: digest(content), MediaType: "text/plain", Size: int64(len(content))})
		w.Header().Set("Maestro-Artifact-Meta", base64.StdEncoding.EncodeToString(meta))
		_, _ = w.Write([]byte("tampered"))
	}))
	t.Cleanup(srv.Close)
	c, err := storehttp.NewClient(srv.URL, storehttp.ClientOptions{HTTPClient: srv.Client()})
	must(t, err)
	if _, _, err = c.Artifact(context.Background(), "inst", digest(content)); err == nil || store.IsDefinite(err) {
		t.Fatalf("Artifact = %v, want indeterminate", err)
	}
}

// A multipart commit over the body limit, or cut short, is refused with
// nothing applied.
func TestMultipartBodyLimits(t *testing.T) {
	content := bytes.Repeat([]byte("y"), 4<<10)
	d := digest(content)
	t.Run("over the body limit", func(t *testing.T) {
		e := newRawEnv(t, storehttp.HandlerOptions{MaxBodyBytes: 2 << 10})
		body, ct := multipartBody(t, []part{jsonPart(t, commitBody(e.fence, d)), artifactPart(d, content)})
		if status, we := e.send(t, http.MethodPost, "/v1/instances/inst/commit", ct, body); status != 413 || we.Class != "invalid" {
			t.Fatalf("status %d class %q, want 413 invalid", status, we.Class)
		}
		if _, ok, _ := e.inner.Object(context.Background(), "ns", "k"); ok {
			t.Fatal("a refused commit applied its object write")
		}
	})
	t.Run("cut short", func(t *testing.T) {
		e := newRawEnv(t, storehttp.HandlerOptions{})
		body, ct := multipartBody(t, []part{jsonPart(t, commitBody(e.fence, d)), artifactPart(d, content)})
		if status, we := e.send(t, http.MethodPost, "/v1/instances/inst/commit", ct, body[:len(body)-100]); status != 400 || we.Class != "invalid" {
			t.Fatalf("status %d class %q, want 400 invalid", status, we.Class)
		}
		if _, ok, _ := e.inner.Object(context.Background(), "ns", "k"); ok {
			t.Fatal("a refused commit applied its object write")
		}
	})
}
