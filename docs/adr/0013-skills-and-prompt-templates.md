# 0013. Skills and prompt templates

- **Status:** Accepted
- **Date:** 2026-11-11

## Context

SKL-1 asks each agent type to embed its skills, a `SKILL.md` plus
reference files, declared in the manifest. SKL-2 asks for progressive
disclosure: the model sees each skill's description up front and loads a
body or reference file on demand, through a built-in read-skill tool.
SKL-3 puts skills behind a source interface: compiled-in only for now, with
admin-written skills from another source planned for. SKL-4 asks for
prompts as embedded templates with content identity (MAN-4), resolved once
at run start and never re-resolved mid-run. [ADR 0011](0011-manifests.md)
left skills and templates out of the manifest for this work, and left
grouping runs by prompt template to it (item 11).

Morris already runs this pattern in production: `morris_help`, Morris's
own help offered to its chat model (Morris ADR 0035, amendment of
2026-10-15, decisions 9 to 11; `docs/design/chat-tools.md` §6;
`internal/help`). Its topics are embedded markdown files with a strict
`key: value` header. The tool's description carries the index (one line
of purpose, then `- <slug>: <summary>` per topic), its input is an enum of
the offered slugs, and its `Execute` checks the input against the offered
set, because the enum is advice to the model, not validation. A topic's
body is capped below the tool-result cap, so it is never truncated. That
amendment rejected Anthropic's Agent Skills because they need Anthropic's
code-execution sandbox. This ADR takes the Agent Skills file layout only
(a directory per skill, `SKILL.md` with `name` and `description`). Loading
stays this library's own tool, on any provider, so the rejection does not
apply. Maestro's `pkg/templates` (embedded `text/template` files) and its
ADR 0031 (content-addressed prompt packs, prompt text hashed byte for
byte) are the precedent for templates. Pack slots, schemes and selectors
are not taken: an agent type's templates are code, versioned with it.

It amends, item by item:

- ADR 0004 item 2 (`run_started` holds the digests of the manifest,
  prompts, skills and tool definitions; by ADR 0011, the manifest's
  digest, with "skills join with their PR"): by items 4 and 5, the
  manifest's digest covers the type's skills and templates.
- ADR 0007 item 2 (what an agent type declares and `Register` checks): by
  items 1, 2 and 5, an agent type also declares `Skills` and `Templates`,
  which `Register` reads and checks.
- ADR 0007 item 13 (what `agent.Env` exposes): by item 5, it also renders
  the type's templates.
- ADR 0008 item 1 (`ModelState` holds `Prompt`, `Tools` and `Nudge`): by
  item 3, it also holds `Skills`.
