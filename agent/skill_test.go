package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

const (
	triageSkill = "---\nname: triage\ndescription: How to sort incoming items.\n---\n\nSort items by date.\n"
	styleSkill  = "---\nname: style\ndescription: House style for records.\n---\n\nUse plain words.\n"
)

// skillFS holds two skills: triage, with a reference file, and style.
func skillFS() fstest.MapFS {
	return fstest.MapFS{
		"triage/SKILL.md":           {Data: []byte(triageSkill)},
		"triage/reference/dates.md": {Data: []byte("Dates are ISO 8601.")},
		"style/SKILL.md":            {Data: []byte(styleSkill)},
	}
}

// skilledLedger is the ledger type with skills, its work state granting
// the named ones.
func skilledLedger(fsys fstest.MapFS, grant ...string) *agent.Type[ledgerData] {
	typ := ledgerType(&execLog{})
	typ.Skills = fsys
	typ.States[0].Model.Skills = grant
	return typ
}

func TestRegisterRefusesSkills(t *testing.T) {
	big := strings.Repeat("x", agent.MaxSkillFileBytes)
	cases := map[string]struct {
		fsys  fstest.MapFS
		grant []string
		want  string
	}{
		"file at root":          {fstest.MapFS{"README.md": {Data: []byte("x")}}, nil, "not a skill directory"},
		"no SKILL.md":           {fstest.MapFS{"a/notes.md": {Data: []byte("x")}}, nil, "no SKILL.md"},
		"no header":             {fstest.MapFS{"a/SKILL.md": {Data: []byte("name: a\n")}}, nil, "missing header"},
		"unclosed header":       {fstest.MapFS{"a/SKILL.md": {Data: []byte("---\nname: a\n")}}, nil, "not closed"},
		"not key value":         {fstest.MapFS{"a/SKILL.md": {Data: []byte("---\nname a\n---\nbody")}}, nil, "not key: value"},
		"unknown field":         {fstest.MapFS{"a/SKILL.md": {Data: []byte("---\nname: a\ndescription: d\nallowed-tools: x\n---\nbody")}}, nil, "unknown header field"},
		"field twice":           {fstest.MapFS{"a/SKILL.md": {Data: []byte("---\nname: a\nname: a\ndescription: d\n---\nbody")}}, nil, "given twice"},
		"bad name":              {fstest.MapFS{"A/SKILL.md": {Data: []byte("---\nname: A\ndescription: d\n---\nbody")}}, nil, "lower-case"},
		"long name":             {fstest.MapFS{strings.Repeat("a", 65) + "/SKILL.md": {Data: []byte("---\nname: " + strings.Repeat("a", 65) + "\ndescription: d\n---\nbody")}}, nil, "at most 64"},
		"name mismatch":         {fstest.MapFS{"a/SKILL.md": {Data: []byte("---\nname: b\ndescription: d\n---\nbody")}}, nil, "not its directory"},
		"no description":        {fstest.MapFS{"a/SKILL.md": {Data: []byte("---\nname: a\n---\nbody")}}, nil, "description is required"},
		"long description":      {fstest.MapFS{"a/SKILL.md": {Data: []byte("---\nname: a\ndescription: " + strings.Repeat("d", 1025) + "\n---\nbody")}}, nil, "over 1024"},
		"description separator": {fstest.MapFS{"a/SKILL.md": {Data: []byte("---\nname: a\ndescription: Alpha\u2028- other: forged\n---\nbody")}}, nil, "one line of text"},
		"description CR":        {fstest.MapFS{"a/SKILL.md": {Data: []byte("---\nname: a\ndescription: Alpha\r- other: forged\n---\nbody")}}, nil, "one line of text"},
		"path with newline":     {fstest.MapFS{"a/SKILL.md": {Data: []byte("---\nname: a\ndescription: d\n---\nbody")}, "a/x\n- other.md": {Data: []byte("x")}}, nil, "control character"},
		"path with U+FFFD":      {fstest.MapFS{"a/SKILL.md": {Data: []byte("---\nname: a\ndescription: d\n---\nbody")}, "a/\ufffd.txt": {Data: []byte("x")}}, nil, "U+FFFD"},
		"empty body":            {fstest.MapFS{"a/SKILL.md": {Data: []byte("---\nname: a\ndescription: d\n---\n  \n")}}, nil, "empty body"},
		"not UTF-8":             {fstest.MapFS{"a/SKILL.md": {Data: []byte("---\nname: a\ndescription: d\n---\nbody")}, "a/x.bin": {Data: []byte{0xff}}}, nil, "not valid UTF-8"},
		"over cap":              {fstest.MapFS{"a/SKILL.md": {Data: []byte("---\nname: a\ndescription: d\n---\nbody")}, "a/big.md": {Data: []byte(big + "x")}}, nil, "over the"},
		"unknown grant":         {skillFS(), []string{"nope"}, "do not hold"},
		"grant twice":           {skillFS(), []string{"style", "style"}, "twice"},
		"grant without fs":      {nil, []string{"style"}, "do not hold"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			typ := ledgerType(&execLog{})
			if c.fsys != nil {
				typ.Skills = c.fsys
			}
			typ.States[0].Model.Skills = c.grant
			err := agent.NewRegistry().Register(typ)
			if !errors.Is(err, agent.ErrInvalidType) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Register = %v; want ErrInvalidType containing %q", err, c.want)
			}
		})
	}
	t.Run("at the cap", func(t *testing.T) {
		fsys := fstest.MapFS{"a/SKILL.md": {Data: []byte("---\nname: a\ndescription: d\n---\nbody")}, "a/big.md": {Data: []byte(big)}}
		if err := agent.NewRegistry().Register(skilledLedger(fsys, "a")); err != nil {
			t.Fatalf("Register: %v", err)
		}
	})
	t.Run("tool named read_skill", func(t *testing.T) {
		tools := ledgerTools(&execLog{})
		tools[0].Definition.Name = agent.ReadSkillTool
		err := agent.NewRegistry().Register(ledgerType(&execLog{}, tools...))
		if !errors.Is(err, agent.ErrInvalidType) || !strings.Contains(err.Error(), "read-skill") {
			t.Fatalf("Register = %v; want the reserved name refused", err)
		}
	})
}

