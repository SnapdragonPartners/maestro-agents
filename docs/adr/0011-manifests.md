# 0011. Manifests

- **Status:** Accepted
- **Date:** 2026-11-10

## Context

MAN-1 asks for a manifest per agent type: its name, version, states and
transitions, granted tools, skills, the trigger kinds it supports, the
schema of its instance configuration and a default budget, with a content
identity for each granted tool that changes whenever the tool's definition
or implementation does (TOOL-4 binds approvals to it). MAN-2 asks the
runtime to enforce it: a tool not granted is neither offered nor executable,
an undeclared trigger kind is refused before the run starts, and the
instance configuration is validated against the schema before the run
starts. MAN-4 asks each run to record a digest of what the model was told
and offered.

Some of this exists. Agent types are registered by name (MAN-3, [ADR
0007](0007-agent-types-and-the-run-controller.md) item 2). A model state's
`Tools` is its grant: a call naming any other tool is refused and the model
receives the refusal as data ([ADR 0008](0008-model-states.md) items 3
and 11). Each model attempt records its parameters' artifact, with the
system instructions and the tools offered (ADR 0008 item 8). What does not
exist: trigger kinds anywhere, a configuration schema, default budgets, tool
identities, a manifest an orchestrator can read, and a reason for a run
refused for its configuration, which today fails with `state_error`, the
reason for a defect in the agent type (ADR 0009 item 2).

A JSON Schema validator is the one piece that could be imported. The
mature Go implementations implement the whole specification, and every
dependency of this library is a dependency of every agent and orchestrator
that imports it (CLAUDE.md, *Dependency Discipline*). Configuration needs
a small part of JSON Schema.

Three things are left out. Confining an instance to the object-store
namespaces the host assigns (STORE-2; ADR 0005 item 7 and ADR 0008 item 4
left it to manifests) gets its own PR. Skills and prompt templates (SKL-1
to SKL-4) get their own PR, which adds them to the manifest. A host tool
registry, from which tools that are not the agent type's own (MCP servers,
host-supplied tools; TOOL-1, TOOL-6) are granted, is expected to follow
soon; item 4 fixes how its tools fit.

It amends, item by item:

- ADR 0004 item 2 (`run_started` holds the digests of the manifest,
  prompts, skills and tool definitions): by item 11, `run_started` records
  the manifest's digest, which covers the tool definitions; prompts stay
  recorded per attempt; skills join with their PR.
- ADR 0007 item 2 (what an agent type declares and `Register` checks): by
  items 1, 3, 5, 7 and 9, an agent type also declares its trigger kinds,
  configuration schema and default budgets, and `Register` checks them.
- ADR 0007 item 6 (what ends a run before its start commit lands): by
  items 6, 8 and 11, an undeclared trigger kind and a configuration the
  schema rejects end it too, with `config_error`, and the ending records
  the manifest.
- ADR 0007 item 12 (`state_error`, a defect in the agent type or its
  input): by item 8, a run refused for its input (its trigger kind,
  configuration or budgets) fails with `config_error`; `state_error`
  keeps the agent type's own defects.
- ADR 0008 item 3 (what a `Tool` declares and `Register` refuses): by
  items 3 and 9, a tool declares a version and an approval mode, and
  `Register` refuses two tools of one name with different identities and
  any approval mode.
- ADR 0008 item 5 (the policy hook): by items 3 and 10, the request
  carries the tool's identity, and a `pending` verdict fails the run with
  `config_error`.
- ADR 0008 item 8 (entry payloads): by item 3, a `permission` entry
  records the identity of the tool the policy decided on.
- ADR 0008 item 11 (a recorded call whose tool the recovering version
  declares with another effect is refused): by item 3, so is one whose
  tool the recovering version declares with another identity.
- ADR 0008 item 16 (the policy hook's failure fails the run with
  `tool_error`, a `pending` verdict too): by item 10, a `pending` verdict
  fails it with `config_error`; an error, a panic or an unknown verdict
  still fails it with `tool_error`.
