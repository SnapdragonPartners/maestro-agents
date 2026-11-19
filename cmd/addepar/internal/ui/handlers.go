package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-cms/extract/pdf/pdftoppm"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/export"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/review"
)

func (s *Server) documentsPage(w http.ResponseWriter, r *http.Request, sess *session) {
	docs, err := s.documents(r.Context())
	if err != nil {
		s.storeFailed(w, sess, err)
		return
	}
	s.render(w, sess, "documents", &view{Title: "Documents", Data: docs, Refresh: working(docs)})
}

// storeFailed shows a store failure: the system's, never the person's.
func (s *Server) storeFailed(w http.ResponseWriter, sess *session, err error) {
	s.opts.Logger.Error("ui: cannot read the store", "error", err.Error())
	s.fail(w, sess, http.StatusInternalServerError, "The agent's records cannot be read: "+err.Error())
}

// documentData is the document page's data.
type documentData struct {
	Doc  *DocumentView
	Form RecordForm
	Seen int
	// Open shows the manual form open: a draft being extended.
	Open bool
}

func (s *Server) documentPage(w http.ResponseWriter, r *http.Request, sess *session) {
	s.renderDocument(w, r, sess, nil)
}

// renderDocument shows the document page; draft, when set, is the manual
// form's posted values to show again (more rows, or after a failure).
func (s *Server) renderDocument(w http.ResponseWriter, r *http.Request, sess *session, draft url.Values) {
	sha := r.PathValue("sha")
	if refuseDraft(w, r, sess, draft, "/documents/"+sha) {
		return
	}
	if !shaOK(sha) {
		s.fail(w, sess, http.StatusNotFound, "There is no such document.")
		return
	}
	c, err := s.catalog(r.Context())
	if err != nil {
		s.storeFailed(w, sess, err)
		return
	}
	doc, _, err := c.document(r.Context(), sha)
	switch {
	case errors.Is(err, errNotFound):
		s.fail(w, sess, http.StatusNotFound, "There is no such document, or the agent has not read it yet.")
		return
	case err != nil:
		s.storeFailed(w, sess, err)
		return
	}
	d := &documentData{Doc: doc, Form: formOf(nil), Seen: len(doc.Records)}
	if draft != nil {
		d.Form, d.Open = formFromValues(draft), true
		if n, err := strconv.Atoi(draft.Get("seen")); err == nil {
			d.Seen = n
		}
	}
	s.render(w, sess, "document", &view{Title: doc.Name, Data: d, Refresh: doc.Kind == kindWorking && draft == nil})
}

func shaOK(sha string) bool {
	_, _, ok := review.SplitRecordID(sha + "/01")
	return ok
}

func (s *Server) recordPage(w http.ResponseWriter, r *http.Request, sess *session) {
	s.renderRecord(w, r, sess, nil)
}

// refuseDraft is the one gate every draft passes before it is shown again:
// a form past the row limit cannot be shown whole, so it is refused with
// the reason, never truncated. It reports whether it answered.
func refuseDraft(w http.ResponseWriter, r *http.Request, sess *session, draft url.Values, back string) bool {
	if draft == nil || !tooManyRows(draft) {
		return false
	}
	sess.say("error", errTooManyRows.Error())
	http.Redirect(w, r, back, http.StatusSeeOther)
	return true
}

// renderRecord shows the record page; draft, when set, is the correction
// form's posted values to show again (more rows, or after a failure).
func (s *Server) renderRecord(w http.ResponseWriter, r *http.Request, sess *session, draft url.Values) {
	if refuseDraft(w, r, sess, draft, "/records/"+r.PathValue("sha")+"/"+r.PathValue("nn")) {
		return
	}
	d, err := s.record(r.Context(), r.PathValue("sha"), r.PathValue("nn"))
	switch {
	case errors.Is(err, errNotFound):
		s.fail(w, sess, http.StatusNotFound, "There is no such record.")
		return
	case err != nil:
		s.storeFailed(w, sess, err)
		return
	}
	if draft != nil {
		d.Form, d.CorrectOpen = formFromValues(draft), true
		if n, err := strconv.Atoi(draft.Get("seen")); err == nil {
			d.CorrectSeen = n
		}
	}
	s.render(w, sess, "record", &view{Title: d.Summary.Investor + " · " + d.Summary.Fund, Data: d})
}

