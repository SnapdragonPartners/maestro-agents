// Package ui is the intake agent's review UI, served by `addepar serve`
// (ADR 0022): the documents and their records, each record with its
// evidence on the page, the review actions (ADR 0021) and the export, as
// HTML pages and as JSON routes under /api/.
//
// It holds no state of its own beyond sessions: every action is a review
// run submitted to the host, and every page is read from the host's store.
// It serves loopback only, behind a session the launch token opens, with a
// Host check and CSRF tokens.
package ui

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/review"
	"github.com/SnapdragonPartners/maestro-agents/standalone"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Backend is what the UI reads and submits to: a *standalone.Host.
type Backend interface {
	review.Submitter
	Store() store.Reader
	Namespace() string
}

// SetupInput is what the first-run setup page collects (ADR 0022, item 4).
type SetupInput struct {
	// Provider is a model provider ID, Model the model's name.
	Provider, Model string
	// Key is the API key.
	Key string
}

// Options configure a Server.
type Options struct {
	// Logger logs requests' failures, never their content.
	Logger *slog.Logger
	// Now is the clock actions are stamped with; nil is time.Now.
	Now func() time.Time
	// Pages renders page images and reads word boxes; nil is Poppler's.
	Pages Pages
	// Setup completes first-run setup and returns the backend, or the
	// errors to show; nil when the workspace is configured.
	Setup func(ctx context.Context, in SetupInput) (Backend, error)
	// Token opens a session (/auth/session?token=) and authorizes a JSON
	// request as a bearer token. Required.
	Token string
	// Addr is the address the server listens on, host and port: requests
	// for any other Host are refused.
	Addr string
	// Inbox is the watched folder, Exports the CSVs' folder and Manifests
	// the manifests' (ADR 0021, item 11).
	Inbox, Exports, Manifests string
}

// Server is the review UI's handler.
type Server struct {
	backend  atomic.Pointer[backendBox]
	pages    *template.Template
	mux      *http.ServeMux
	hosts    map[string]bool
	sessions sessions
	inbox    inboxCache
	opts     Options
	// setupMu serializes setup.
	setupMu sync.Mutex
}

type backendBox struct{ b Backend }

// New builds the server. backend is nil until setup completes.
func New(opts Options, backend Backend) (*Server, error) { //nolint:gocritic // hugeParam: called once
	if len(opts.Token) < 16 {
		return nil, errors.New("ui: the session token is missing or too short")
	}
	hosts, err := allowedHosts(opts.Addr)
	if err != nil {
		return nil, err
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Pages == nil {
		opts.Pages = NewPopplerPages()
	}
	t, err := template.New("").Funcs(funcMap()).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("ui: templates: %w", err)
	}
	s := &Server{opts: opts, pages: t, hosts: hosts, sessions: sessions{m: map[string]*session{}}}
	if backend != nil {
		s.SetBackend(backend)
	}
	s.routes()
	return s, nil
}

// SetBackend makes the UI serve backend, after setup.
func (s *Server) SetBackend(b Backend) { s.backend.Store(&backendBox{b: b}) }

func (s *Server) current() Backend {
	if box := s.backend.Load(); box != nil {
		return box.b
	}
	return nil
}

// allowedHosts lists the Host headers the server answers: its address, by
// IP and as localhost (ADR 0022, item 8).
func allowedHosts(addr string) (map[string]bool, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" || port == "0" {
		return nil, fmt.Errorf("ui: the address %q has no port", addr)
	}
	if !Loopback(host) {
		return nil, fmt.Errorf("ui: the address %q is not a loopback address", addr)
	}
	out := map[string]bool{net.JoinHostPort("localhost", port): true, net.JoinHostPort(host, port): true}
	if ip := net.ParseIP(host); ip != nil {
		out[net.JoinHostPort(ip.String(), port)] = true
	}
	return out, nil
}