- ADR 0009 items 1 and 2 (what a zero budget means, where budgets come
  from, and how invalid ones fail): by items 7 and 8, an agent type's
  default budgets fill the fields the host leaves unset, so a zero token,
  turn or active-time budget in the host's input means no limit only if
  the type sets no default, and an invalid budget fails the run with
  `config_error`.

## Decision

### The manifest

1. **The agent type is the manifest's source.** `Type[D]` gains
   `Triggers`, `Config` and `Budgets` beside its name, version, states and
   transitions. **`Registry.Manifest(name)`** returns the registered
   type's `Manifest`: a plain value, serializable as JSON (format 1), with
   the name, version, initial state, each state (its kind, `Next`, ending,
   tools granted and wrap-up tools), each tool's declaration and identity,
   the trigger kinds, the configuration schema and the default budgets. It
   is computed once, at registration. There is no separately written
   manifest file to drift from the code.
2. **The manifest has a digest**: the SHA-256 of its JSON, which changes
   whenever anything it declares does (`Registry.ManifestDigest`).

### Tools

3. **A tool's content identity** is the SHA-256 of a fixed JSON encoding
   of its name, description, input schema, effect, write contract and
   **`Tool.Version`**, the version of the code that implements it. A Go
   function cannot be hashed, so the version is declared; an empty
   `Version` takes the agent type's, since a tool declared in a type
   ships with it. A tool name names one tool in an agent type: two states
   declaring tools of one name with different identities are refused at
   registration. The policy request (`PolicyRequest.Identity`) and the
   `permission` entry carry the identity, so a decision is bound to what
   it decided on (TOOL-4): recovery refuses a recorded call whose tool
   the recovering version declares with another identity, as it does one
   with another effect (ADR 0008 item 11), rather than executing a
   changed tool under the old decision; the model receives the refusal as
   data, and a write attempted with no result is still unknown. A
   permission recorded before manifests has no identity and is held to
   its effect alone. `llms.ToolDefinition` has no output schema, so
   the identity has none until it does.
4. **Tools from outside the agent type will join by name.** A host tool
   registry (TOOL-1, TOOL-6) grants its tools to states by name, with the
   same identity, manifest entry, policy path and record. Such a tool has
   no Go version to declare, so its identity's version comes from, in
   order: a version the host pins in its configuration (an image digest
   or package version), the version the server reports (MCP's
   `serverInfo.version`), or none. A tool with none is **unversioned**:
   its manifest entry has an empty version, its identity covers its
   definition alone, and a change to its implementation that leaves the
   definition alone does not change it; approval (TOOL-4) can treat such
   a tool's binding as weaker. A tool the agent type declares is
   unversioned only if neither it nor the type declares a version.

### Triggers and configuration

5. **Trigger kinds are declared and recorded.** `Type.Triggers` names the
   kinds the type accepts: at least one, each a non-empty string; the set
   is open, with constants for the kinds MODE-4 names (`one_shot`,
   `folder`, `schedule`). The kind is a property of the trigger, so it is
   stored with it: **`store.Run.TriggerKind`** and
   **`store.Trigger.Kind`**, set by the host. A run whose kind its agent
   type does not declare fails at its start with `config_error`.
6. **The configuration schema is JSON Schema, a documented subset,
   validated locally.** `Type.Config` is a schema; a type that declares
   none takes any configuration. Supported: `type` (one name or a list),
   `properties`, `required`, `additionalProperties` (a boolean or a
   schema), `items`, `enum`, `const`, `minimum`, `maximum`,
   `exclusiveMinimum`, `exclusiveMaximum`, `minLength`, `maxLength`,
   `pattern` (Go's RE2 syntax), `minItems`, `maxItems`, boolean schemas,
   and the annotations `$schema`, `$comment`, `title`, `description`,
   `default`, `examples` and `deprecated`, which validate nothing.
   **`Register` refuses a schema that uses any other keyword** (`$ref`,
   `oneOf`, `format` and the rest), or a supported keyword with an invalid
   value, so nothing in a schema goes silently unenforced. Numbers compare
   exactly (as rationals), so a number whose decimal exponent is beyond
   ±1000 is refused rather than expanded; a string's length counts code
   points. At
   the run's start the configuration is validated (an absent configuration
   as JSON `null`), and a configuration the schema rejects fails the run
   with `config_error`, the detail listing every problem with its JSON
   Pointer. The validator is internal (`internal/jsonschema`), about the
   size of a dependency's adapter; a third-party validator was rejected
   for the reason in the Context, and a Go type with a hand-written
   `Validate` because the published schema could drift from what is
   enforced.
