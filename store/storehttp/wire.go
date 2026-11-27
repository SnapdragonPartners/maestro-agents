package storehttp

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Errors the client returns besides the store's own. Each is matched with
// [errors.Is].
var (
	// ErrForbidden is a call the handler's [Authorizer] refused, where
	// the refusal is not masked as a missing record. It wraps
	// [store.ErrInvalid]: it is definite, nothing was applied, and a host
	// does not reread and retry it as it would an indeterminate error.
	ErrForbidden = fmt.Errorf("storehttp: forbidden: %w", store.ErrInvalid)

	// ErrUnavailable is a call the server could not answer: the store
	// behind it failed, or an [Authorizer] could not decide. It is
	// indeterminate ([store.IsDefinite] is false): the host rereads
	// before retrying (ADR 0003, item 14). An Authorizer returns an error
	// wrapping it to say it could not decide, rather than that it
	// refuses.
	ErrUnavailable = errors.New("storehttp: store unavailable")
)

// The error classes of the wire's error body.
const (
	classOwnership    = "ownership"
	classInvalid      = "invalid"
	classNotFound     = "not_found"
	classNotClaimable = "not_claimable"
	classInstanceBusy = "instance_busy"
	classConflict     = "conflict"
	classForbidden    = "forbidden"
	classUnavailable  = "unavailable"
)

// errorBody is the body of every error response. Message is fixed per
// class: the inner error's text never crosses the wire, since a store's
// messages name other instances and runs (ADR 0026, item 6).
type errorBody struct {
	Conflict  *conflictBody `json:"conflict,omitempty"`
	Class     string        `json:"class"`
	Message   string        `json:"message"`
	RequestID string        `json:"request_id"`
}

// conflictBody carries a [store.ConflictError] without its Detail.
type conflictBody struct {
	Kind    store.OpKind `json:"kind"`
	Op      int          `json:"op"`
	Current uint64       `json:"current"`
}

// classify maps a store error to its wire class and HTTP status.
func classify(err error) (class string, status int, conflict *conflictBody) {
	var c *store.ConflictError
	switch {
	case errors.As(err, &c):
		return classConflict, http.StatusConflict, &conflictBody{Kind: c.Kind, Op: c.Op, Current: c.Current}
	case errors.Is(err, ErrForbidden):
		return classForbidden, http.StatusForbidden, nil
	case errors.Is(err, store.ErrOwnership):
		return classOwnership, http.StatusConflict, nil
	case errors.Is(err, store.ErrNotClaimable):
		return classNotClaimable, http.StatusConflict, nil
	case errors.Is(err, store.ErrInstanceBusy):
		return classInstanceBusy, http.StatusConflict, nil
	case errors.Is(err, store.ErrNotFound):
		return classNotFound, http.StatusNotFound, nil
	case errors.Is(err, errTooLarge):
		return classInvalid, http.StatusRequestEntityTooLarge, nil
	case errors.Is(err, store.ErrInvalid):
		return classInvalid, http.StatusBadRequest, nil
	}
	return classUnavailable, http.StatusServiceUnavailable, nil
}

// message is a class's fixed message.
func message(class string) string {
	switch class {
	case classOwnership:
		return "ownership lost"
	case classInvalid:
		return "invalid request"
	case classNotFound:
		return "not found"
	case classNotClaimable:
		return "instance has a live owner"
	case classInstanceBusy:
		return "instance has a non-terminal run"
	case classConflict:
		return "precondition failed"
	case classForbidden:
		return "forbidden"
	}
	return "store unavailable"
}

// sentinel returns the error a class arrives as on the client, or nil for
// a class the client does not know.
func sentinel(b errorBody) error {
	switch b.Class {
	case classOwnership:
		return store.ErrOwnership
	case classInvalid:
		return store.ErrInvalid
	case classNotFound:
		return store.ErrNotFound
	case classNotClaimable:
		return store.ErrNotClaimable
	case classInstanceBusy:
		return store.ErrInstanceBusy
	case classForbidden:
		return ErrForbidden
	case classUnavailable:
		return ErrUnavailable
	case classConflict:
		if b.Conflict == nil {
			return nil
		}
		return &store.ConflictError{
			Kind: b.Conflict.Kind, Op: b.Conflict.Op, Current: b.Conflict.Current,
			Detail: "detail withheld by the server (ADR 0026)",
		}
	}
	return nil
}

