package ui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/review"
	"github.com/SnapdragonPartners/maestro-agents/internal/strictjson"
	"github.com/SnapdragonPartners/maestro-agents/standalone"
)

// maxBody bounds a request body: a record is at most 1 MiB.
const maxBody = 2 << 20

// apiRoutes registers the JSON routes (ADR 0022, item 13).
func (s *Server) apiRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/documents", s.api(s.apiDocuments))
	m.HandleFunc("GET /api/records/{sha}/{nn}", s.api(s.apiRecord))
	m.HandleFunc("POST /api/records/{sha}/{nn}/actions", s.api(s.apiAction))
	m.HandleFunc("POST /api/documents/{sha}/manual", s.api(s.apiManual))
	m.HandleFunc("GET /api/exports", s.api(s.apiExports))
	m.HandleFunc("POST /api/exports", s.api(s.apiExport))
}

// caller is who a JSON request acts for: a session's operator, or, with
// the bearer token, the name the body gives.
type caller struct {
	sess   *session
	bearer bool
}

func (c caller) by(bodyBy string) (string, bool) {
	if c.bearer {
		return cleanName(bodyBy)
	}
	n := c.sess.name()
	return n, n != ""
}

type apiHandler func(w http.ResponseWriter, r *http.Request, c caller)

// api wraps a JSON route: the bearer token, or a session cookie and, for
// a POST, its CSRF token in X-CSRF-Token (ADR 0022, items 6 and 8).
func (s *Server) api(h apiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := caller{bearer: s.bearer(r)}
		if !c.bearer {
			c.sess = s.sessions.fromRequest(r)
			if c.sess == nil {
				writeJSON(w, http.StatusUnauthorized, apiError{Error: "unauthorized"})
				return
			}
			if r.Method == http.MethodPost && !c.sess.checkCSRF(r.Header.Get("X-CSRF-Token")) {
				writeJSON(w, http.StatusForbidden, apiError{Error: "missing or wrong X-CSRF-Token"})
				return
			}
		}
		if s.current() == nil {
			writeJSON(w, http.StatusServiceUnavailable, apiError{Error: "setup is not finished"})
			return
		}
		h(w, r, c)
	}
}

