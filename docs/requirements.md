# maestro-agents Requirements

Status: **Draft** (2026-11-06). These are requirements, not a design: each
says *what* the library must do, and leaves *how* to the design ADRs that
follow. Where a requirement constrains the design in a non-obvious way, its
reason is given so a later ADR can weigh it rather than rediscover it.

**Sources.** Morris's requirements
([SnapdragonPartners/morris#568](https://github.com/SnapdragonPartners/morris/issues/568));
the standing requirement that agents run standalone; scope decisions taken
by a human operator during this requirements pass; and a survey of
`buggrit` and `maestro` for patterns to adopt and mistakes to avoid
(section 6). Maestro v2's accepted
[ADR 0032, The Agent Execution Contract](https://github.com/SnapdragonPartners/maestro/blob/main/docs/adr/0032-agent-execution-contract.md),
and its [ADR 0030, Tool Execution Policy Hook](https://github.com/SnapdragonPartners/maestro/blob/main/docs/adr/0030-tool-execution-policy-hook.md),
already settle much of the orchestrator boundary; this document aligns with
them and says where it does not.

**Conventions.** Requirements are numbered by area (`RUN-3`). **Must** is
required for the first release that a consumer adopts; **Should** is
expected but may follow; **Plan for** means the first release need not
implement it, but its design must not rule it out.

## 1. Purpose and consumers

`maestro-agents` is a Go library for building LLM agents. An *agent type*
is first-party Go code built on the library; an *agent run* is one bounded
execution of an agent type. The library supplies the runtime around the
agent type's logic: the model loop, tools, skills, budgets, events and the
seams to whatever hosts the run.

Known consumers:

| Consumer | How it uses the library |
|---|---|
| Morris | Compiles agent types into its private worker service and runs them in-process. Morris owns persistence, auth, credentials, UI and audit. Its chat loop should share the library's tool registry, policy layer and loop. First agent: capital-account statement PDFs to an Addepar valuation CSV (morris#569). |
| Standalone executables (`cmd/<agent>/`) | Run an agent type as a command-line tool or a long-running daemon, driven by a config file, with no orchestrator. |
| Maestro v2 | Plans to run agents as separate processes speaking a versioned wire contract (ADR 0032). Maestro has not built its side; the library plans for an external wire protocol and may counterpropose its shape. |

## 2. Principles

- **P-1 One runtime, three wrappers.** Compiled, standalone and wire modes
  (section 4) are wrappers over the same run controller and the same
  contract. A behavior available in one mode is available, with the same
  semantics, in the others, except where a section below says otherwise.
- **P-2 The library owns no infrastructure.** No storage, network service,
  authentication or UI in the core. Everything stateful is a seam the host
  implements. Reference implementations of those seams (in memory, file,
  database) live outside the core, in packages a compiled host need not
  import.
- **P-3 No credentials in agent code.** The model loop, the agent type's
  code and the tools' model-facing paths never see a credential (TOOL-8).
- **P-4 No global mutable state.** No self-registering global registries,
  configuration singletons or package-level metrics. Everything a run uses
  is passed to it. (Maestro v1's global tool registry and `GetConfig()`
  calls from inside tools are the counterexample.)
- **P-5 Facts are reported as data.** Model errors, deadlines, budget
  exhaustion, policy denials and incomplete runs come back as typed values,
  not as panics, log lines or silently empty results.
- **P-6 Small import tree.** The core depends on the Go standard library
  and `maestro-llms` (CLAUDE.md, *Dependency Discipline*). Database
  drivers, file watchers and transports sit in adapter packages.

## 3. Requirements

### 3.1 Runs (RUN)

- **RUN-1 Must: bounded runs.** A run takes a trigger payload, the instance
  configuration and the previous checkpoint, and returns a result,
  artifacts and a new checkpoint. A run is not a daemon; long-running
  behavior ("watch a folder") is a host firing triggered runs. (morris#568
  item 1)
- **RUN-2 Must: cancellation through `context`.** Cancelling the run's
  context stops it at the next safe point. A cancelled run leaves a
  well-formed transcript: a tool call interrupted mid-flight is either
  completed or recorded as interrupted, never left dangling (buggrit
  repairs this itself in `wellFormed`; the library owns it).
- **RUN-3 Must: a structured terminal result** that represents at least
  Morris's four terminal states (succeeded, failed, cancelled,
  budget-exhausted) and Maestro's distinctions: a success that changed
  nothing (`already_satisfied`), why a run was cancelled (superseded,
  operator, shutdown), and whether a failure is retryable infrastructure or
  a non-retryable agent failure. Invalid combinations must be
  unrepresentable or rejected (ADR 0032 binding item 7). The exact axes are
  a design decision; see open question Q-3.
- **RUN-4 Must: the terminal result is a claim.** The run reports what it
  believes happened; the host records the outcome and may override it with
  its own observations (for example, a run that reports success after its
  lease was revoked). The result type carries enough for the host to tell.
- **RUN-5 Must: checkpoints are opaque to the host.** A checkpoint is
  produced and read only by the agent type that wrote it, with a version so
  an agent type can refuse or migrate an old one. Recovery may be
  artifact-level (restart from the last committed artifact, as ADR 0032
  prefers) or checkpoint-level; the agent type chooses.
- **RUN-6 Must: a run is reproducible from its inputs.** Trigger, config,
  checkpoint, the resolved manifest and skill content identities, and the
  model responses are sufficient to replay a run (TEST-1).

### 3.2 Agent types and manifests (MAN)

- **MAN-1 Must: a manifest per agent type** declaring name, version,
  granted tools, skills, supported trigger kinds, the instance config
  schema and a default budget. (morris#568 item 2)
- **MAN-2 Must: the runtime enforces the manifest.** A tool not granted is
  neither offered to the model nor executable; a trigger kind not declared
  is refused before the run starts; instance config is validated against
  the schema before the run starts.
- **MAN-3 Must: agent types are registered explicitly**, by name, into a
  registry the host constructs. No fixed set of stages or roles in the
  library (buggrit's hard-coded stage struct and Maestro's three roles are
  the counterexample).
- **MAN-4 Should: content identity.** Each run records a digest of what the
  model was told and offered (system prompt, skill set, tool definitions),
  so telemetry, replay and evals group runs by behavior, not just by
  version string (buggrit `identity.go`; Maestro prompt packs, ADR 0031).

### 3.3 Model access (LLM)

- **LLM-1 Must: the host injects the model client.** The library accepts a
  `maestro-llms` client the host built and never builds one from its own
  configuration or reads a key. Morris routes every call through its own
  seam (Vertex over PSC, with its telemetry). (morris#568 item 3)
- **LLM-2 Must: multimodal input.** Agents can pass document pages to the
  model as document or image content. This needs upstream work (section 5).
- **LLM-3 Must: the route is explicit.** The model route is given, never
  inferred from a model name, and the run records the model identity as
  served, not only the alias requested (ADR 0032 binding item 3; Maestro
  v1's provider-from-name patterns needed special cases).
- **LLM-4 Should: one shared limiter seam** across concurrent runs in a
  process, using `maestro-llms`' limiter interfaces, so a host can plug in
  a distributed limiter (buggrit's Postgres bucket with local fallback).

### 3.4 Tools (TOOL)

- **TOOL-1 Must: one tool registry, exposure per consumer.** Tools have
  typed JSON Schema input and output. Each consumer (an agent type, or a
  host's chat loop) sees only the tools its grant names. An agent type may
  register dedicated tools only it can use. (morris#568 item 6)
- **TOOL-2 Must: effect classification.** Every tool is classified `read`
  or `write`. **Plan for** Maestro's richer classification: where the
  effect happens (host side, inside the agent's resource, external) and the
  commit point, so a host can reconcile a write whose outcome is unknown
  (Maestro v2 `boundary/family`).
- **TOOL-3 Must: one mandatory policy boundary.** Every tool call,
  including an agent type's dedicated tools, passes through a host-supplied
  policy hook before its effect. The hook can allow, deny, or require
  approval. A denial is returned to the model as data and the run
  continues. The boundary is structural: there is no path to a tool's
  effect that skips it (ADR 0030 §1).
- **TOOL-4 Must: propose mode for writes.** A write tool can return a
  pending action for the host to approve instead of executing. The run can
  then end in a state that says it is waiting on approval, and a later run
  resumes with the decision as input.
- **TOOL-5 Must: idempotency keys.** Every write call carries an
  idempotency key. The runtime derives a stable key per (run, step) so a
  retried run does not repeat a side effect. (morris#568 item 9)
- **TOOL-6 Must: registry, policy and transport are separate**, so a host
  can later expose the same registry over MCP or the wire protocol without
  changing tools. No MCP server now; the design must not rule one out.
- **TOOL-7 Must: tool middleware.** Cross-cutting behavior (policy,
  idempotency, redaction, the visible clock, output caps, per-tool circuit
  breaking) composes by wrapping a tool's execution, not by editing each
  tool (buggrit `clock.go`; Maestro's per-tool circuit breaker).
- **TOOL-8 Must: credentials at call time, through the host.** Every tool
  call carries an opaque run context, issued by the host and identifying
  the run and its principal. The tool asks the host for whatever credential
  that principal needs when it executes. Where a tool executes, in-process
  or forwarded, is the host's choice. (morris#568 item 4)
- **TOOL-9 Must: result redaction.** The host has a hook to redact tool
  results before the model sees them.
- **TOOL-10 Must: bounded output.** Tool output is capped, and a truncated
  result tells the model how to get the rest (buggrit `tools.go`).
- **TOOL-11 Should: non-text results.** A tool can return document or image
  content, not only text (section 5).

### 3.5 Skills and prompts (SKL)

- **SKL-1 Must: embedded skills.** Each agent type embeds its skills, a
  `SKILL.md` plus reference files, declared in the manifest. (morris#568
  item 5)
- **SKL-2 Must: progressive disclosure.** Skill descriptions go into the
  system prompt; bodies and references load on demand through a built-in
  read-skill tool.
- **SKL-3 Must: skills sit behind a source interface.** Compiled-in only
  for now; **plan for** admin-written skills from another source.
- **SKL-4 Should: prompts are embedded templates with content identity**
  (MAN-4), resolved once at run start and never re-resolved mid-run.

### 3.6 The agent loop (LOOP)

- **LOOP-1 Must: build on `maestro-llms/toolloop`**, extending it upstream
  where an extension is app-neutral, rather than forking a second loop.
  Agent concepts (manifests, budgets, policy) stay in this library;
  `toolloop` stays a tool loop (maestro-llms ADR 0011).
- **LOOP-2 Must: structured output through terminal tools.** An agent
  type's result is submitted through a terminal tool whose arguments are
  decoded strictly; all validation problems go back in one tool error so
  the model fixes them in one resubmission (buggrit `findings.go`).
- **LOOP-3 Must: the loop can force completion.** After a text-only or
  empty answer the loop nudges once; when the budget nears its end the loop
  forces the terminal tool, and the agent type can relax validation for
  that wrap-up turn. A run never ends silently without a result (buggrit
  `stage.go`; Maestro issue #317, where an architect deadlocked because the
  model never called its terminal tool).
- **LOOP-4 Must: the same registry, policy layer and loop are usable by a
  host's chat loop**, so a host has one tool path, not two. (morris#568
  item 7) A chat turn is not a run: it has no checkpoint or terminal tool,
  but it shares tools, policy, events and budgets.
- **LOOP-5 Should: compaction is optional and off by default.** When an
  agent type enables it, compaction never splits a tool call from its
  result, and the event stream records that it happened. (buggrit chose
  never to compact so replay stays faithful; Maestro v1's drop-oldest
  window orphaned tool results.)

### 3.7 Budgets (BUD)

- **BUD-1 Must: budgets in tokens, steps and wall clock**, with defaults
  from the manifest and per-run overrides from the host. The runtime
  enforces every budget it is given; a configured budget that is not
  enforced is a defect (buggrit's `MaxTokensPerPass` is the
  counterexample). Zero means no limit.
- **BUD-2 Must: wall clock is a first-class budget**, with a reserve for
  wrap-up, and the model is shown the clock (buggrit design §9: slow
  reasoning models spent whole budgets on single turns).
- **BUD-3 Should: cost is reported as usage, not priced.** The library
  reports tokens per model; pricing is the host's. An unpriced model is an
  explicit unknown, never $0 (Maestro's wishlist).

### 3.8 Host seams (HOST)

The host implements these; the library calls them. (morris#568 item 8)

- **HOST-1 Must: event stream.** Model turns, tool calls, policy decisions,
  usage and state changes are emitted as events the host persists as the
  transcript. Events have a stable identity (run, sequence) so a host can
  deduplicate redelivered events. A nil sink records nothing.
- **HOST-2 Must: artifact store and checkpoint store.**
- **HOST-3 Must: control plane** for lease, heartbeat and cancellation.
  Liveness comes from heartbeats, not from output silence.
- **HOST-4 Must: credential provider** (TOOL-8) and **policy hook**
  (TOOL-3), both keyed by the opaque run context.
- **HOST-5 Must: fencing.** Before each write's effect, the runtime checks
  that the run still holds its lease and refuses the effect if not (buggrit
  `fence.go` re-reads the PR head before each mutation; ADR 0032 binding
  item 9).
- **HOST-6 Should: cancellation order.** A forced end closes admission of
  new tool calls, lets admitted calls finish or reach their deadline, and
  only then emits the terminal result (ADR 0032 binding item 8).

### 3.9 Delivery (DEL)

- **DEL-1 Must: at-least-once runs.** A run may be delivered more than
  once. Idempotency keys (TOOL-5) and fencing (HOST-5) keep a retried run
  from repeating a side effect.
- **DEL-2 Should: event delivery guarantees by kind.** Usage and
  provenance events are delivered at least once; heartbeat and progress are
  best effort. Incomplete event delivery is reported, not hidden (buggrit's
  telemetry writer marks a run complete only if every event was written).

### 3.10 Testability (TEST)

- **TEST-1 Must: replay harness.** Drive an agent type from recorded model
  responses against fake host seams, deterministically and without network.
  (morris#568 item 10)
- **TEST-2 Must: fakes for every seam**, usable by agent-type authors and
  hosts, built on `maestro-llms/testllm`.
- **TEST-3 Should: record mode.** A live run can record its model exchange
  in the form the replay harness consumes.

## 4. Run modes (MODE)

All three modes drive the same run controller (P-1).

### 4.1 Compiled (Morris)

- **MODE-1 Must:** a host imports the library, registers agent types and
  calls the run controller with its seam implementations. No mode-specific
  behavior beyond that.

### 4.2 Standalone

A standalone executable is a small host that ships with the library,
modeled on buggrit: a declarative config file says what to do, and
pluggable sources and sinks hold inputs, state and output.

- **MODE-2 Must: config file.** One file declares the agent type, its
  instance config, triggers, budgets, the model route and which sources and
  sinks to use. It is decoded strictly (unknown fields are errors) and
  validated before anything runs, with a `config validate` command. The
  format is JSON: it is in the standard library and avoids YAML's
  whitespace pitfalls (decided in this requirements pass).
- **MODE-3 Must: pluggable sources and sinks** implementing the host seams:
  in memory (sufficient for a long-running process that need not survive
  restart), file based, and database based. Database drivers sit in adapter
  packages (P-6).
- **MODE-4 Must: triggers.** One-shot (run once and exit, for CLI and CI
  use) and long-running (a daemon that fires triggered runs on a schedule
  or on an event such as a new file). A daemon is a host firing bounded
  runs (RUN-1), not a long-lived run.
- **MODE-5 Must: credentials stay outside agent code.** The standalone
  host implements the credential provider from environment variables or a
  secrets file. Only the host reads them; agent types and tools still go
  through TOOL-8.
- **MODE-6 Must: graceful shutdown.** SIGINT/SIGTERM cancel runs through
  their contexts, with exit codes distinguishing complete, incomplete and
  usage errors (buggrit's `0/1/2`).
- **MODE-7 Should: optional infrastructure degrades.** A failing optional
  sink (telemetry, a shared limiter) is a warning, not a failed run; a
  required one fails before the run starts.

### 4.3 Wire protocol

- **MODE-8 Plan for: an external wire protocol** exposing the same
  contract as the Go interfaces (run inputs and result, events, control,
  tool calls forwarded to the host) to an orchestrator in another process
  or language. Maestro v2's ADR 0032 (newline-delimited JSON over stdio) is
  the reference; Maestro has not built its side, so the shape may be
  deferred or counterproposed.
- **MODE-9 Plan for: versioning and handshake.** The protocol is versioned,
  with capabilities negotiated separately; a version mismatch fails before
  the run starts. A framing violation is fatal; a policy denial is data.
- **MODE-10 Plan for: authentication.** The peer is authenticated and every
  message carries the run's authority, so a request from a superseded or
  fenced run is rejected. Stdio inherits the parent's trust; any socket
  transport needs an explicit mechanism (Maestro's MCP bridge uses a token
  over TCP loopback because Docker Desktop cannot pass Unix sockets).
- **MODE-11 Plan for: no local paths or data-plane access across the
  wire.** The orchestrator passes references, never filesystem paths,
  database connections or credentials (ADR 0032 binding items 4 and 5).

## 5. Out of scope, and upstream dependencies

**Out of scope for the core library:** persistence, HTTP serving,
authentication, UI, an MCP server, and knowledge of any particular host.
(Reference seam implementations for standalone mode are in scope, in
separate packages.) Maestro-style agent-to-agent messaging, role state
machines and container sandboxing are not requirements; they may return if
a consumer needs them.

**Upstream work in `maestro-llms`.** The library must not work around these
locally:

| Need | Gap in maestro-llms | Requirement |
|---|---|---|
| Image and document content | `ContentPart` has only text, tool call, tool result and reasoning parts | LLM-2 |
| Non-text tool results | `toolloop.ToolResult.Content` is a string | TOOL-11 |
| Forced wrap-up, turn-boundary stop that still runs sibling calls, a pre-request hook that may fail | Not in `toolloop`; Maestro's wishlist items 1 and 2 | LOOP-1, LOOP-3 |

## 6. Lessons from buggrit and maestro

What is adopted is decided per piece in an ADR when the work starts
(CLAUDE.md). This section records the survey so those ADRs start from it.

**Adopt from buggrit:**

- The single loop wrapper: investigate, nudge once, forced wrap-up, outcomes
  with reasons (`internal/review/stage.go`).
- Terminal tools with strict decoding and all-at-once validation errors
  (`findings.go`).
- Tool middleware by wrapping execution, and the visible clock
  (`clock.go`).
- Telemetry: nil-safe recorder, requests stored as deltas, a `Sink`
  interface with idempotent writes and honest completeness
  (`internal/telemetry`).
- Prompt and tool content hashing (`identity.go`).
- The head fence before each side effect (`internal/github/fence.go`).
- The replay and benchmark harness (`internal/bench`).
- Engine takes interfaces; env vars and front-end specifics stay in `cmd/`.

**Avoid from buggrit:** the agent reading its own API key
(`internal/llm/llm.go`); a fixed stage set; configured but unenforced
budgets; durable state in side channels instead of explicit checkpoints;
results passed through closure side effects.

**Adopt from maestro:** the v2 execution contract's binding items (ADR
0032) as the target for the wire mode and the shape of results, events and
cancellation; the v2 action-family registry (effect site, commit point,
reconcile probe, attempt ID as idempotency key); one mandatory policy
boundary (ADR 0030); per-tool circuit breaking; content-digested prompts
resolved once; `already_satisfied` as an explicit outcome; reported and
observed facts kept distinct.

**Avoid from maestro (v1):** global mutable registries and config
singletons; tools holding a back-reference to the agent; flattened outcome
enums that mix loop, transport and workflow facts; two channels that can
disagree about a result; untyped, non-atomic checkpoints; recovery that
does not survive a restart; fire-and-forget audit writes; credential
lookup in the agent factory; drop-oldest compaction; iteration-only
budgets.

**New design work** (neither repository has it): manifests, embedded
skills, propose mode, read/write classification, recorded-response replay
of an agent type, and the three-mode wrapping.

## 7. Open questions

- **Q-1 Wire protocol timing.** Design the wire mode with Maestro now, or
  build compiled and standalone first and keep the wire mode at "plan for"
  until Maestro's Phase 3 needs it?
- **Q-2 Approval round trip.** When a write is proposed (TOOL-4), does the
  run end and a new run resume with the decision (bounded, as RUN-1
  implies), or may a run wait in place? ADR 0032 has a durable named wait;
  Morris's model suggests ending the run.
- **Q-3 Result axes.** Adopt ADR 0032's four axes as they are, or a smaller
  model that maps onto them? Where does budget-exhausted sit: a failure
  class, or its own status?
- **Q-4 Shared limiter and telemetry.** Do the standalone database sinks
  share a schema or package with buggrit's Postgres telemetry and bucket
  limiter, or start fresh?
- **Q-5 Acceptance.** Morris's acceptance is that morris#569 runs on the
  library and Morris's chat tools move onto its registry. Is there a
  standalone acceptance agent, and does buggrit itself move onto the
  library later?