7. **Default budgets fill what the host leaves unset.** `Type.Budgets`
   holds defaults for the token, turn and active-time budgets; at the
   run's start each of those the host's `ActivationInput.Budgets` leaves
   zero takes the default, and the result is what is recorded and fixed
   for the run (ADR 0009 item 2). A default deadline is refused at
   registration (the deadline is an absolute time, the host's to set), as
   is a default cost budget (not built, MVP.md BUD-1) and any default
   `Budgets.validate` refuses. A host cannot lift a default to no limit,
   since zero means "take the default"; it can set a larger limit.

### Refusals

8. **A run refused for its configuration fails with `config_error`**, a
   new RUN-3 reason (failed; a configuration error, so retrying as-is
   cannot help): an undeclared trigger kind, a configuration that is not
   valid JSON or that the schema rejects, an invalid budget, and a policy
   that asks for approval no state or tool provides for (item 10).
   `state_error` keeps its meaning: a defect in the agent type's code.
   The run's inputs are prepared before anything can refuse them, so a
   start refused for any of these records them (ADR 0007 item 6).
9. **Approval is declared and refused** (TOOL-4, MVP.md: a contract).
   `Tool.Approval` and `ModelState.Approval` take `propose` (propose and
   finish) or `suspend` (suspend and resume); empty is none. Approval is
   not built, so `Register` refuses a type that declares either.
10. **A `pending` verdict fails the run with `config_error`.** With no
    approval declared, nothing can follow a `pending` answer: the policy
    hook and the agent type disagree, which is the host's configuration
    to fix.

### The record

11. **`run_started` records the manifest and the trigger kind.**
    `RunStarted.Manifest` is the manifest's digest, its JSON stored as an
    artifact in the same commit (ADR 0004 item 1), and
    `RunStarted.TriggerKind` the kind. The manifest's tool identities
    cover the tool definitions offered. The system instructions and
    messages a model state sends depend on the run's data, so they are not
    the run's but each attempt's, recorded in its parameters' artifact
    (ADR 0008 item 8); grouping runs by prompt template, not by rendered
    prompt, comes with templates (SKL-4). This is MAN-4 as far as the
    agent type's behavior is declared today.

## Consequences

- An agent type must declare its trigger kinds, and a host must set each
  run's trigger kind: an existing type registers only once it declares
  some, and a run created with no kind fails at its start.
- Orchestrators read a type's manifest from the registry (morris#568
  item 2) and can group runs by its digest; a change to a tool's
  description or schema, or a new version of a type, changes it.
- A configuration the schema rejects, an undeclared trigger kind and an
  invalid budget no longer fail with `state_error`; hosts that classify
  retries by reason see `config_error`.
- The schema subset is small. A type that needs a keyword outside it
  declares a looser schema and checks the rest in `Init`, failing there
  with `state_error`; a keyword is added to the subset, with its tests,
  when a type needs it.
- Not built here, each with its own work: namespace confinement
  (STORE-2), skills and prompt templates (SKL-1 to SKL-4) and their place
  in the manifest, the host tool registry and the identity sources for
  its tools (item 4), and approval itself (TOOL-4).

## References

- [docs/requirements.md](../requirements.md): MAN-1 to MAN-4, TOOL-1,
  TOOL-4, TOOL-6, RUN-3, MODE-2, MODE-4, BUD-1, STORE-2, SKL-4
- [docs/MVP.md](../MVP.md): MAN-1 to 4 (Build), TOOL-4 (Contract)
- [ADR 0004](0004-execution-record-and-recovery.md),
  [ADR 0007](0007-agent-types-and-the-run-controller.md),
  [ADR 0008](0008-model-states.md), [ADR 0009](0009-budgets.md)
- morris#568 item 2 (the manifest an orchestrator reads)