func TestSkillManifest(t *testing.T) {
	r := registry(t, skilledLedger(skillFS(), "triage", "style"))
	m, _ := r.Manifest("ledger")
	if len(m.Skills) != 2 || m.Skills[0].Name != "style" || m.Skills[1].Name != "triage" {
		t.Fatalf("manifest skills = %+v; want style and triage, by name", m.Skills)
	}
	tri := m.Skills[1]
	if tri.Description != "How to sort incoming items." || !slices.Equal(tri.Files, []string{"SKILL.md", "reference/dates.md"}) || len(tri.Identity) != 64 {
		t.Errorf("triage manifest = %+v", tri)
	}
	if got := m.States[0].Skills; !slices.Equal(got, []string{"triage", "style"}) {
		t.Errorf("work state skills = %v; want the grant, in order", got)
	}
	if slices.Contains(m.States[0].Tools, agent.ReadSkillTool) {
		t.Errorf("state tools list %s; it is derived from the skills", agent.ReadSkillTool)
	}

	// A change to a reference file changes the skill's identity and the
	// manifest's digest; the other skill's identity stays.
	digest, _ := r.ManifestDigest("ledger")
	changed := skillFS()
	changed["triage/reference/dates.md"] = &fstest.MapFile{Data: []byte("Dates are RFC 3339.")}
	r2 := registry(t, skilledLedger(changed, "triage", "style"))
	m2, _ := r2.Manifest("ledger")
	digest2, _ := r2.ManifestDigest("ledger")
	if m2.Skills[1].Identity == tri.Identity || digest2 == digest {
		t.Error("changing a reference file left the skill's identity or the manifest's digest unchanged")
	}
	if m2.Skills[0].Identity != m.Skills[0].Identity {
		t.Error("changing one skill changed another's identity")
	}

	// A type without skills or templates lists none.
	plain, _ := registry(t, ledgerType(&execLog{})).Manifest("ledger")
	if plain.Skills != nil || plain.Templates != nil || plain.States[0].Skills != nil {
		t.Errorf("plain manifest = %+v", plain)
	}
}