// Loopback reports whether host is a loopback address or localhost.
func Loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) routes() {
	m := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	m.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	m.HandleFunc("GET /auth/session", s.authSession)
	m.HandleFunc("GET /name", s.page(s.namePage, false))
	m.HandleFunc("POST /name", s.page(s.setName, false))
	m.HandleFunc("GET /setup", s.page(s.setupPage, false))
	m.HandleFunc("POST /setup", s.page(s.setupSubmit, false))
	m.HandleFunc("GET /{$}", s.page(s.documentsPage, true))
	m.HandleFunc("GET /documents/{sha}", s.page(s.documentPage, true))
	m.HandleFunc("POST /documents/{sha}/manual", s.page(s.manualSubmit, true))
	m.HandleFunc("GET /documents/{sha}/pages/{n}", s.page(s.pageImage, true))
	m.HandleFunc("GET /records/{sha}/{nn}", s.page(s.recordPage, true))
	m.HandleFunc("POST /records/{sha}/{nn}/actions", s.page(s.actionSubmit, true))
	m.HandleFunc("GET /exports", s.page(s.exportsPage, true))
	m.HandleFunc("POST /exports", s.page(s.exportSubmit, true))
	m.HandleFunc("POST /exports/{run}/rewrite", s.page(s.rewriteSubmit, true))
	m.HandleFunc("GET /exports/{run}/csv", s.page(s.exportCSV, true))
	s.apiRoutes(m)
	s.mux = m
}

// ServeHTTP checks the Host, sets the security headers and serves the
// request (ADR 0022, item 8).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	// same-origin, not no-referrer: under no-referrer browsers send
	// "Origin: null" on a same-origin form post, which the Origin check
	// below refuses; same-origin still sends nothing to another site.
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
	if !s.hosts[r.Host] {
		http.Error(w, "unknown host", http.StatusMisdirectedRequest)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	s.mux.ServeHTTP(w, r)
}

// handler is a page's handler: it has a session, and for an unsafe
// method a checked CSRF token.
type handler func(w http.ResponseWriter, r *http.Request, sess *session)

// page wraps a page's handler: a session is required (else 401), a POST
// needs the session's CSRF token, a named page needs the operator's name,
// and a page needing the backend redirects to setup without one.
func (s *Server) page(h handler, needsBackend bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.sessions.fromRequest(r)
		if sess == nil {
			s.unauthorized(w)
			return
		}
		if r.Method == http.MethodPost && !sess.checkCSRF(r.PostFormValue("csrf")) {
			http.Error(w, "the form has expired: reload the page", http.StatusForbidden)
			return
		}
		if needsBackend && s.current() == nil {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		if needsBackend && sess.name() == "" {
			http.Redirect(w, r, "/name?next="+url.QueryEscape(safeNext(r.URL.Path)), http.StatusSeeOther)
			return
		}
		h(w, r, sess)
	}
}

// safeNext keeps a redirect target on this server: a path, never a URL.
func safeNext(p string) string {
	// Browsers drop tabs and newlines from a URL, so "/\t/host" would
	// become "//host": every control character is refused first.
	if strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) ||
		!strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsRune(p, '\\') {
		return "/"
	}
	return p
}

func (s *Server) unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	s.render(w, nil, "signin", nil)
}

// view is every page's data.
type view struct {
	Data     any
	Operator string
	CSRF     string
	Flash    *flash
	Title    string
	Refresh  bool
}

// render executes a page's template within the layout.
func (s *Server) render(w http.ResponseWriter, sess *session, name string, v *view) {
	if v == nil {
		v = &view{}
	}
	if sess != nil {
		v.Operator, v.CSRF, v.Flash = sess.name(), sess.csrf, sess.takeFlash()
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	if err := s.pages.ExecuteTemplate(w, name, v); err != nil {
		s.opts.Logger.Error("ui: render failed", "page", name, "error", err.Error())
	}
}

// fail shows an error page: the store failed, or the request named
// nothing that exists.
func (s *Server) fail(w http.ResponseWriter, sess *session, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	s.render(w, sess, "error", &view{Title: http.StatusText(status), Data: msg})
}

func (s *Server) objects() review.StoreObjects {
	b := s.current()
	return review.StoreObjects{Reader: b.Store(), Namespace: b.Namespace()}
}

// busyMessage is what a person is told when the document is being read.
const busyMessage = "The agent is still working on this document. Try again in a moment."

// submitError turns a submission's error into what the person is told.
func submitError(err error) (msg string, refused bool) {
	if r, ok := review.Refused(err); ok {
		return r.Message, true
	}
	if errors.Is(err, standalone.ErrBusy) {
		return busyMessage, false
	}
	return "The action could not be saved: " + err.Error(), false
}
