package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// ReadSkillTool is the name of the built-in tool that loads a skill (SKL-2,
// ADR 0013). A model state granted skills is offered it after its own
// tools; no agent type may declare a tool of this name.
const ReadSkillTool = "read_skill"

// SkillFile is the file every skill directory holds: its header and
// instructions.
const SkillFile = "SKILL.md"

// MaxSkillFileBytes caps each file of a skill, so the read-skill tool never
// returns more than a tool result may carry (ADR 0013, item 3).
const MaxSkillFileBytes = 32 * 1024

// Limits on a skill's header, the Agent Skills format's own.
const (
	maxSkillNameLen        = 64
	maxSkillDescriptionLen = 1024
)

var skillNameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// skill is one loaded skill: its header, its files and its identity.
type skill struct {
	// files are the skill's files by path within its directory, SKILL.md
	// included, and paths their sorted keys.
	files       map[string]string
	name        string
	description string
	// body is SKILL.md after its header, trimmed.
	body     string
	identity string
	paths    []string
}

// references returns the skill's files other than SKILL.md, sorted.
func (s *skill) references() []string {
	return slices.DeleteFunc(slices.Clone(s.paths), func(p string) bool { return p == SkillFile })
}

// skillIdentityFormat tags the encoding a skill identity hashes.
const skillIdentityFormat = "maestro-agents/skill-identity/1"

// loadSkills reads an agent type's skills (SKL-1, ADR 0013 items 1 and 2):
// one directory per skill at the root of fsys, named for the skill and
// holding SKILL.md and any reference files. Loading is strict: a file at
// the root, a directory without SKILL.md, a malformed or unknown header
// field, a name that does not match its directory, a file that is not
// valid UTF-8 or is over MaxSkillFileBytes, or anything but regular files
// and directories, is an error. The result is ordered by name.
func loadSkills(fsys fs.FS) ([]*skill, error) {
	root, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("skills: read root: %w", err)
	}
	out := make([]*skill, 0, len(root))
	for _, e := range root {
		if !e.IsDir() {
			return nil, fmt.Errorf("skills: %q at the root is not a skill directory", e.Name())
		}
		s, err := loadSkill(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("skills: %s: %w", e.Name(), err)
		}
		out = append(out, s)
	}
	return out, nil
}

func loadSkill(fsys fs.FS, dir string) (*skill, error) {
	s := &skill{files: map[string]string{}}
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(p, dir+"/")
		if !d.Type().IsRegular() {
			return fmt.Errorf("%q is not a regular file", rel)
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err //nolint:wrapcheck // wrapped with the skill's name by the caller
		}
		switch {
		case len(b) > MaxSkillFileBytes:
			return fmt.Errorf("%q is %d bytes, over the %d cap", rel, len(b), MaxSkillFileBytes)
		case !utf8.Valid(b):
			return fmt.Errorf("%q is not valid UTF-8", rel)
		}
		s.files[rel] = string(b)
		s.paths = append(s.paths, rel)
		return nil
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // wrapped with the skill's name by the caller
	}
	raw, ok := s.files[SkillFile]
	if !ok {
		return nil, fmt.Errorf("no %s", SkillFile)
	}
	if err := s.parse(raw); err != nil {
		return nil, fmt.Errorf("%s: %w", SkillFile, err)
	}
	if s.name != dir {
		return nil, fmt.Errorf("%s names the skill %q, not its directory's name", SkillFile, s.name)
	}
	slices.Sort(s.paths)
	s.identity = s.digest()
	return s, nil
}

// parse reads SKILL.md: a header of key: value lines between --- markers,
// naming the skill and describing it, then its instructions.
func (s *skill) parse(raw string) error {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	rest, ok := strings.CutPrefix(raw, "---\n")
	if !ok {
		return errors.New("missing header: the file must open with ---")
	}
	header, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return errors.New("header is not closed with ---")
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(header, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			return fmt.Errorf("header line %q is not key: value", line)
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if seen[key] {
			return fmt.Errorf("header field %q is given twice", key)
		}
		seen[key] = true
		switch key {
		case "name":
			s.name = val
		case "description":
			s.description = val
		default:
			return fmt.Errorf("unknown header field %q", key)
		}
	}
	switch {
	case !skillNameRE.MatchString(s.name) || len(s.name) > maxSkillNameLen:
		return fmt.Errorf("name %q is not lower-case words joined by hyphens, at most %d characters", s.name, maxSkillNameLen)
	case s.description == "":
		return errors.New("description is required")
	case utf8.RuneCountInString(s.description) > maxSkillDescriptionLen:
		return fmt.Errorf("description is over %d characters", maxSkillDescriptionLen)
	}
	s.body = strings.TrimSpace(body)
	if s.body == "" {
		return errors.New("empty body")
	}
	return nil
}