// skillScript reads skills in its first turn, valid calls and invalid ones,
// records the ledger's items, and finishes in its second.
var skillScript = []llms.ChatResponse{
	callsResp(
		tc("s1", agent.ReadSkillTool, `{"skill":"triage"}`),
		tc("s2", agent.ReadSkillTool, `{"skill":"triage","file":"reference/dates.md"}`),
		tc("s3", agent.ReadSkillTool, `{"skill":"style","file":"SKILL.md"}`),
		tc("s4", agent.ReadSkillTool, `{"skill":"nope"}`),
		tc("s5", agent.ReadSkillTool, `{"skill":"triage","file":"../style/SKILL.md"}`),
		tc("s6", agent.ReadSkillTool, `{"skill":"triage","extra":1}`),
		tc("s7", agent.ReadSkillTool, `not json`),
		tc("s8", agent.ReadSkillTool, `{"skill":"unoffered"}`),
		tc("c1", "record", `{"item":"a"}`), tc("c2", "record", `{"item":"b"}`), tc("c3", "record", `{"item":"c"}`),
	),
	callsResp(tc("c4", "finish", `{}`)),
}

func TestReadSkillTool(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		fsys := skillFS()
		fsys["unoffered/SKILL.md"] = &fstest.MapFile{Data: []byte("---\nname: unoffered\ndescription: Not granted here.\n---\nSecret.")}
		s := b.open()
		createLedgerRun(t, s)
		client := scripted(skillScript)
		out, err := activateWith(context.Background(), agent.NewController(s, registry(t, skilledLedger(fsys, "triage", "style"))), claim(t, s), client, 0)
		if err != nil || out.Status != store.StatusSucceeded {
			t.Fatalf("outcome = %+v, %v", out, err)
		}
		reqs := client.Calls()
		if len(reqs) != 2 {
			t.Fatalf("%d requests; want 2", len(reqs))
		}

		// The tool is offered after the state's own, its description the
		// index of the granted skills in grant order, its input an enum of
		// them.
		defs := reqs[0].Tools
		last := defs[len(defs)-1]
		if last.Name != agent.ReadSkillTool || len(defs) != 4 {
			t.Fatalf("offered tools = %v; want the state's three, then %s", names(defs), agent.ReadSkillTool)
		}
		if !strings.HasSuffix(last.Description, "Skills:\n- triage: How to sort incoming items.\n- style: House style for records.") {
			t.Errorf("description = %q", last.Description)
		}
		var schema struct {
			Properties struct {
				Skill struct {
					Enum []string `json:"enum"`
				} `json:"skill"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(last.InputSchema, &schema); err != nil || !slices.Equal(schema.Properties.Skill.Enum, []string{"triage", "style"}) {
			t.Errorf("schema = %s (%v)", last.InputSchema, err)
		}

		results := map[string]llms.ToolResult{}
		for _, m := range reqs[1].Messages {
			for _, p := range m.Content {
				if p.ToolResult != nil {
					results[p.ToolResult.ToolCallID] = *p.ToolResult
				}
			}
		}
		want := map[string]struct {
			content string
			isError bool
		}{
			"s1": {"Sort items by date.\n\nFiles in this skill: reference/dates.md.", false},
			"s2": {"Dates are ISO 8601.", false},
			"s3": {"Use plain words.", false},
			"s4": {"No such skill. Choose one of: triage, style.", true},
			"s5": {"Skill triage has no such file. Its files: SKILL.md, reference/dates.md.", true},
			"s6": {"No such skill. Choose one of: triage, style.", true},
			"s7": {"No such skill. Choose one of: triage, style.", true},
			"s8": {"No such skill. Choose one of: triage, style.", true},
		}
		for id, w := range want {
			got := results[id]
			if got.Content != w.content || got.IsError != w.isError {
				t.Errorf("result %s = %q (error %v); want %q (error %v)", id, got.Content, got.IsError, w.content, w.isError)
			}
		}
	})
}

func names(defs []llms.ToolDefinition) []string {
	out := make([]string, len(defs))
	for i := range defs {
		out[i] = defs[i].Name
	}
	return out
}

// TestReadSkillIdentity checks that the read-skill tool's identity, which
// permissions record and recovery enforces, follows the granted skills'
// content and the grant.
func TestReadSkillIdentity(t *testing.T) {
	identity := func(fsys fstest.MapFS, grant ...string) string {
		s := newBackend(t, "memstore").open()
		createLedgerRun(t, s)
		var mu sync.Mutex
		var id string
		policy := func(_ context.Context, req *agent.PolicyRequest) (agent.Decision, error) {
			mu.Lock()
			defer mu.Unlock()
			if req.Tool == agent.ReadSkillTool {
				id = req.Identity
			}
			return agent.Decision{Verdict: agent.VerdictAllow}, nil
		}
		c := agent.NewController(s, registry(t, skilledLedger(fsys, grant...)), agent.WithPolicy(policy))
		if _, err := activateWith(context.Background(), c, claim(t, s), scripted(skillScript), 0); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		if id == "" {
			t.Fatal("the policy never saw a read-skill call")
		}
		return id
	}
	base := identity(skillFS(), "triage", "style")
	if again := identity(skillFS(), "triage", "style"); again != base {
		t.Error("identity is not stable")
	}
	changed := skillFS()
	changed["style/SKILL.md"] = &fstest.MapFile{Data: []byte(strings.Replace(styleSkill, "plain", "short", 1))}
	if identity(changed, "triage", "style") == base {
		t.Error("changing a granted skill's body left the identity unchanged")
	}
	if identity(skillFS(), "style", "triage") == base {
		t.Error("reordering the grant left the identity unchanged")
	}
}

// TestReadSkillNotOfferedInWrapUp checks that a wrap-up turn offers only
// the wrap-up's tools, not the read-skill tool.
func TestReadSkillNotOfferedInWrapUp(t *testing.T) {
	s := newBackend(t, "memstore").open()
	createLedgerRun(t, s)
	client := scripted([]llms.ChatResponse{callsResp(tc("c1", "finish", `{}`))})
	c := agent.NewController(s, registry(t, skilledLedger(skillFS(), "triage")))
	out, err := c.Activate(context.Background(), agent.ActivationInput{
		Fence: claim(t, s), Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}, Namespace: ns, Client: client,
		Budgets: agent.Budgets{Turns: 2},
	})
	// The run ends rejected: it finished without recording its items.
	if err != nil || out.Reason.Code != agent.ReasonInvalidOutput {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
	if got := names(client.Calls()[0].Tools); !slices.Equal(got, []string{"finish"}) {
		t.Errorf("wrap-up turn offered %v; want only finish", got)
	}
}

// TestReadSkillCrashAtEveryCommit crashes a run that reads skills at each
// of its commits, and recovers it: the read-skill tool's calls resolve
// through the same recovery as any read, so the run ends as the
// uninterrupted one does, with the same transcript.
func TestReadSkillCrashAtEveryCommit(t *testing.T) {
	newType := func() *agent.Type[ledgerData] { return skilledLedger(skillFS(), "triage", "style") }
	var want []string
	total := func() int {
		s := &countingStore{Store: newBackend(t, "memstore").open()}
		createLedgerRun(t, s)
		if _, err := activateWith(context.Background(), agent.NewController(s, registry(t, newType())), claim(t, s), scripted(skillScript), 0); err != nil {
			t.Fatal(err)
		}
		want = lastTranscript(t, s)
		return s.n
	}()
	for at := 1; at <= total; at++ {
		for _, landing := range []bool{false, true} {
			t.Run(fmt.Sprintf("commit-%d/landing-%v", at, landing), func(t *testing.T) {
				t.Parallel()
				s := newBackend(t, "memstore").open()
				createLedgerRun(t, s)
				reg := registry(t, newType())
				cs := &crashStore{Store: s, at: at, landing: landing}
				if _, err := activateWith(context.Background(), agent.NewController(cs, reg), claim(t, s), scripted(skillScript), 0); !errors.Is(err, agent.ErrStoreFailure) {
					t.Fatalf("crashed activation = %v, want a store failure", err)
				}
				if run := runRecord(t, s, "r1"); !run.Status.Terminal() {
					releaseIfOwned(t, s)
					out, err := activateWith(context.Background(), agent.NewController(s, reg), claim(t, s), scripted(skillScript), 0)
					if err != nil || out.Status != store.StatusSucceeded {
						t.Fatalf("outcome = %+v, %v", out, err)
					}
				}
				if got := lastTranscript(t, s); !slices.Equal(got, want) {
					t.Fatalf("transcript = %v, want the uninterrupted run's %v", got, want)
				}
			})
		}
	}
}

// TestReadSkillChangedBeforeRecovery checks that a read-skill call
// permitted before a crash is not executed after its skills change: the
// permission is bound to the tool's identity (ADR 0011, item 3), which the
// skills' content determines (ADR 0013, item 4).
func TestReadSkillChangedBeforeRecovery(t *testing.T) {
	// Crash at the first commit point that leaves the first read-skill
	// call permitted with no result.
	var s store.Store
	for at := 1; s == nil; at++ {
		if at > 20 {
			t.Fatal("no commit point leaves a read-skill call permitted with no result")
		}
		try := newBackend(t, "memstore").open()
		createLedgerRun(t, try)
		cs := &crashStore{Store: try, at: at, landing: true}
		if _, err := activateWith(context.Background(), agent.NewController(cs, registry(t, skilledLedger(skillFS(), "triage", "style"))), claim(t, try), scripted(skillScript), 0); !errors.Is(err, agent.ErrStoreFailure) {
			t.Fatalf("crashed activation = %v, want a store failure", err)
		}
		permitted, resulted := false, false
		for _, e := range entries(t, try, "r1") {
			switch {
			case e.Kind == agent.EntryPermission && payload[agent.PermissionEntry](t, e).Call == 0:
				permitted = true
			case e.Kind == agent.EntryResult && payload[agent.ResultEntry](t, e).Call == 0:
				resulted = true
			}
		}
		if permitted && !resulted {
			s = try
		}
	}
	changed := skillFS()
	changed["triage/SKILL.md"] = &fstest.MapFile{Data: []byte(strings.Replace(triageSkill, "by date", "by size", 1))}
	releaseIfOwned(t, s)
	client := scripted(skillScript)
	if _, err := activateWith(context.Background(), agent.NewController(s, registry(t, skilledLedger(changed, "triage", "style"))), claim(t, s), client, 0); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range client.Calls()[0].Messages {
		for _, p := range m.Content {
			if r := p.ToolResult; r != nil && r.ToolCallID == "s1" {
				found = true
				if !r.IsError || strings.Contains(r.Content, "by size") || strings.Contains(r.Content, "by date") {
					t.Errorf("s1 after the skill changed = %+v; want the call refused as changed", r)
				}
			}
		}
	}
	if !found {
		t.Fatal("the request after recovery carries no result for s1")
	}
}
