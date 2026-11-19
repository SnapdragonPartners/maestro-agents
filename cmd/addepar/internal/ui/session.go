package ui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"unicode/utf8"
)

// cookiePrefix begins the session cookie's name, which ends with the
// server's port (ADR 0022, item 6): browsers scope cookies by host, not
// port, so two servers on one machine must not share a name.
const cookiePrefix = "addepar_session_"

// maxName bounds an operator's name.
const maxName = 100

// session is one browser's sign-in: its CSRF token, the operator's name
// and a message for the next page.
type session struct {
	flash *flash
	csrf  string
	who   string
	mu    sync.Mutex
}

// flash is a one-time message: a refusal, a failure or a confirmation.
type flash struct {
	Message string
	Kind    string // "ok", "refused" or "error"
}

func (s *session) name() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.who
}

func (s *session) setName(n string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.who = n
}

func (s *session) say(kind, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flash = &flash{Kind: kind, Message: msg}
}

func (s *session) takeFlash() *flash {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.flash
	s.flash = nil
	return f
}

func (s *session) checkCSRF(token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.csrf)) == 1
}

// sessions are the process's sign-ins, in memory: they end with it.
type sessions struct {
	m map[string]*session
	// cookie is the session cookie's name.
	cookie string
	// order is the sessions' IDs, oldest first, to drop the oldest past
	// maxSessions.
	order []string
	mu    sync.Mutex
}

// maxSessions bounds the sign-ins kept: the launch token is valid for the
// process's life, so each use of the link opens one, and past this the
// oldest is dropped (its browser signs in again with the link).
const maxSessions = 64

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (ss *sessions) open() string {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	id := randomHex(32)
	for len(ss.order) >= maxSessions {
		delete(ss.m, ss.order[0])
		ss.order = ss.order[1:]
	}
	ss.m[id] = &session{csrf: randomHex(32)}
	ss.order = append(ss.order, id)
	return id
}

func (ss *sessions) fromRequest(r *http.Request) *session {
	c, err := r.Cookie(ss.cookie)
	if err != nil || c.Value == "" {
		return nil
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.m[c.Value]
}

// authSession exchanges the launch token for a session cookie (ADR 0022,
// item 6). The token stays valid for the life of the process.
func (s *Server) authSession(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r.URL.Query().Get("token")) {
		s.unauthorized(w)
		return
	}
	id := s.sessions.open()
	http.SetCookie(w, &http.Cookie{
		Name: s.sessions.cookie, Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) tokenOK(t string) bool {
	return t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(s.opts.Token)) == 1
}

// bearer reports whether the request carries the launch token as a bearer
// token.
func (s *Server) bearer(r *http.Request) bool {
	t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && s.tokenOK(t)
}

func (s *Server) namePage(w http.ResponseWriter, r *http.Request, sess *session) {
	s.render(w, sess, "name", &view{Title: "Your name", Data: safeNext(r.URL.Query().Get("next"))})
}

// cleanName checks an operator's name: printable, trimmed, bounded.
func cleanName(n string) (string, bool) {
	n = strings.TrimSpace(n)
	if n == "" || len(n) > maxName || !utf8.ValidString(n) {
		return "", false
	}
	for _, r := range n {
		if r < ' ' || r == 0x7f {
			return "", false
		}
	}
	return n, true
}

func (s *Server) setName(w http.ResponseWriter, r *http.Request, sess *session) {
	n, ok := cleanName(r.PostFormValue("name"))
	if !ok {
		sess.say("error", "Type your name: it is recorded with everything you do.")
		http.Redirect(w, r, "/name?next="+url.QueryEscape(safeNext(r.PostFormValue("next"))), http.StatusSeeOther)
		return
	}
	sess.setName(n)
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}