// RemoteError is an error the server answered with. It unwraps to the
// store's error of its class ([store.ErrInvalid], a [*store.ConflictError]
// and the rest), to [ErrForbidden] or to [ErrUnavailable], so errors.Is,
// errors.As and [store.IsDefinite] see the class as the store returned it.
// RequestID finds the server's log line, which holds the inner error's
// text the wire withholds.
type RemoteError struct {
	err        error
	Class      string
	RequestID  string
	StatusCode int
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("%v (storehttp: HTTP %d, request %s)", e.err, e.StatusCode, e.RequestID)
}

// Unwrap returns the error of the class.
func (e *RemoteError) Unwrap() error { return e.err }

// The request and response envelopes: the store's own types, wrapped only
// where a method takes or returns more than one value.
type (
	commitRequest struct {
		Ops   []store.Op  `json:"ops"`
		Fence store.Fence `json:"fence"`
	}
	claimRequest struct {
		Owner string `json:"owner"`
		// Lease is in nanoseconds (time.Duration's JSON form).
		Lease int64 `json:"lease"`
	}
	heartbeatRequest struct {
		Fence store.Fence `json:"fence"`
		Lease int64       `json:"lease"`
	}
	heartbeatResponse struct {
		CancelRequest *store.CancelRequest `json:"cancel_request"`
		Ownership     store.Ownership      `json:"ownership"`
	}
	releaseRequest struct {
		Fence store.Fence `json:"fence"`
	}
	createRunResponse struct {
		Run     store.Run `json:"run"`
		Created bool      `json:"created"`
	}
	cancelRequestBody struct {
		Cause store.CancelCause `json:"cause"`
	}
	requestCancelResponse struct {
		Request  store.CancelRequest `json:"request"`
		Recorded bool                `json:"recorded"`
	}
	cancelReadResponse struct {
		Request *store.CancelRequest `json:"request"`
	}
	activeRunResponse struct {
		Run *store.Run `json:"run"`
	}
	runsResponse struct {
		Runs []store.Run `json:"runs"`
		More bool        `json:"more"`
	}
	checkpointResponse struct {
		Checkpoint *store.Checkpoint `json:"checkpoint"`
	}
	objectResponse struct {
		Object store.Object `json:"object"`
		OK     bool         `json:"ok"`
	}
	// ack is the body of a call that returns nothing: an explicit
	// acknowledgement, so a client never takes another server's 200 for
	// an applied write (ADR 0026, item 5).
	ack struct {
		Applied bool `json:"applied"`
	}
	eventsRequest struct {
		Events []store.Event `json:"events"`
	}
	// Health is the body of GET /v1/healthz.
	Health struct {
		// Version is the module version the handler was built with.
		Version string `json:"version"`
		// SchemaVersion is the store's schema version, omitted when the
		// store does not report one.
		SchemaVersion *int `json:"schema_version,omitempty"`
		// Store is "ok" when the store answered a read, else
		// "unavailable".
		Store string `json:"store"`
	}
)

// Headers and media types.
const (
	// contentJSON is the media type of every JSON body.
	contentJSON = "application/json"
	// headerArtifactDigest names the digest of an artifact part in a
	// commit.
	headerArtifactDigest = "Maestro-Artifact-Digest"
	// headerArtifactMeta carries an artifact read's metadata, as base64
	// (standard, padded) JSON of [store.ArtifactMeta], since a media type
	// is any UTF-8 string and may not be a valid header value.
	headerArtifactMeta = "Maestro-Artifact-Meta"
	// headerRequestID echoes the request ID of every response.
	headerRequestID = "Maestro-Request-Id"
)

// escape encodes an identifier as one path segment. Every byte outside
// ALPHA / DIGIT / "-" / "_" / "~" is percent-encoded, "." included, so no
// identifier becomes a "." or ".." segment or a slash that a ServeMux
// would clean or redirect (it cleans the escaped path).
func escape(id string) string {
	var b strings.Builder
	for i := range len(id) {
		c := id[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}