type apiError struct {
	Refusal *review.Refusal `json:"refusal,omitempty"`
	Error   string          `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) apiFailed(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNotFound):
		writeJSON(w, http.StatusNotFound, apiError{Error: "not found"})
	case errors.Is(err, standalone.ErrBusy):
		w.Header().Set("Retry-After", "2")
		writeJSON(w, http.StatusServiceUnavailable, apiError{Error: busyMessage})
	default:
		if r, ok := review.Refused(err); ok {
			status := http.StatusConflict
			if r.Code == review.RefuseNoRecord || r.Code == review.RefuseNoDocument {
				status = http.StatusNotFound // as GET answers an unknown record
			}
			writeJSON(w, status, apiError{Error: "refused", Refusal: r})
			return
		}
		var fe *formError
		if errors.As(err, &fe) {
			writeJSON(w, http.StatusBadRequest, apiError{Error: fe.msg})
			return
		}
		s.opts.Logger.Error("ui: request failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
	}
}

func (s *Server) apiDocuments(w http.ResponseWriter, r *http.Request, _ caller) {
	docs, err := s.documents(r.Context())
	if err != nil {
		s.apiFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": docs})
}

// apiRecordView is a record as the JSON route returns it.
type apiRecordView struct {
	Current  any           `json:"current"`
	Document *DocumentView `json:"document"`
	Record   RecordView    `json:"record"`
	Blockers []string      `json:"blockers"`
	Accepted []string      `json:"accepted_warnings"`
	// Seen is what an action's seen must be.
	Seen int `json:"seen"`
}

func (s *Server) apiRecord(w http.ResponseWriter, r *http.Request, _ caller) {
	d, err := s.record(r.Context(), r.PathValue("sha"), r.PathValue("nn"))
	if err != nil {
		s.apiFailed(w, err)
		return
	}
	v := apiRecordView{Record: d.Summary, Current: d.State.Record, Blockers: d.Blockers, Document: d.Document, Seen: d.State.Entries, Accepted: []string{}}
	if v.Blockers == nil {
		v.Blockers = []string{}
	}
	for _, f := range d.Findings {
		if f.Accepted {
			v.Accepted = append(v.Accepted, f.ID)
		}
	}
	writeJSON(w, http.StatusOK, v)
}

// readAction decodes a JSON action strictly, as the review run will.
func readAction(r *http.Request) (*review.Action, error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		return nil, formErr("the body cannot be read: %v", err)
	}
	a, err := review.DecodeAction(body)
	if err != nil {
		return nil, formErr("%v", err)
	}
	return a, nil
}

// apiSubmit sets who and when, submits and answers.
func (s *Server) apiSubmit(w http.ResponseWriter, r *http.Request, c caller, a *review.Action) {
	by, ok := c.by(a.By)
	if !ok {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "name who acts: set your name, or give by"})
		return
	}
	a.By, a.At = by, s.opts.Now().UTC()
	run, err := review.Submit(r.Context(), s.current(), s.objects(), a)
	if err != nil {
		s.apiFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"run": run})
}

func (s *Server) apiAction(w http.ResponseWriter, r *http.Request, c caller) {
	a, err := readAction(r)
	if err != nil {
		s.apiFailed(w, err)
		return
	}
	id := r.PathValue("sha") + "/" + r.PathValue("nn")
	if _, _, ok := review.SplitRecordID(id); !ok {
		s.apiFailed(w, errNotFound)
		return
	}
	switch a.Kind {
	case review.Correct, review.AcceptWarning, review.SetEntities, review.Approve:
	default:
		writeJSON(w, http.StatusBadRequest, apiError{Error: "a record's action is correct, accept_warning, set_entities or approve"})
		return
	}
	if a.Record != "" && a.Record != id {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "the action names another record"})
		return
	}
	a.Record, a.Document = id, ""
	s.apiSubmit(w, r, c, a)
}

func (s *Server) apiManual(w http.ResponseWriter, r *http.Request, c caller) {
	a, err := readAction(r)
	if err != nil {
		s.apiFailed(w, err)
		return
	}
	sha := r.PathValue("sha")
	if !shaOK(sha) {
		s.apiFailed(w, errNotFound)
		return
	}
	if a.Kind != review.Manual || a.Record != "" || (a.Document != "" && a.Document != sha) {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "a document's action is manual, naming no record"})
		return
	}
	a.Document = sha
	s.apiSubmit(w, r, c, a)
}

func (s *Server) apiExports(w http.ResponseWriter, r *http.Request, _ caller) {
	d, err := s.exportsData(r.Context())
	if err != nil {
		s.apiFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"waiting": d.Waiting, "exports": d.Past})
}

// apiExportRequest is POST /api/exports' body.
type apiExportRequest struct {
	By      string   `json:"by,omitempty"`
	Records []string `json:"records"`
}

func (s *Server) apiExport(w http.ResponseWriter, r *http.Request, c caller) {
	var req apiExportRequest
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err == nil {
		err = strictjson.Decode(body, &req)
	}
	if err != nil || len(req.Records) == 0 {
		writeJSON(w, http.StatusBadRequest, apiError{Error: `the body is {"records": ["<sha256>/<NN>", ...]}`})
		return
	}
	by, ok := c.by(req.By)
	if !ok {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "name who exports: set your name, or give by"})
		return
	}
	res, err := s.export(r.Context(), by, req.Records)
	if err != nil {
		s.apiFailed(w, err)
		return
	}
	out := map[string]any{"run": res.Exported.Run, "records": len(res.Exported.Records), "csv": res.CSVPath}
	if res.WriteErr != nil {
		out["write_error"] = res.WriteErr.Error()
		writeJSON(w, http.StatusInternalServerError, out)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
