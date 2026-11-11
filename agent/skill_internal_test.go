package agent

import (
	"strings"
	"testing"
)

// TestReadSkillInputBoundary drives the read-skill tool's input, which the
// model writes, through every class an adversarial review enumerated: only
// an exact, well-formed call naming a granted skill and, optionally, one of
// its files by exact path is served (ADR 0013, item 3).
func TestReadSkillInputBoundary(t *testing.T) {
	a := &skill{name: "a", body: "Body A.", files: map[string]string{"SKILL.md": "raw", "ref/note.md": "Note."}, paths: []string{"SKILL.md", "ref/note.md"}}
	b := &skill{name: "b", body: "Body B.", files: map[string]string{"SKILL.md": "raw", "only-b.txt": "B only."}, paths: []string{"SKILL.md", "only-b.txt"}}
	for _, s := range []*skill{a, b} {
		if err := s.results(); err != nil {
			t.Fatal(err)
		}
	}
	byName := map[string]*skill{"a": a, "b": b}
	names := []string{"a", "b"}
	const (
		bodyA   = "Body A.\n\nFiles in this skill: ref/note.md."
		noSkill = "No such skill. Choose one of: a, b."
		noFile  = "Skill a has no such file. Its files: SKILL.md, ref/note.md."
	)
	cases := []struct{ in, want string }{
		{`{"skill":"a"}`, bodyA},
		{`{"skill":"a"}`, bodyA},
		{` {"skill":"a"} `, bodyA},
		{`{"skill":"a","file":"SKILL.md"}`, bodyA},
		{`{"skill":"a","file":"ref/note.md"}`, "Note."},
		{`{"skill":"a","skill":"b"}`, "Body B.\n\nFiles in this skill: only-b.txt."},
		{`{"skill":"c","skill":"a"}`, bodyA},
		{`{"skill":"a","skill":"c"}`, noSkill},
		// Review finding 1: trailing data, explicit null, case-folded keys.
		{`{"skill":"a"}junk`, noSkill},
		{`{"skill":"a"}{"skill":"b"}`, noSkill},
		{`{"skill":"a"}[]`, noSkill},
		{`{"skill":"a","file":null}`, noSkill},
		{`{"skill":"a","file":"ref/note.md","file":null}`, noSkill},
		{`{"skill":"a","skill":null}`, noSkill},
		{`{"skill":null}`, noSkill},
		{`{"SKILL":"a"}`, noSkill},
		{`{"ſkill":"a"}`, noSkill},
		{`{"sKill":"a"}`, noSkill},
		{`{"skill":"a","FILE":"ref/note.md"}`, noSkill},
		// Review finding 2: invalid UTF-8 and unpaired surrogates.
		{"{\"skill\":\"a\",\"file\":\"\xff.txt\"}", noSkill},
		{`{"skill":"a","file":"\ud800.txt"}`, noSkill},
		{`{"skill":"\ud800"}`, noSkill},
		{`{"skill":"a","file":"�.txt"}`, noSkill},
		// The rest of the enumerated classes.
		{``, noSkill},
		{`null`, noSkill},
		{`[]`, noSkill},
		{`"a"`, noSkill},
		{`{}`, noSkill},
		{`{"file":"SKILL.md"}`, noSkill},
		{`{"skill":1}`, noSkill},
		{`{"skill":["a"]}`, noSkill},
		{`{"skill":"A"}`, noSkill},
		{`{"skill":" a"}`, noSkill},
		{`{"skill":"a","extra":1}`, noSkill},
		{`{"skill":"a","file":1}`, noSkill},
		{`{"skill":"a" /* c */}`, noSkill},
		{`{"skill":"a","file":""}`, noFile},
		{`{"skill":"a","file":"./SKILL.md"}`, noFile},
		{`{"skill":"a","file":"../b/SKILL.md"}`, noFile},
		{`{"skill":"a","file":"/a/SKILL.md"}`, noFile},
		{`{"skill":"a","file":"ref\\note.md"}`, noFile},
		{`{"skill":"a","file":"ref%2fnote.md"}`, noFile},
		{`{"skill":"a","file":"ref/note.md/"}`, noFile},
		{`{"skill":"a","file":"ref"}`, noFile},
		{`{"skill":"a","file":"only-b.txt"}`, noFile},
		{`{"skill":"a","file":"skill.md"}`, noFile},
		{`{"skill":"a","file":"ref/note.md\u0000"}`, noFile},
		{`{"skill":"a","file":"` + strings.Repeat("x", 1<<20) + `"}`, noFile},
	}
	for _, c := range cases {
		got := readSkillResult(byName, names, []byte(c.in))
		if got.content != c.want || got.isError != (c.want == noSkill || c.want == noFile) {
			in := c.in
			if len(in) > 80 {
				in = in[:80] + "..."
			}
			t.Errorf("input %q: got %q (error %v); want %q", in, got.content, got.isError, c.want)
		}
	}
}

// TestPlainLine checks the one-line rule for what an author writes that
// reaches the model as text: descriptions and file paths (ADR 0013, item 2).
func TestPlainLine(t *testing.T) {
	for _, s := range []string{"plain words: with a colon", "ref/note.md", "naïve — dash", `literal \n`} {
		if !plainLine(s) {
			t.Errorf("plainLine(%q) = false", s)
		}
	}
	for _, s := range []string{"a\nb", "a\rb", "a\tb", "a\vb", "a\fb", "a\x00b", "a\u0085b", "a - other: forged", "a b", "a�b", "a\x7fb"} {
		if plainLine(s) {
			t.Errorf("plainLine(%q) = true", s)
		}
	}
}
