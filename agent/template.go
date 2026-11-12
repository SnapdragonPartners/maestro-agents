package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"text/template"
	"unicode/utf8"
)

// TemplateManifest is one prompt template in a manifest (SKL-4, ADR 0013
// item 5).
type TemplateManifest struct {
	// Name is the template's path in the agent type's Templates.
	Name string `json:"name"`
	// Identity is the SHA-256, in hex, of the template file's bytes.
	Identity string `json:"identity"`
}

// standaloneName names a template file while it is parsed alone. A
// definition under any other name shows as a second template. One under
// this name can only fill an empty file's own body (a non-empty body is a
// multiple-definition parse error), and its tree is still the file's own.
const standaloneName = "\x00maestro-agents/template-file"

// templates is an agent type's parsed prompt templates.
type templates struct {
	set      *template.Template
	manifest []TemplateManifest
}

// loadTemplates parses every file in fsys as a text/template named by its
// path (SKL-4, ADR 0013 item 5). Parsing is strict: a file that is not
// valid UTF-8 or does not parse, a file that defines further templates
// ({{define}} or {{block}}: a template is a file, so its identity is the
// file's), or anything but regular files and directories, is an error.
// Templates execute with missingkey=error, so a missing map key fails the
// render rather than printing "<no value>". A template may include another
// by its path ({{template "partials/x.tmpl" .}}).
func loadTemplates(fsys fs.FS) (*templates, error) {
	out := &templates{set: template.New("").Option("missingkey=error")}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%q is not a regular file", p)
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err //nolint:wrapcheck // wrapped below
		}
		if !utf8.Valid(b) {
			return fmt.Errorf("%q is not valid UTF-8", p)
		}
		// Each file is parsed alone, so a definition of any name, another
		// file's or its own, shows as a second template; only then does
		// its tree join the set, under its path, where it can replace no
		// other file's.
		alone, err := template.New(standaloneName).Parse(string(b))
		if err != nil {
			return fmt.Errorf("%q: %w", p, err)
		}
		if ts := alone.Templates(); len(ts) != 1 || ts[0] != alone {
			return fmt.Errorf("%q defines templates of its own; a template is a file", p)
		}
		if _, err := out.set.AddParseTree(p, alone.Tree); err != nil {
			return fmt.Errorf("%q: %w", p, err)
		}
		sum := sha256.Sum256(b)
		out.manifest = append(out.manifest, TemplateManifest{Name: p, Identity: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("templates: %w", err)
	}
	return out, nil
}

// ErrNoTemplate is returned by Env.Render for a name the agent type's
// Templates do not hold.
var ErrNoTemplate = errors.New("agent: no such template")

// Render executes the agent type's prompt template named name, its path in
// Type.Templates, with data, and returns the text (SKL-4, ADR 0013 item 5).
// Templates are parsed when the type is registered, so a run renders the
// templates its manifest records, and never re-reads them.
func (e *Env) Render(name string, data any) (string, error) {
	if e.templates == nil {
		return "", fmt.Errorf("%w: %q (the agent type declares no templates)", ErrNoTemplate, name)
	}
	t := e.templates.Lookup(name)
	if t == nil || name == "" {
		return "", fmt.Errorf("%w: %q", ErrNoTemplate, name)
	}
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", fmt.Errorf("agent: render template %q: %w", name, err)
	}
	return b.String(), nil
}
