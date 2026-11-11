package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

func templateFS() fstest.MapFS {
	return fstest.MapFS{
		"system.tmpl":          {Data: []byte(`Record the items. {{template "partials/rules.tmpl" .}}`)},
		"partials/rules.tmpl":  {Data: []byte(`Be brief, {{.Name}}.`)},
		"user.tmpl":            {Data: []byte(`items for {{.Run}}`)},
		"missing.tmpl":         {Data: []byte(`{{.absent}}`)},
		"nested/deep/x.tmpl":   {Data: []byte(`x`)},
		"partials/unused.tmpl": {Data: []byte(`unused`)},
	}
}

// templatedLedger is the ledger type whose work state renders its prompt
// from templates; render is called with the env, and its error, if any, is
// the prompt's.
func templatedLedger(fsys fstest.MapFS, render func(env *agent.Env) (string, string, error)) *agent.Type[ledgerData] {
	typ := ledgerType(&execLog{})
	typ.Templates = fsys
	typ.States[0].Model.Prompt = func(_ context.Context, env *agent.Env, _ ledgerData) (llms.ChatRequest, error) {
		system, user, err := render(env)
		if err != nil {
			return llms.ChatRequest{}, err
		}
		return llms.ChatRequest{System: []llms.ContentPart{llms.Text(system)}, Messages: []llms.Message{llms.UserText(user)}}, nil
	}
	return typ
}

func TestRegisterRefusesTemplates(t *testing.T) {
	cases := map[string]struct {
		fsys fstest.MapFS
		want string
	}{
		"does not parse": {fstest.MapFS{"a.tmpl": {Data: []byte(`{{.x`)}}, "a.tmpl"},
		"defines":        {fstest.MapFS{"a.tmpl": {Data: []byte(`{{define "b"}}b{{end}}a`)}}, "defines templates"},
		"block":          {fstest.MapFS{"a.tmpl": {Data: []byte(`{{block "b" .}}b{{end}}`)}}, "defines templates"},
		"not UTF-8":      {fstest.MapFS{"a.tmpl": {Data: []byte{0xff}}}, "not valid UTF-8"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			typ := ledgerType(&execLog{})
			typ.Templates = c.fsys
			err := agent.NewRegistry().Register(typ)
			if !errors.Is(err, agent.ErrInvalidType) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Register = %v; want ErrInvalidType containing %q", err, c.want)
			}
		})
	}
}

func TestTemplateManifest(t *testing.T) {
	r := registry(t, templatedLedger(templateFS(), nil))
	m, _ := r.Manifest("ledger")
	var got []string
	for _, tm := range m.Templates {
		if len(tm.Identity) != 64 {
			t.Errorf("template %s identity = %q", tm.Name, tm.Identity)
		}
		got = append(got, tm.Name)
	}
	want := []string{"missing.tmpl", "nested/deep/x.tmpl", "partials/rules.tmpl", "partials/unused.tmpl", "system.tmpl", "user.tmpl"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("manifest templates = %v; want %v", got, want)
	}
	digest, _ := r.ManifestDigest("ledger")
	changed := templateFS()
	changed["partials/rules.tmpl"] = &fstest.MapFile{Data: []byte(`Be thorough, {{.Name}}.`)}
	if d2, _ := registry(t, templatedLedger(changed, nil)).ManifestDigest("ledger"); d2 == digest {
		t.Error("changing a template left the manifest's digest unchanged")
	}
}

func TestRender(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	fsys := templateFS()
	typ := templatedLedger(fsys, func(env *agent.Env) (string, string, error) {
		system, err := env.Render("system.tmpl", map[string]string{"Name": "agent"})
		if err != nil {
			return "", "", err
		}
		user, err := env.Render("user.tmpl", struct{ Run string }{env.RunID()})
		return system, user, err
	})
	reg := registry(t, typ)
	// Templates are read at registration: a later change to the files
	// does not reach the run.
	fsys["system.tmpl"] = &fstest.MapFile{Data: []byte("changed")}
	client := scripted(ledgerScript)
	out, err := activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), client, 0)
	if err != nil || out.Status != store.StatusSucceeded {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
	if got := client.Calls()[0].System[0].Text; got != "Record the items. Be brief, agent." {
		t.Errorf("system = %q", got)
	}
}

func TestRenderErrors(t *testing.T) {
	cases := map[string]struct {
		fsys   fstest.MapFS
		render func(env *agent.Env) error
		want   string
	}{
		"missing key": {templateFS(), func(env *agent.Env) error {
			_, err := env.Render("missing.tmpl", map[string]string{})
			return err
		}, "absent"},
		"unknown template": {templateFS(), func(env *agent.Env) error {
			_, err := env.Render("nope.tmpl", nil)
			if !errors.Is(err, agent.ErrNoTemplate) {
				return errors.New("not ErrNoTemplate")
			}
			return err
		}, "no such template"},
		"no templates": {nil, func(env *agent.Env) error {
			_, err := env.Render("system.tmpl", nil)
			if !errors.Is(err, agent.ErrNoTemplate) {
				return errors.New("not ErrNoTemplate")
			}
			return err
		}, "declares no templates"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := newBackend(t, "memstore").open()
			createLedgerRun(t, s)
			typ := templatedLedger(c.fsys, func(env *agent.Env) (string, string, error) { return "", "", c.render(env) })
			if c.fsys == nil {
				typ.Templates = nil
			}
			out, err := activateWith(context.Background(), agent.NewController(s, registry(t, typ)), claim(t, s), scripted(ledgerScript), 0)
			if err != nil || out.Reason.Code != agent.ReasonStateError || !strings.Contains(out.Detail, c.want) {
				t.Fatalf("outcome = %+v, %v; want state_error mentioning %q", out, err, c.want)
			}
		})
	}
}
