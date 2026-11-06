# maestro-agents Requirements

Status: **Draft** (2026-11-06). These are requirements, not a design: each
says *what* the library must do and leaves *how* to the design ADRs that
follow. Where a requirement constrains the design in a non-obvious way, its
reason is given so a later ADR can weigh it rather than rediscover it.

**Sources, and how much weight they carry.** Morris's requirements
([SnapdragonPartners/morris#568](https://github.com/SnapdragonPartners/morris/issues/568)),
Maestro v2's accepted
[ADR 0032, The Agent Execution Contract](https://github.com/SnapdragonPartners/maestro/blob/main/docs/adr/0032-agent-execution-contract.md)
and [ADR 0030, Tool Execution Policy Hook](https://github.com/SnapdragonPartners/maestro/blob/main/docs/adr/0030-tool-execution-policy-hook.md),
the draft requirements for the first agent (capital-statement intake for
Addepar), scope decisions taken by a human operator during this
requirements pass, and a survey of `buggrit` and `maestro` (section 8).
Neither Morris's agent manager nor Maestro v2 is built yet, so their
requirements are **inputs, not constraints**: where this document departs
from one, it says so and why (marked **Departs from**).

**Conventions.** Requirements are numbered by area (`RUN-3`). **Must** is
required for the first release (the MVP, section 6); **Should** is expected
but may follow; **Plan for** means the first release need not implement
it, but its design must not rule it out. Every behavior has a default, so
an agent type configures only what it changes.

## 1. Purpose and consumers

`maestro-agents` is a Go foundation for building LLM agents as **state
machines**. An *agent type* is first-party Go code built on the library: a
set of states, the transitions between them, and the tools, skills and
prompts its model-driven states use. An *agent instance* is one configured
deployment of an agent type; a *run* is one stretch of an instance's
execution, from receiving an input to reaching a terminal state or
stopping at a step boundary. The library supplies the runtime around the
agent type's logic: the state machine engine, the model loop, tools,
skills, budgets, events and the seams to whatever hosts the run.

Known consumers:

| Consumer | How it uses the library |
|---|---|
| Standalone executables (`cmd/<agent>/`) | Run an agent type as a command-line tool or a daemon, driven by a config file, with no orchestrator. **The first consumer**: the standalone capital-statement intake agent (section 6). |
| Morris | Compiles agent types into its private worker service, which runs on serverless infrastructure, and runs them in-process. Morris owns persistence, auth, credentials, UI and audit. Its chat loop should share the library's tool registry, policy layer and loop. Its first agent is the same capital-statement intake agent (morris#569). |
| Maestro v2 | Plans to run agents as separate processes speaking a versioned wire contract (ADR 0032). Maestro has not built its side; the wire protocol is deferred (MODE-8). |
| buggrit | Ideally moves onto the library once it exists; not a requirement. |

## 2. Principles

- **P-1 One runtime, three wrappers.** Compiled, standalone and wire modes
  (section 5) wrap the same run controller and the same contract. A
  behavior available in one mode is available, with the same semantics, in
  the others, except where a section below says otherwise.
- **P-2 The library owns no infrastructure.** No storage, network service,
  authentication or UI in the core. Everything stateful is a seam the host
  implements. Reference implementations of those seams (in memory, file,
  database) live outside the core, in packages a compiled host need not
  import.
- **P-3 No credentials in agent code.** The state machine, the model loop,
  the agent type's code and the tools' model-facing paths never see a
  credential (TOOL-8).
- **P-4 No global mutable state.** No self-registering global registries,
  configuration singletons or package-level metrics. Everything a run uses
  is passed to it. (Maestro v1's global tool registry and `GetConfig()`
  calls from inside tools are the counterexample.)
- **P-5 Facts are reported as data.** Model errors, deadlines, budget
  exhaustion, policy denials and incomplete runs come back as typed values,
  not as panics, log lines or silently empty results.
- **P-6 The interaction layer is JSON.** Every value that crosses a seam
  between the runtime and its host or its tools (run inputs and results,
  events, checkpoints' envelopes, tool calls and results, policy
  decisions) has a JSON form, and no seam passes Go callbacks or closures
  as part of its contract. MCP tools, skills and a future wire protocol
  then reuse the same values. (Maestro v1 already passes JSON messages,
  over Go channels.)
- **P-7 Small import tree.** The core depends on the Go standard library
  and `maestro-llms` (CLAUDE.md, *Dependency Discipline*). Database
  drivers, file watchers, MCP and transports sit in adapter packages.

## 3. Requirements

### 3.1 State machine (STM)

- **STM-1 Must: an agent type is a state machine** with a declared set of
  states, a declared set of allowed transitions, and an initial state. A
  transition not declared is rejected at runtime, and the declaration is
  checked when the agent type is registered.
- **STM-2 Must: two kinds of state.** A *code state* runs the agent type's
  Go code and chooses the next state. A *model state* runs a tool loop
  (LOOP) and leaves it when a tool's effect or the loop's outcome causes a
  transition. An agent type can mix both; a single-state agent that is one
  tool loop must still be simple to write.
- **STM-3 Must: tools can cause transitions.** A tool's result can carry a
  transition, with data, for the state machine to apply (Maestro v1's
  `ProcessEffect` is the precedent). What ends a model state is entering a
  new state, not the call of a particular tool.
- **STM-4 Must: terminal states.** Each state is terminal or not. A run
  that enters a terminal state ends with that state's result (RUN-3). The
  agent type decides what makes a state terminal; for example, the intake
  agent's run is complete when every statement in the document has been
  submitted and verified, set aside, or sent to manual review.
- **STM-5 Must: typed state data.** The data carried between states is the
  agent type's own Go type, serialized as JSON at step boundaries (P-6),
  not an untyped map (Maestro v1's `StateData map[string]any` is the
  counterexample).
- **STM-6 Must: step boundaries.** A step boundary is a completed model
  turn or a state transition. At every step boundary the instance's state
  (current state, state data, and the transcript of a model state in
  progress) can be checkpointed and later restored, so a run can resume
  from its last boundary in a new process. A model turn is recorded,
  including the tool calls it requested, before any of those calls
  execute; a run resumed after that point executes the recorded calls and
  does not ask the model to repeat the turn.
- **STM-7 Must: the host decides how many steps one process covers.** A
  serverless host may checkpoint and stop at every boundary; a daemon may
  run indefinitely, checkpointing as it goes. A daemon is therefore not a
  different kind of agent: it is the same state machine with a process
  lifetime long enough to be effectively unbounded. **Departs from**
  morris#568 item 1 ("bounded runs, not daemons") in wording, not
  substance: what Morris needs is state restorable at bounded intervals,
  which STM-6 gives every agent.
- **STM-8 Must: waiting is a state.** An instance waiting for external
  input (the next trigger, an approval) sits in a non-terminal state that
  names what it waits for. A daemon waits in-process; a serverless host
  stops the run as `suspended` (RUN-3) and starts a new one when the input
  arrives.

### 3.2 Runs (RUN)

- **RUN-1 Must: inputs and outputs.** A run takes a trigger payload, the
  instance configuration and the previous checkpoint, and returns a
  result, artifacts and a new checkpoint. (morris#568 item 1)
- **RUN-2 Must: cancellation through `context`.** Cancelling the run's
  context stops it at the next safe point, with the host's reason taken
  from the context's cause (`context.WithCancelCause`). A cancelled run
  leaves a well-formed transcript: a tool call interrupted mid-flight is
  either completed or recorded as interrupted, never left dangling (buggrit
  repairs this itself in `wellFormed`; the library owns it).
- **RUN-3 Must: a typed result with four statuses**, each carrying a reason
  that is valid only for that status, so an invalid combination cannot be
  constructed:

  | Status | Reason | Maestro ADR 0032 equivalent |
  |---|---|---|
  | `succeeded` | `changed` or `no_change` | completed, with disposition changed or already_satisfied |
  | `failed` | `budget_exhausted` (naming the budget), `model_error`, `tool_error` or `invalid_output`; each reason states whether retrying as-is can help | failed, with failure class; `timed_out` is `failed` with the wall-clock budget |
  | `cancelled` | the cause the host supplied: superseded, operator, shutdown or lease lost | cancelled, with cancel reason |
  | `suspended` | the input the instance is waiting for (STM-8), including any pending actions (TOOL-4) | blocked |

  A daemon returns only when it stops: `cancelled` with cause shutdown,
  its checkpoint intact, or a terminal status if its agent type has a
  terminal state. **Departs from** morris#568 item 1, which lists
  budget-exhausted as its own terminal state: it is a failure here, but its
  checkpoint stays valid, so a host can resume it with a larger budget.
  **Departs from** ADR 0032's four axes, which this table maps onto with
  fewer moving parts.
- **RUN-4 Must: the result is a claim.** The run reports what it believes
  happened; the host records the outcome and may override it with its own
  observations (for example, a run that reports success after its lease
  was revoked). The result carries enough for the host to tell.
- **RUN-5 Must: checkpoints are opaque to the host.** The host stores and
  returns them but does not read their contents. A checkpoint carries a
  version so an agent type can refuse or migrate an old one.
- **RUN-6 Must: a run is reproducible from its record.** Trigger, config,
  checkpoint, the content identities of the manifest, prompts, skills and
  tool definitions, and the model responses are sufficient to replay a run
  (TEST-1). Explaining a past result never requires calling the model
  again.

### 3.3 Agent types and manifests (MAN)

- **MAN-1 Must: a manifest per agent type** declaring name, version,
  states and transitions (STM-1), granted tools, skills, supported trigger
  kinds, the instance config schema and a default budget. (morris#568
  item 2)
- **MAN-2 Must: the runtime enforces the manifest.** A tool not granted is
  neither offered to the model nor executable; a trigger kind not declared
  is refused before the run starts; instance config is validated against
  the schema before the run starts.
- **MAN-3 Must: agent types are registered explicitly**, by name, into a
  registry the host constructs. The library has no fixed set of stages or
  roles (buggrit's hard-coded stage struct and Maestro's three roles are
  the counterexample).
- **MAN-4 Should: content identity.** Each run records a digest of what the
  model was told and offered (system prompt, skill set, tool definitions),
  so telemetry, replay and evals group runs by behavior, not just by
  version string (buggrit `identity.go`; Maestro prompt packs, ADR 0031).

### 3.4 Model access (LLM)

- **LLM-1 Must: the host injects the model client.** The library accepts a
  `maestro-llms` client the host built, and never builds one from its own
  configuration or reads a key. Morris routes every call through its own
  seam (Vertex over PSC, with its telemetry). (morris#568 item 3)
- **LLM-2 Must: multimodal input.** Agents can pass a PDF, or its pages, to
  the model as document or image content. This needs upstream work
  (section 7).
- **LLM-3 Must: the route is explicit.** The model route is given, never
  inferred from a model name, and the run records the model identity as
  served, not only the alias requested (ADR 0032 binding item 3; Maestro
  v1's provider-from-name patterns needed special cases).
- **LLM-4 Must: rate limiting is the host's limiter, and waiting is not a
  result.** The library uses `maestro-llms`' limiter (the `bucket` package,
  in-memory or Postgres) as injected by the host. A call held by the
  limiter blocks until capacity is free or the run's context or wall-clock
  budget ends; being rate limited never fails a run on its own.

### 3.5 Tools (TOOL)

- **TOOL-1 Must: one tool registry, exposure per consumer.** Tools have
  typed JSON Schema input and output. Each consumer (an agent type, a state
  within it, or a host's chat loop) sees only the tools its grant names. An
  agent type may register dedicated tools only it can use. (morris#568
  item 6)
- **TOOL-2 Must: effect classification.** Every tool is classified `read`
  or `write`. **Plan for** Maestro's richer classification: where the
  effect happens (host side, inside the agent's resource, external) and the
  commit point, so a host can reconcile a write whose outcome is unknown
  (Maestro v2 `boundary/family`).
- **TOOL-3 Must: one mandatory policy boundary.** Every tool call,
  including an agent type's dedicated tools and MCP tools, passes through a
  host-supplied policy hook before its effect. The hook answers `allow`,
  `deny` or `pending` (approval required). A denial is returned to the
  model as data and the run continues. The boundary is structural: there is
  no path to a tool's effect that skips it (ADR 0030 §1). The default hook
  allows everything.
- **TOOL-4 Must: approval through policy, not per-tool modes.** When the
  hook answers `pending`, the runtime records a pending action, bound to a
  digest of the exact tool name and arguments, and does not execute it.
  The agent type declares, per tool or per state, which of two things
  follows:
  - *Propose and finish.* The pending action is part of the run's output;
    approving and carrying it out happen outside the agent, which never
    sees the outcome. (The intake agent works this way: approval and export
    are human actions in its app.)
  - *Suspend and resume.* The run checkpoints and ends `suspended`. When
    the host later starts a run with the decision as input, the runtime
    executes the approved action, with its original idempotency key, only
    if the digest still matches, and returns the result to the model.

  A host that can get a decision quickly (a chat user) may simply block
  inside the hook and return `allow` or `deny`; the library needs nothing
  more for that. **Departs from** morris#568 item 6, which gives write tools
  their own propose mode: approval is a policy outcome here, so no tool
  implements it.
- **TOOL-5 Must: idempotency keys.** Every write call carries an
  idempotency key that is distinct for each logical tool invocation and
  stable across redelivery, so a retried run does not repeat a side effect
  and two writes in one model turn (two `submit_extraction` calls for
  different statements, say) never share a key. The runtime derives it
  from the run, the step, and the call's identity within that step (for
  example, its position in the recorded turn), never from the step alone.
  Write calls execute only from a model turn already recorded (STM-6), so
  a redelivered run re-issues the same calls with the same keys rather
  than asking the model again. **Departs from** morris#568 item 9, which
  asks for one key per (run, step): a step can hold several writes.
- **TOOL-6 Should: MCP tools.** The registry can hold tools served by an
  external MCP server alongside in-process Go tools, with the same grants,
  policy, middleware and events. The MCP client itself comes from
  `maestro-llms`, not from this library (section 7). The MVP does not
  depend on it (section 6). **Plan for** the reverse: exposing the
  registry over MCP or the wire protocol. Registry, policy and transport
  stay separate so neither direction changes tools.
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
- **TOOL-11 Must: non-text results.** A tool can return document or image
  content, not only text (section 7). The intake agent's `get_document`
  returns the PDF itself for visual reading.

### 3.6 Skills and prompts (SKL)

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

### 3.7 The model loop (LOOP)

- **LOOP-1 Must: build on `maestro-llms/toolloop`**, extending it upstream
  where an extension is app-neutral, rather than forking a second loop.
  Agent concepts (states, manifests, budgets, policy) stay in this
  library; `toolloop` stays a tool loop (maestro-llms ADR 0011).
- **LOOP-2 Must: validated submissions.** A tool that submits structured
  output decodes its arguments strictly and returns every validation
  problem in one tool error, so the model fixes them in one resubmission
  (buggrit `findings.go`). The agent type can cap resubmissions; reaching
  the cap is a transition the agent type defines (the intake agent sends
  the document to manual review).
- **LOOP-3 Must: the loop can force progress.** After a text-only or empty
  answer the loop nudges the model once; when the state's budget nears its
  end the loop forces the tools that leave the state, and the agent type
  can relax validation for that wrap-up turn. A model state never ends
  silently without a transition (buggrit `stage.go`; Maestro issue #317,
  where an architect deadlocked because the model never called its exit
  tool).
- **LOOP-4 Must: usable by a host's chat loop.** The same registry, policy
  layer and loop serve a host's chat, so a host has one tool path, not
  two. (morris#568 item 7) A chat turn is not a run: it has no state
  machine or checkpoint, but it shares tools, policy, events and budgets.
- **LOOP-5 Must: summarization is available, off by default.** An agent
  type can enable `maestro-llms`' compaction per model state. Compaction
  never separates a tool call from its result (maestro-llms ADR 0038), and
  the event stream records each compaction.

### 3.8 Budgets (BUD)

- **BUD-1 Must: budgets are totals**: tokens, model turns, wall clock and,
  when the host supplies prices, cost. Defaults come from the manifest;
  the host can override them per run. The runtime enforces every budget it
  is given; a configured budget that is not enforced is a defect (buggrit's
  `MaxTokensPerPass` is the counterexample). Zero means no limit.
  Exhausting a budget fails the run with `budget_exhausted` (RUN-3). Rate
  limits are not budgets (LLM-4).
- **BUD-2 Must: wall clock is a first-class budget**, with a reserve for
  wrap-up, and the model is shown the clock (buggrit design §9: slow
  reasoning models spent whole budgets on single turns).
- **BUD-3 Must: the library does not price.** It reports tokens per
  model; a cost budget uses a pricing function the host supplies. An
  unpriced model is an explicit unknown, never $0 (Maestro's wishlist).

### 3.9 Host seams (HOST)

The host implements these; the library calls them. (morris#568 item 8)

- **HOST-1 Must: event stream.** State transitions, model turns, tool
  calls, policy decisions, usage and compactions are emitted as events the
  host persists as the transcript. Events have a stable identity (run,
  sequence) so a host can deduplicate redelivered events. A nil sink
  records nothing.
- **HOST-2 Must: event redaction.** The host can classify or redact event
  content before it leaves the run, so a transcript can carry hashes and
  IDs instead of document text or amounts (the intake agent's
  confidentiality requirement, its N-5).
- **HOST-3 Must: stores**, defined in section 4: checkpoints, artifacts,
  run records and a per-instance object store.
- **HOST-4 Must: control plane** for lease, heartbeat and cancellation.
  Liveness comes from heartbeats, not from output silence.
- **HOST-5 Must: credential provider** (TOOL-8) and **policy hook**
  (TOOL-3), both keyed by the opaque run context.
- **HOST-6 Must: fencing.** Before each write's effect, the runtime checks
  that the run still holds its lease and refuses the effect if not (buggrit
  `fence.go` re-reads the PR head before each mutation; ADR 0032 binding
  item 9).
- **HOST-7 Should: cancellation order.** A forced end closes admission of
  new tool calls, lets admitted calls finish or reach their deadline, and
  only then emits the result (ADR 0032 binding item 8).

### 3.10 Delivery (DEL)

- **DEL-1 Must: at-least-once runs.** A run may be delivered more than
  once. Idempotency keys (TOOL-5), fencing (HOST-6) and conditional writes
  to the object store (STORE-2) keep a retried run from repeating a side
  effect.
- **DEL-2 Should: event delivery guarantees by kind.** Usage and
  provenance events are delivered at least once; heartbeat and progress are
  best effort. Incomplete event delivery is reported, not hidden (buggrit's
  telemetry writer marks a run complete only if every event was written).

### 3.11 Testability (TEST)

- **TEST-1 Must: replay harness.** Drive an agent type from recorded model
  responses against fake host seams, deterministically and without network.
  (morris#568 item 10)
- **TEST-2 Must: fakes for every seam**, usable by agent-type authors and
  hosts, built on `maestro-llms/testllm`.
- **TEST-3 Should: record mode.** A live run can record its model exchange
  in the form the replay harness consumes.
- **TEST-4 Should: scoring harness.** Run an agent type over a set of
  inputs and compare its outputs to an answer key, reporting matches,
  mismatches, exceptions and token use per input (the intake agent's build
  step 3; buggrit's `internal/bench`).

## 4. Storage (STORE)

The library defines the store interfaces and a reference schema. Morris
implements the interfaces over its own database; standalone mode uses the
reference implementations (MODE-3). An agent type's own data goes in the
object store rather than in tables of its own.

- **STORE-1 Must: fixed records.** *Runs*: ID, agent type and version,
  instance, status and reason, trigger, timestamps. *Checkpoints*: per
  instance, versioned, opaque. *Events*: run, sequence, kind, payload.
  *Artifacts*: content-addressed blobs with metadata.
- **STORE-2 Must: a per-instance object store.** Keys map to JSON or
  bytes, in one namespace per agent instance; an instance cannot read
  another's namespace. Every write is conditional on a version number
  (compare-and-set), because redelivered runs are normal (DEL-1). Keys can
  be listed by prefix. That covers the intake agent's needs: "has this file
  hash been seen?" is a lookup, and "records awaiting review" is a prefix
  listing.
- **STORE-3 Must: the object store stays simple.** No secondary indexes or
  query language. An agent type that needs relational queries owns a store
  outside the library, behind its own interface.
- **STORE-4 Must: three reference implementations**: in memory (enough for
  a daemon whose state need not outlive the process), files (JSON files
  written by atomic rename, so no database driver is needed), and
  Postgres (in an adapter package, P-7).
- **STORE-5 Must: one Postgres database, schemas side by side.** The
  Postgres store shares its database with `maestro-llms`' token bucket
  (`bucket/postgres`), which standalone mode always includes (memory or
  Postgres backend, LLM-4). Each keeps its own Postgres schema. Following
  the bucket's pattern (maestro-llms ADR 0025), the store exports its
  migrations in golang-migrate's format and a required schema version,
  never runs DDL itself, and refuses to start against an older schema; the
  standalone host applies both sets of migrations. The stack is the one
  buggrit uses: golang-migrate for migrations, sqlc for queries, pgx as the
  driver.

## 5. Run modes (MODE)

All three modes drive the same run controller (P-1).

### 5.1 Compiled

- **MODE-1 Must:** a host imports the library, registers agent types and
  calls the run controller with its seam implementations. No mode-specific
  behavior beyond that. (Morris)

### 5.2 Standalone

A standalone executable is a small host that ships with the library,
modeled on buggrit: a declarative config file says what to do, and
pluggable stores hold inputs, state and output.

- **MODE-2 Must: config file.** One JSON file declares the agent type, its
  instance config, triggers, budgets, the model route, MCP servers and
  which stores to use. It is decoded strictly (unknown fields are errors)
  and validated before anything runs, with a `config validate` command.
  JSON is in the standard library and avoids YAML's whitespace pitfalls.
- **MODE-3 Must: pluggable stores** from STORE-4, chosen in the config.
- **MODE-4 Must: triggers.** One-shot (process one input and exit, for
  command-line and CI use) and long-running (wait for triggers such as a
  schedule or a new file in a folder, and run the instance on each, per
  STM-7 and STM-8).
- **MODE-5 Must: credentials stay outside agent code.** The standalone
  host implements the credential provider from environment variables or a
  secrets file. Only the host reads them; agent types and tools still go
  through TOOL-8.
- **MODE-6 Must: graceful shutdown.** SIGINT and SIGTERM cancel runs
  through their contexts (cause: shutdown), checkpointing at the last step
  boundary, with exit codes distinguishing complete, incomplete and usage
  errors (buggrit's `0/1/2`).
- **MODE-7 Should: optional infrastructure degrades.** A failing optional
  sink (telemetry, a shared limiter) is a warning, not a failed run; a
  required one fails before the run starts.

### 5.3 Wire protocol

Deferred until Maestro v2 needs it. P-6 keeps it a thin wrapper when it
comes.

- **MODE-8 Plan for: an external wire protocol** exposing the same
  contract as the Go interfaces (run inputs and result, events, control,
  tool calls forwarded to the host) to an orchestrator in another process
  or language. Maestro's ADR 0032 (newline-delimited JSON over stdio) is
  the reference; the shape may be counterproposed.
- **MODE-9 Plan for: versioning and handshake.** The protocol is versioned,
  with capabilities negotiated separately; a version mismatch fails before
  the run starts. A framing violation is fatal; a policy denial is data.
- **MODE-10 Plan for: authentication.** The peer is authenticated and every
  message carries the run's authority, so a request from a superseded or
  fenced run is rejected. Stdio inherits the parent's trust; a socket
  transport needs an explicit mechanism (Maestro's MCP bridge uses a token
  over TCP loopback because Docker Desktop cannot pass Unix sockets).
- **MODE-11 Plan for: no local paths or data-plane access across the
  wire.** The orchestrator passes references, never filesystem paths,
  database connections or credentials (ADR 0032 binding items 4 and 5).

## 6. The MVP: standalone capital-statement intake

The first agent built on the library is the standalone capital-statement
intake agent: one PDF statement in, a verified valuation record and an
Addepar import row out. Its requirements are drafted separately (the
*Capital Statement Intake* requirements; not in this repository). It is the
MVP because it has no external blockers and has users waiting for it. It
lives in this repository as `cmd/addepar`, both as a usable tool and as
the reference implementation of an agent type; it may move later. The
same agent type later runs compiled into Morris (morris#569), which is
Morris's acceptance test.

**Departs from** the intake draft's recommendation to prototype in Python.
Its one advantage was PDF tooling; as in Morris (Morris ADR 0047), the PDF
work goes to an external tool, Poppler, run as a subprocess. That
capability belongs in `maestro-cms`, which already runs Poppler's
`pdftotext` for Morris (`extract/pdf/pdftotext`) and is where other
consumers will look for it. It needs extending with what the intake agent
requires and the engine does not yet provide: word positions per page (for
the grounding check) and page rendering (section 7). The intake agent calls
it in-process, as Go tools. An MCP server over the same package, for
consumers outside Go, is a thin wrapper `maestro-cms` can add when one is
wanted; the MVP does not wait for it.

The intake agent's review UI is a web UI embedded in the `cmd/addepar`
binary (`go:embed`) and served on localhost: it must show page images with
highlighted evidence, which rules out a terminal UI, and a separate
application would duplicate the store access the binary already has.

What the intake agent requires of the library, all covered above:

| Intake need | Requirement |
|---|---|
| One run per arriving PDF; folder drop or command line | MODE-4, RUN-1 |
| PDF sent to the model; `get_document` returns the PDF | LLM-2, TOOL-11 |
| PDF text with word positions, page rendering (Poppler, through `maestro-cms`) | Section 7 |
| `submit_extraction` verified in code, with reasons on failure and a resubmission cap | LOOP-2, STM-3 |
| Run complete when every statement is verified, set aside or sent to manual review | STM-4 |
| Approval and export are human actions outside the agent | TOOL-4 (propose and finish) |
| Hash-based duplicate detection; records accumulate until export | STORE-2 |
| Logs carry hashes and IDs, never amounts or text | HOST-2 |
| Extraction stored with model ID, prompt version and transcript; no re-run to explain a value | RUN-6, MAN-4 |
| Several statements at once; token use recorded | LLM-4, HOST-1 |
| Scoring against an answer key | TEST-4 |

The intake agent's review UI, verifier, exporter and decimal handling are
the agent's own code, not library requirements.

## 7. Out of scope, and upstream dependencies

**Out of scope for the core library:** persistence, HTTP serving,
authentication, UI, an MCP server, and knowledge of any particular host.
(Reference stores and the standalone host are in scope, in separate
packages.) Maestro-style agent-to-agent messaging and container sandboxing
are not requirements; they may return if a consumer needs them.

**Upstream work.** The library does not work around these locally.
Each is proposed to the owning team only where it would serve that
library's other consumers too, not this library alone; anything specific
to agents stays here.

| Need | Gap | Requirement |
|---|---|---|
| Image and document content | `maestro-llms`' `ContentPart` has only text, tool call, tool result and reasoning parts | LLM-2 |
| Non-text tool results | `maestro-llms`' `toolloop.ToolResult.Content` is a string | TOOL-11 |
| Forced wrap-up, a turn-boundary stop that still runs sibling calls, a pre-request hook that may fail | Not in `maestro-llms`' `toolloop`; Maestro's wishlist items 1 and 2 | LOOP-1, LOOP-3 |
| An MCP client that turns an MCP server's tools into `toolloop` tools | None. `maestro-llms` ADR 0011 excludes MCP adapters from `toolloop` itself, so this is a proposal for a separate leaf package with its own ADR there (the official Go MCP SDK is the likely dependency) | TOOL-6 |
| PDF word positions and page rendering | `maestro-cms`' `pdftotext` engine returns plain text per page | Section 6 |

## 8. Lessons from buggrit and maestro

What is adopted is decided per piece in an ADR when the work starts
(CLAUDE.md). This section records the survey so those ADRs start from it.

**Adopt from buggrit:**

- The single loop wrapper: investigate, nudge once, forced wrap-up, outcomes
  with reasons (`internal/review/stage.go`).
- Validated submissions with strict decoding and all-at-once errors
  (`findings.go`).
- Tool middleware by wrapping execution, and the visible clock
  (`clock.go`).
- Telemetry: nil-safe recorder, requests stored as deltas, a `Sink`
  interface with idempotent writes and honest completeness
  (`internal/telemetry`).
- Prompt and tool content hashing (`identity.go`).
- The head fence before each side effect (`internal/github/fence.go`).
- The replay and benchmark harness (`internal/bench`).
- The engine takes interfaces; env vars and front-end specifics stay in
  `cmd/`.
- A strictly decoded config file with a `config validate` command.

**Avoid from buggrit:** the agent reading its own API key
(`internal/llm/llm.go`); a fixed stage set; configured but unenforced
budgets; durable state in side channels instead of explicit checkpoints;
results passed through closure side effects.

**Adopt from maestro:** the state machine engine (Maestro v2 retains it)
with declared transitions; tools that cause transitions; the v2 execution
contract's binding items (ADR 0032) as the target for results, events,
cancellation and the wire mode; the v2 action-family registry (effect
site, commit point, reconcile probe, attempt ID as idempotency key); one
mandatory policy boundary (ADR 0030); per-tool circuit breaking;
content-digested prompts resolved once; `already_satisfied` as an explicit
outcome; reported and observed facts kept distinct.

**Avoid from maestro (v1):** global mutable registries and config
singletons; tools holding a back-reference to the agent; untyped state
data; flattened outcome enums that mix loop, transport and workflow facts;
two channels that can disagree about a result; non-atomic checkpoints;
recovery that does not survive a restart; fire-and-forget audit writes;
credential lookup in the agent factory; drop-oldest compaction;
iteration-only budgets.

**New design work** (neither repository has it): manifests, embedded
skills, approval as a policy outcome, read/write classification,
recorded-response replay of an agent type, the per-instance object store,
and the three-mode wrapping.

## 9. Open questions

- **Q-1 buggrit's telemetry schema.** Should the Postgres store reuse or
  share buggrit's telemetry schema (runs, events stored as request
  deltas, idempotent writes) for HOST-1's events? Likely yes; to be
  confirmed during design. (Sharing a database with `maestro-llms`' bucket
  is settled: STORE-5.)