// digest returns the skill's content identity (ADR 0013, item 4): the
// SHA-256 of its files, by sorted path, each path and content
// length-prefixed, so no two skills encode alike.
func (s *skill) digest() string {
	h := sha256.New()
	field := func(b string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write([]byte(b))
	}
	field(skillIdentityFormat)
	for _, p := range s.paths {
		field(p)
		field(s.files[p])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// readSkillDescription opens the read-skill tool's description; the index
// of the offered skills follows it.
const readSkillDescription = "Reads one of your skills: instructions written for this work, loaded when you need them. " +
	"Before work a skill covers, read it and follow it. Omit file to read the skill's SKILL.md, " +
	"which names the skill's other files; give file to read one of them. Skills:"

// readSkill builds the read-skill tool for a model state granted skills,
// in grant order (SKL-2, ADR 0013 item 3). Its description carries the
// index, one line per skill; its input names a skill, from an enum of the
// granted ones, and optionally a file. The enum is advice, not validation:
// Execute checks the input against the granted skills and their files, and
// answers anything else with a tool error the model can correct. Its
// identity's version is derived from the granted skills' identities, so a
// change to any granted skill's files changes it (ADR 0013, item 4).
func readSkill(granted []*skill) *erasedTool {
	var desc strings.Builder
	desc.WriteString(readSkillDescription)
	names := make([]string, len(granted))
	byName := make(map[string]*skill, len(granted))
	ids := sha256.New()
	ids.Write([]byte(skillIdentityFormat))
	for i, s := range granted {
		names[i] = s.name
		byName[s.name] = s
		fmt.Fprintf(&desc, "\n- %s: %s", s.name, s.description)
		ids.Write([]byte("\n" + s.name + " " + s.identity))
	}
	schema, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"skill": map[string]any{"type": "string", "enum": names, "description": "The skill to read."},
			"file":  map[string]any{"type": "string", "description": "A file of the skill, by the path its SKILL.md gives; omit to read SKILL.md."},
		},
		"required":             []string{"skill"},
		"additionalProperties": false,
	})
	if err != nil {
		// Every value is a string or a list of strings.
		panic(fmt.Sprintf("agent: encode read-skill schema: %v", err))
	}
	et := &erasedTool{
		def:    llms.ToolDefinition{Name: ReadSkillTool, Description: desc.String(), InputSchema: compactJSON(schema)},
		effect: EffectRead,
	}
	et.identity = toolIdentity(&et.def, EffectRead, "", "skills:"+hex.EncodeToString(ids.Sum(nil)))
	et.exec = func(_ context.Context, _ *Env, _ json.RawMessage, call llms.ToolCall, _ ActionID, _ string, _ bool) (erasedResult, error) {
		return readSkillResult(byName, names, call.Parameters), nil
	}
	return et
}

// readSkillResult answers a read-skill call.
func readSkillResult(byName map[string]*skill, names []string, params json.RawMessage) erasedResult {
	var in struct {
		File  *string `json:"file"`
		Skill string  `json:"skill"`
	}
	dec := json.NewDecoder(bytes.NewReader(params))
	dec.DisallowUnknownFields()
	var s *skill
	if dec.Decode(&in) == nil {
		s = byName[in.Skill]
	}
	if s == nil {
		return erasedResult{content: "No such skill. Choose one of: " + strings.Join(names, ", ") + ".", isError: true}
	}
	if in.File == nil || *in.File == SkillFile {
		content := s.body
		if refs := s.references(); len(refs) > 0 {
			content += "\n\nFiles in this skill: " + strings.Join(refs, ", ") + "."
		}
		return erasedResult{content: content}
	}
	if f, ok := s.files[*in.File]; ok {
		return erasedResult{content: f}
	}
	return erasedResult{content: fmt.Sprintf("Skill %s has no such file. Its files: %s.", s.name, strings.Join(s.paths, ", ")), isError: true}
}

// SkillManifest is one skill in a manifest (MAN-1, ADR 0013 item 4).
type SkillManifest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Identity is the skill's content identity: a SHA-256, in hex, over
	// its files.
	Identity string `json:"identity"`
	// Files are the skill's files by path in its directory, sorted,
	// SKILL.md included.
	Files []string `json:"files"`
}

func (s *skill) manifest() SkillManifest {
	return SkillManifest{Name: s.name, Description: s.description, Identity: s.identity, Files: slices.Clone(s.paths)}
}
