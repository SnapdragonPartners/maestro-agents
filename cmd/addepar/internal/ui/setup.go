package ui

import (
	"net/http"
	"strings"
)

// Provider is a model provider setup offers (ADR 0022, item 4; ADR 0019,
// item 7 covers both for production).
type Provider struct {
	ID, Name, Model string
}

// Providers returns the model providers setup offers, the first the
// default.
func Providers() []Provider {
	return []Provider{
		{ID: "anthropic", Name: "Anthropic", Model: "claude-opus-5-5"},
		{ID: "openai", Name: "OpenAI", Model: "gpt-5"},
	}
}

func providerOK(id string) bool {
	for _, p := range Providers() {
		if p.ID == id {
			return true
		}
	}
	return false
}

// maxKey bounds an API key.
const maxKey = 4096

// setupData is the setup page's data.
type setupData struct {
	Provider  string
	Model     string
	Errors    string
	Providers []Provider
}

func (s *Server) setupPage(w http.ResponseWriter, _ *http.Request, sess *session) {
	if s.current() != nil || s.opts.Setup == nil {
		s.render(w, sess, "setup-done", &view{Title: "Set up"})
		return
	}
	s.render(w, sess, "setup", &view{Title: "Set up", Data: &setupData{Providers: Providers(), Provider: Providers()[0].ID}})
}

// setupSubmit writes the configuration and starts the agent (ADR 0022,
// item 4). It never overwrites a configuration: once a backend runs,
// setup is done.
func (s *Server) setupSubmit(w http.ResponseWriter, r *http.Request, sess *session) {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if s.current() != nil || s.opts.Setup == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	in := SetupInput{
		Provider: r.PostFormValue("provider"), Model: strings.TrimSpace(r.PostFormValue("model")),
		Key: strings.TrimSpace(r.PostFormValue("key")),
	}
	d := &setupData{Providers: Providers(), Provider: in.Provider, Model: in.Model}
	var problems []string
	if !providerOK(in.Provider) {
		problems = append(problems, "Choose a model provider.")
		d.Provider = Providers()[0].ID
	}
	if in.Model == "" || len(in.Model) > 200 || strings.ContainsAny(in.Model, " \t\r\n") {
		problems = append(problems, "Name the model, as the provider spells it.")
	}
	if in.Key == "" || len(in.Key) > maxKey || strings.ContainsAny(in.Key, " \t\r\n") {
		problems = append(problems, "Paste the API key.")
	}
	if len(problems) == 0 {
		b, inbox, err := s.opts.Setup(r.Context(), in)
		if err == nil {
			s.SetBackend(b, inbox)
			sess.say("ok", "The agent is running. Drop statements into the Inbox folder.")
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		s.opts.Logger.Error("ui: setup failed", "error", err.Error())
		problems = append(problems, err.Error())
	}
	d.Errors = strings.Join(problems, "\n")
	w.WriteHeader(http.StatusUnprocessableEntity)
	s.render(w, sess, "setup", &view{Title: "Set up", Data: d})
}