func (s *Server) pageImage(w http.ResponseWriter, r *http.Request, _ *session) {
	sha := r.PathValue("sha")
	n, err := strconv.Atoi(r.PathValue("n"))
	if !shaOK(sha) || err != nil || n < 1 {
		http.NotFound(w, r)
		return
	}
	data, err := s.pdfBytes(r.Context(), sha)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	img, err := s.opts.Pages.Render(r.Context(), sha, data, n)
	if errors.Is(err, pdftoppm.ErrPageOutOfRange) {
		http.NotFound(w, r) // a page the PDF does not have
		return
	}
	if err != nil {
		s.opts.Logger.Warn("ui: cannot render a page", "document", sha, "page", n, "error", err.Error())
		http.Error(w, "the page cannot be shown", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(img)
}

// actionOf reads a record page's posted action (ADR 0021, item 7).
func actionOf(form url.Values, id string) (*review.Action, error) {
	seen, err := strconv.Atoi(form.Get("seen"))
	if err != nil {
		return nil, formErr("the form is incomplete: reload the page")
	}
	a := &review.Action{Kind: review.Kind(form.Get("kind")), Record: id, Seen: seen, Reason: strings.TrimSpace(form.Get("reason"))}
	switch a.Kind {
	case review.Correct:
		if a.Extraction, err = parseRecordForm(form); err != nil {
			return nil, err
		}
	case review.AcceptWarning:
		a.Finding = form.Get("finding")
	case review.SetEntities:
		a.Owner = entityOf(form, "owner")
		a.Asset = entityOf(form, "asset")
	case review.Approve:
	default:
		return nil, formErr("unknown action %q", a.Kind)
	}
	return a, nil
}

// entityOf reads an owner or asset; both fields blank clears it. Spaces
// are trimmed here: a person's padding is not a different name.
func entityOf(form url.Values, prefix string) *export.Entity {
	id, name := strings.TrimSpace(form.Get(prefix+".id")), strings.TrimSpace(form.Get(prefix+".name"))
	if id == "" && name == "" {
		return nil
	}
	return &export.Entity{ID: id, Name: name}
}

func (s *Server) actionSubmit(w http.ResponseWriter, r *http.Request, sess *session) {
	sha, nn := r.PathValue("sha"), r.PathValue("nn")
	id := sha + "/" + nn
	back := "/records/" + id
	if _, _, ok := review.SplitRecordID(id); !ok {
		s.fail(w, sess, http.StatusNotFound, "There is no such record.")
		return
	}
	if r.PostFormValue("kind") == string(review.Correct) && wantsRows(r.PostForm) {
		s.renderRecord(w, r, sess, r.PostForm)
		return
	}
	a, err := actionOf(r.PostForm, id)
	if err != nil {
		sess.say("error", err.Error())
	} else {
		switch s.submit(r.Context(), sess, a) {
		case submitted:
			http.Redirect(w, r, back, http.StatusSeeOther)
			return
		case submitStale:
			r.PostForm.Del("seen") // the page shows the record as it is now
		}
	}
	// A correction that failed keeps the person's draft: the page comes
	// back with what they typed, never an empty form.
	if r.PostFormValue("kind") == string(review.Correct) {
		s.renderRecord(w, r, sess, r.PostForm)
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// outcome is what became of a submitted action.
type outcome int

const (
	submitted outcome = iota
	// submitFailed is a refusal or a failure, already told to the person.
	submitFailed
	// submitStale is a refusal for a stale view: a draft shown again
	// takes the record's current log length, since the page it comes back
	// on shows the record as it is now, so the person can save it after
	// checking; never the stale one, which would
	// be refused forever.
	submitStale
)

// staleMessage tells the person their view was stale and the draft kept.
const staleMessage = "This changed while you were editing: the page now shows it as it is. Your draft is kept below; check it against the record, then save again."

// submit runs an action as the session's operator and tells the person
// what became of it.
func (s *Server) submit(ctx context.Context, sess *session, a *review.Action) outcome {
	a.By, a.At = sess.name(), s.opts.Now().UTC()
	if _, err := review.Submit(ctx, s.current(), s.objects(), a); err != nil {
		if ref, ok := review.Refused(err); ok && ref.Code == review.RefuseStale && (a.Kind == review.Correct || a.Kind == review.Manual) {
			sess.say("refused", staleMessage)
			return submitStale
		}
		msg, refused := submitError(err)
		kind := "error"
		if refused {
			kind = "refused"
		} else {
			s.opts.Logger.Error("ui: action failed", "kind", kindName(a.Kind), "error", oneLine(err.Error()))
		}
		sess.say(kind, msg)
		return submitFailed
	}
	sess.say("ok", doneMessage(a.Kind))
	return submitted
}

// kindName names a known action kind for a log line, never the request's
// own text.
func kindName(k review.Kind) string {
	switch k {
	case review.Correct:
		return "correct"
	case review.Manual:
		return "manual"
	case review.AcceptWarning:
		return "accept_warning"
	case review.SetEntities:
		return "set_entities"
	case review.Approve:
		return "approve"
	case review.Export:
		return "export"
	}
	return "unknown"
}

// oneLine keeps a logged value on one line.
func oneLine(s string) string { return strings.NewReplacer("\n", " ", "\r", " ").Replace(s) }

func doneMessage(k review.Kind) string {
	switch k {
	case review.Correct:
		return "Correction saved and verified again."
	case review.Manual:
		return "Record entered and verified."
	case review.AcceptWarning:
		return "Warning accepted."
	case review.SetEntities:
		return "Owner and asset saved."
	case review.Approve:
		return "Approved."
	}
	return "Saved."
}

func (s *Server) manualSubmit(w http.ResponseWriter, r *http.Request, sess *session) {
	sha := r.PathValue("sha")
	if !shaOK(sha) {
		s.fail(w, sess, http.StatusNotFound, "There is no such document.")
		return
	}
	back := "/documents/" + sha
	if wantsRows(r.PostForm) {
		s.renderDocument(w, r, sess, r.PostForm)
		return
	}
	seen, err := strconv.Atoi(r.PostFormValue("seen"))
	if err != nil {
		sess.say("error", "The form is incomplete: reload the page.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	ext, err := parseRecordForm(r.PostForm)
	if err != nil {
		sess.say("error", err.Error())
		s.renderDocument(w, r, sess, r.PostForm) // keeps the draft
		return
	}
	a := &review.Action{Kind: review.Manual, Document: sha, Seen: seen, Extraction: ext, Reason: strings.TrimSpace(r.PostFormValue("reason"))}
	switch s.submit(r.Context(), sess, a) {
	case submitted:
	case submitStale:
		r.PostForm.Del("seen") // the page shows the document's records as they are now
		s.renderDocument(w, r, sess, r.PostForm)
		return
	default:
		s.renderDocument(w, r, sess, r.PostForm) // keeps the draft
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/records/%s/%02d", sha, seen+1), http.StatusSeeOther)
}

// ExportView is a past export.
type ExportView struct {
	At      time.Time `json:"at"`
	Run     string    `json:"run"`
	By      string    `json:"by"`
	CSVName string    `json:"csv_name"`
	CSVSHA  string    `json:"csv_sha256"`
	Records int       `json:"records"`
}

// exportsData is the exports page's data.
type exportsData struct {
	Waiting []RecordView
	Past    []ExportView
}

func (s *Server) exportsData(ctx context.Context) (*exportsData, error) {
	objs := s.objects()
	ids, err := review.Unexported(ctx, objs)
	if err != nil {
		return nil, err //nolint:wrapcheck // review names it
	}
	d := &exportsData{Waiting: []RecordView{}, Past: []ExportView{}}
	for _, id := range ids {
		st, lerr := review.Load(ctx, objs, id)
		if lerr != nil {
			return nil, lerr //nolint:wrapcheck // review names it
		}
		d.Waiting = append(d.Waiting, summarize(id, &st, false))
	}
	exports, err := review.Exports(ctx, objs)
	if err != nil {
		return nil, err //nolint:wrapcheck // review names it
	}
	d.Past = pastExports(exports)
	return d, nil
}

// pastExports lists the stored exports newest first: their keys are run
// IDs, random, so the store's key order says nothing about when.
func pastExports(exports []review.Exported) []ExportView {
	out := []ExportView{}
	for i := range exports {
		e := &exports[i]
		if e.Export == nil {
			continue
		}
		out = append(out, ExportView{At: e.At, Run: e.Run, By: e.By, CSVName: e.Export.CSVName(), CSVSHA: e.Export.CSVSHA256, Records: len(e.Records)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].At.Equal(out[j].At) {
			return out[i].Run < out[j].Run
		}
		return out[i].At.After(out[j].At)
	})
	return out
}

func (s *Server) exportsPage(w http.ResponseWriter, r *http.Request, sess *session) {
	d, err := s.exportsData(r.Context())
	if err != nil {
		s.storeFailed(w, sess, err)
		return
	}
	s.render(w, sess, "exports", &view{Title: "Export", Data: d})
}

// exportResult is what an export did: its run, and the files it wrote or
// why they were not written.
type exportResult struct {
	Exported *review.Exported
	// WriteErr is a write that failed after the commit: the export is
	// stored, and its files can be written again (ADR 0021, item 11).
	WriteErr error
	CSVPath  string
}

// export runs an export of ids and writes its files.
func (s *Server) export(ctx context.Context, by string, ids []string) (*exportResult, error) {
	exp, err := review.SubmitExport(ctx, s.current(), s.objects(), by, s.opts.Now(), ids)
	if err != nil {
		return nil, err //nolint:wrapcheck // review names it
	}
	res := &exportResult{Exported: exp}
	res.CSVPath, _, res.WriteErr = exp.Export.Write(s.opts.Exports, s.opts.Manifests)
	return res, nil
}

func (s *Server) exportSubmit(w http.ResponseWriter, r *http.Request, sess *session) {
	ids := r.PostForm["record"]
	if len(ids) == 0 {
		sess.say("error", "Choose at least one record to export.")
		http.Redirect(w, r, "/exports", http.StatusSeeOther)
		return
	}
	res, err := s.export(r.Context(), sess.name(), ids)
	switch {
	case err != nil:
		msg, refused := submitError(err)
		if refused {
			sess.say("refused", "The export was refused: "+msg)
		} else {
			sess.say("error", msg)
		}
	case res.WriteErr != nil:
		sess.say("error", fmt.Sprintf("The export is saved, but its files could not be written: %v. Fix the problem, then use Write again on the export below.", res.WriteErr))
	default:
		sess.say("ok", fmt.Sprintf("Exported %d record(s) to %s.", len(res.Exported.Records), res.CSVPath))
	}
	http.Redirect(w, r, "/exports", http.StatusSeeOther)
}

func (s *Server) rewriteSubmit(w http.ResponseWriter, r *http.Request, sess *session) {
	exp, err := review.StoredExport(r.Context(), s.objects(), r.PathValue("run"))
	switch {
	case errors.Is(err, review.ErrNoExport):
		sess.say("error", "There is no such export.")
	case err != nil:
		sess.say("error", err.Error())
	default:
		if p, _, werr := exp.Export.Write(s.opts.Exports, s.opts.Manifests); werr != nil {
			sess.say("error", fmt.Sprintf("The files could not be written: %v", werr))
		} else {
			sess.say("ok", "Written: "+p)
		}
	}
	http.Redirect(w, r, "/exports", http.StatusSeeOther)
}

func (s *Server) exportCSV(w http.ResponseWriter, r *http.Request, _ *session) {
	exp, err := review.StoredExport(r.Context(), s.objects(), r.PathValue("run"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", exp.Export.CSVName()))
	_, _ = w.Write(exp.Export.CSV)
}