- ADR 0008 item 2 (the request's `Tools` is empty "since the state offers
  its `Tools`"): by item 3, a state granting skills also offers the
  read-skill tool.
- ADR 0008 item 3 (what `Register` refuses of a tool): by item 3, it also
  refuses a tool named `read_skill`.
- ADR 0011 item 1 (what the manifest lists): by items 4 and 5, it also
  lists the type's skills and templates with their identities, and each
  model state's granted skills.
- ADR 0011 item 11 (grouping runs by prompt template "comes with
  templates"): by item 5, the manifest's digest covers every template, so
  runs group by template through it.

## Decision

### Skills

1. **Compiled-in skills are an `fs.FS`** (SKL-1, SKL-3). `Type.Skills`,
   normally an `embed.FS`, holds one directory per skill at its root, named
   for the skill, with `SKILL.md` and any reference files. They are read,
   checked and hashed once, at registration, which for compiled-in skills
   is the same as at run start: they cannot change while the process runs.
   `fs.FS` is the source interface. A source of admin-written skills is any
   `fs.FS`, and resolving such a source at run start is the extension path
   (Consequences).
2. **`SKILL.md` is parsed strictly.** A header of `key: value` lines
   between `---` markers, then the instructions. The header has exactly
   `name` (lower-case words joined by hyphens, at most 64 characters, equal
   to the directory's name) and `description` (required, at most 1024
   characters), the Agent Skills format's limits. A description, and every
   file's path, is one line of plain text: no control character (CR, tab,
   NUL, NEL and the rest), no line or paragraph separator (U+2028,
   U+2029) and no U+FFFD. Both reach the model as text (the index, a
   skill's file list), so neither can forge another line. Any other field
   is refused rather than ignored: silently dropping `allowed-tools`, say,
   would mislead the author. A file at the root, a directory without
   `SKILL.md`, an empty body, a file that is not valid UTF-8, or one over
   `MaxSkillFileBytes` (32 KiB) is refused at registration with
   `ErrInvalidType`. No YAML dependency: the header is a fixed shape that a
   few lines of Go read.
3. **Skills are granted per state, and loaded through the built-in
   `read_skill` tool** (SKL-2). `ModelState.Skills` names the granted
   skills, each held by `Type.Skills`. A state granting any is offered
   `read_skill` after its own tools; a state granting none gets no tool.
   - Its description is one line of purpose, then the index, `- <name>:
     <description>` per granted skill in grant order. SKL-2 says the
     descriptions go "into the system prompt". They go into the tool's
     definition instead, which the model is given with every request just
     as it is given the system prompt. That serves SKL-2's purpose without
     the runtime editing the system instructions `Prompt` returns, and the
     index is covered by the tool's identity (item 4).
   - Its input is `{skill, file?}`, `skill` an enum of the granted names.
     The enum is advice, not validation. `Execute` decodes the input
     strictly, at one choke point: valid UTF-8, one JSON object with
     nothing after it, keys exactly `skill` and optionally `file` (matched
     exactly, not case-folded, as `encoding/json` would into a struct),
     each a string (`null` is refused, not read as omitted), and no decoded
     value holding U+FFFD. Since no path holds U+FFFD (item 2), invalid
     UTF-8 or an unpaired surrogate, which decoding replaces with it, can
     never alias a file. It then checks the input against the granted
     skills and the named skill's files, by exact path. A call
     without `file` (or with `SKILL.md`) returns the instructions without
     the header, followed by the list of the skill's other files. A
     malformed input, an ungranted skill or an unknown file gets a tool
     error naming the valid choices, which the model can correct. It does
     not fail the run.
   - It is a read with no state of its own, so recovery treats it as any
     read (STM-6). It never transitions, so it is never a wrap-up tool
     (ADR 0010), and a wrap-up turn does not offer it.
   - No agent type may declare a tool named `read_skill`.
   - A skill file never exceeds `MaxSkillFileBytes`, so when TOOL-10's
     output cap is built it must be at least that, plus the visible clock's
     footer, and a skill is never truncated.

### Identity

4. **A skill's identity is the SHA-256 of its files**: by sorted path,
   each path and content length-prefixed, with a format tag. The manifest
   lists each skill (name, description, identity, files) and each model
   state's granted skills. `read_skill` is not listed among a state's
   tools or the manifest's tools, since its definition varies by state and
   is derived from the grant. Its tool identity (ADR 0011, item 3) is its
   definition with a version derived from the granted skills' identities,
   in grant order. A change to any granted skill's files therefore changes
   it, and a call permitted before the change is refused after it, as for
   any tool whose identity changed (ADR 0008, item 11, as ADR 0011 amended
   it).

### Templates

5. **Prompt templates are an `fs.FS` of `text/template` files**
   (SKL-4). Every file in `Type.Templates` is a template named by its path.
   All of them are parsed at registration into one set, so one can include
   another by path. Execution uses `missingkey=error`, so a missing map key
   fails the render instead of printing `<no value>`. A file that does not
   parse or is not valid UTF-8 is refused, and so is one that defines
   templates of its own (`{{define}}`, `{{block}}`): a template is a file,
   so the file's identity is the template's. `Env.Render(name, data)`
   executes one, so `Prompt` (or a tool or code state) renders its text
   from the templates registration parsed. An unknown name returns
   `ErrNoTemplate`, and a render error returned from `Prompt` is the
   state's defect (`state_error`, ADR 0008 item 2). The manifest lists each
   template's path and the SHA-256 of its bytes, hashed exactly, since
   whitespace is part of what the model receives. Templates are fixed for
   the process, so a run never re-resolves them. Which templates a given
   attempt rendered is not recorded: the rendered request is (ADR 0008,
   item 8), and the manifest's digest identifies the template set.

## Consequences

- Agent types can ship skills and templates in their binaries. No change
  for a type that declares neither: its manifest, and so its digest, is
  unchanged, since the new fields are omitted when empty, so
  `ManifestFormat` stays 1.
- An orchestrator reads a type's skills and templates, with their
  identities, from its manifest, and a change to any of them changes the
  manifest's digest.
- A run that recovers in a process whose skills changed refuses the
  read-skill calls permitted before the change. A process whose templates
  changed renders the changed text for a prompt not yet recorded. A
  recorded attempt is never re-rendered (ADR 0008, item 2). The run's
  `run_started` keeps the manifest digest it started with.
- The extension path for admin-written skills (SKL-3): a host-supplied
  source, resolved once at the run's start, its skills' identities
  recorded in `run_started` beside the manifest digest and fixed for the
  run. Recovery then reads the recorded skill versions, not the source's
  current ones, so the source must keep a resolved version readable while
  any run uses it. Until a consumer needs it, compiled-in skills stand.
- Content rules (what a skill may say, Morris's URL and vendor-name
  denylist) are the agent type's, tested with its content, not the
  library's.
- Skill files are text only. A reference image or PDF, served through
  `ToolResult.Parts` (TOOL-11), is a later extension.

## References

- [docs/requirements.md](../requirements.md): SKL-1 to SKL-4, MAN-1, MAN-4,
  TOOL-10, STM-6
- [ADR 0004](0004-execution-record-and-recovery.md), item 2
- [ADR 0007](0007-agent-types-and-the-run-controller.md), items 2, 13
- [ADR 0008](0008-model-states.md), items 1, 2, 3
- [ADR 0010](0010-wrap-up-and-the-visible-clock.md)
- [ADR 0011](0011-manifests.md), items 1, 3, 11
- Morris: ADR 0035 (amendment of 2026-10-15), `docs/design/chat-tools.md`
  §6, `internal/help`
- Maestro: ADR 0031, `pkg/templates`
