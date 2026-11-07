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
required for the *first complete release*, the release that implements
every Must; [MVP.md](MVP.md) says which Musts the MVP (section 6) builds
and which follow it. **Should** is expected but may follow; **Plan for**
means the first complete release need not implement it, but its design
must not rule it out. Every behavior has a default, so
an agent type configures only what it changes.

## 1. Purpose and consumers

`maestro-agents` is a Go foundation for building LLM agents as **state
machines**. An *agent type* is first-party Go code built on the library: a
set of states, the transitions between them, and the tools, skills and
prompts its model-driven states use. Three more terms, and only three:

- An *agent instance* is one serial execution stream of a configured agent
  type, with its own checkpoint sequence and at most one owner at a time
  ([ADR 0002](adr/0002-serial-execution-within-an-agent-instance.md)).
- A *run* is the work an instance does for one trigger, from receiving it
  to a terminal outcome. A run keeps its identity, consumed budgets,
  resolved configuration and action identities for its whole life, however
  many workers it passes through.
- An *activation* is one worker's stretch of a run: from acquiring
  ownership to releasing it, at a step boundary (including a stop to wait
  for input, STM-8), with a terminal outcome, on cancellation or failure
  mid-step (RUN-2, RUN-3, HOST-7), or on losing ownership. Only a step
  boundary guarantees a new checkpoint; an activation that ends mid-step
  leaves the run at its last one. A run may
  take one activation (a daemon) or many (a serverless host).

The library supplies the runtime around the agent type's logic: the state
machine engine, the model loop, tools, skills, budgets, the execution
record, events and the seams to whatever hosts the run.

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
  new state, not the call of a particular tool. A call that causes a
  transition ends its turn at that call, with return-statement semantics
  (`maestro-llms` ADR 0017, `ToolResult.Stop`): earlier sibling calls have
  run; later ones are not executed, because they belonged to the state
  that has just ended, and each is recorded with a "not executed" result
  so the transcript stays well formed. The execution record marks them not
  executed, so an activation continuing from that turn never runs them
  (STM-6). **Departs from** item 1 of Maestro's
  [wishlist for `maestro-llms`](https://github.com/SnapdragonPartners/maestro/blob/main/docs/v2/requirements_maestro-llms-wishlist.md), which asks for a
  stop that still runs later siblings.
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
  turn, a state transition, or a stop to wait for input (STM-8). A wait
  boundary does not complete the step or change its identity; the
  checkpoint sequence still advances there. At every step boundary the instance's state
  (current state, state data, and the transcript of a model state in
  progress) can be checkpointed and later restored, so a run can continue
  from its last boundary in a new activation, in another process. A model
  turn is recorded in the execution record (REC-1), including the tool
  calls it requested, before any of those calls execute.

  A call's *permission* is recorded separately from whether it was sent,
  and takes exactly one of these values, each with one name used
  everywhere:
  - *allowed*: the policy hook answered `allow`;
  - *denied*: the hook answered `deny`;
  - *awaiting approval*: the hook answered `pending` in *suspend and
    resume* mode (TOOL-4), and no decision has been accepted yet;
  - *approved*: an approval decision was accepted for it (STM-8) and its
    digest still matches (TOOL-4);
  - *rejected*: a rejection was accepted for it;
  - *invalidated*: an approval was accepted but the digest no longer
    matches (TOOL-4);
  - *proposed*: the hook answered `pending` in *propose and finish* mode;
    the agent never executes it (TOOL-4).

  Only an *allowed* or *approved* call may be sent. For each call, in this
  order: its permission is durably recorded (REC-2); then, only if it is
  allowed or approved, its attempt (action identity and the fact that it
  is being attempted) is durably recorded immediately before the call is
  sent (REC-2). A call with any other permission never carries an attempt
  marker. So the attempt marker says whether a call was sent, the
  permission says whether it may be, and a call with no recorded attempt
  has not been sent. An activation that continues after
  that point does not ask the model to repeat the turn, and the execution
  record decides what happens to each recorded call:
  - *Not yet attempted, not marked not executed by a transition (STM-3),
    and allowed or approved*: issued. A call with no recorded permission
    first gets one from the policy hook. The cases form a partition; the
    transition marker takes precedence over every other state.
  - *Read call attempted, with no recorded result* (including one
    interrupted mid-flight, RUN-2): issued again. A read has no effect
    to duplicate (TOOL-2).
  - *Write call attempted, with no recorded result* (including one
    interrupted mid-flight): its outcome is *unknown* (TOOL-12). It is
    never retried blindly; its write contract's reconciliation probe, or
    a person, resolves it to one of the next two states.
  - *Known not to have happened* (resolved as above): issued again.
  - *Executed*: never issued again. Its recorded result stands; for a
    write resolved from *unknown*, the result recorded is the one the
    resolution produced (what the probe or person established), so the
    model always receives a result for every call.
  - *Not executed because a transition ended the turn* (STM-3), or with
    permission *denied*, *awaiting approval*, *rejected*, *invalidated*
    or *proposed* (TOOL-3, TOOL-4): not issued; each is handled by its own
    requirement (an awaiting call may become approved when its decision
    is accepted, STM-8).

  A write issued again keeps its original action identity and idempotency
  key (TOOL-5); reads carry no idempotency key.
- **STM-7 Must: the host decides how many steps one activation covers.** A
  serverless host may checkpoint and stop at every boundary; a daemon may
  run indefinitely, checkpointing as it goes. A daemon is therefore not a
  different kind of agent: it is the same state machine with an activation
  long enough to be effectively unbounded. **Departs from** morris#568
  item 1 ("bounded runs, not daemons") in wording, not substance: what
  Morris needs is state restorable at bounded intervals, which STM-6 gives
  every agent.
- **STM-8 Must: two kinds of wait.**
  - *A run waiting for input it needs to continue* (an approval in
    *suspend and resume* mode, or other external input mid-run) has the
    status `waiting` (RUN-3). Waiting is a status, not a state transition:
    the run stays in the state it was in, a model state in progress keeps
    its transcript (STM-6), and the step is unchanged, so the resumed
    action keeps its original action identity and idempotency key (TOOL-4,
    TOOL-5). This is the one definition of the wait's lifecycle; RUN-1,
    TOOL-4 and HOST-4 refer to it. The lifecycle is the same whether a
    daemon waits in-process or a serverless host ends the activation and
    later starts another activation of the same run:
    1. *Enter.* Before waiting, the runtime durably commits, with the
       wait's step boundary (STM-6, REC-2), any pending action the wait is
       for (its call and TOOL-4 digest), the run's `waiting` status and
       a reason naming the input it awaits. The reason lives in the run
       record the host reads (RUN-3, STORE-1), not in the checkpoint's
       opaque contents (RUN-5).
    2. *Deliver.* The host delivers the input through the control plane
       (HOST-4), to the activation waiting in-process or as an input of
       the next activation (RUN-1). Every delivery, on either path,
       carries the input's identity.
    3. *Accept, once.* The runtime accepts an input only if the run is
       `waiting` for it. It durably records the accepted input with its
       identity in the execution record (REC-1), where a recovering
       activation finds it, and moves the run's status out of `waiting`
       (to `runnable`) in one commit, before acting on the input. A delivery
       whose identity was already accepted, or that arrives when the run
       is not waiting for it, is acknowledged and not applied again.
    4. *Continue.* The run continues from the wait's step boundary; the
       committed record never describes a wait that has ended.
  - *An idle instance waiting for its next trigger* has no run in flight.
    This is not a run status: when the trigger arrives, a new run starts
    (one run per trigger, section 1; MODE-4).
- **STM-9 Must: serial execution within an instance.** State processing,
  model turns and tool calls execute one at a time, including sibling tool
  calls from one model response, in their recorded order, unless a
  transition ends the turn before them (STM-3). Only the
  instance's current owner advances its checkpoint sequence. Independent
  instances run concurrently. Heartbeats, cancellation and transport
  handling may run alongside the execution stream but never advance
  workflow state. ([ADR 0002](adr/0002-serial-execution-within-an-agent-instance.md))
- **STM-10 Plan for: subagents.** Parallelism within an agent's work uses
  separately identified child instances, spawned through the host (which
  controls admission and authority), each with its own state, checkpoints
  and owner, with facilities to collect their results and cancel them.
  Post-MVP: the intake agent must not acquire this machinery.

### 3.2 Runs (RUN)

- **RUN-1 Must: inputs and outputs.** An activation takes the run's
  identity, the trigger payload, the resolved configuration, the latest
  checkpoint and, for a run that was `waiting`, the awaited input with its
  identity, accepted once (STM-8). It returns an outcome (RUN-3),
  artifacts and a new checkpoint. The run's identity, consumed budgets,
  resolved configuration and action identities survive every handover
  between activations, so a new worker never resets the work. (morris#568 item 1)
- **RUN-2 Must: cancellation through `context`.** Cancelling an
  activation's context stops it at the next safe point, with the host's
  reason taken from the context's cause (`context.WithCancelCause`). A
  stopped activation leaves a well-formed transcript: a tool call
  interrupted mid-flight is either completed or recorded as interrupted,
  never left dangling (buggrit repairs this itself in `wellFormed`; the
  library owns it). Stopping a worker is not cancelling the work: a
  shutdown ends the activation with the run's actual disposition (RUN-3).
- **RUN-3 Must: a typed outcome per activation**, in two groups. Each
  status carries a reason valid only for that status, so an invalid
  combination cannot be constructed:

  | Group | Status | Reason | Maestro ADR 0032 equivalent |
  |---|---|---|---|
  | Not terminal | `runnable` | more work is ready now; the activation stopped at a boundary (the host's step limit, or a shutdown while working) | none: an incarnation ending without a result |
  | Not terminal | `waiting` | the input the run needs to continue (STM-8): approval of pending actions in *suspend and resume* mode (TOOL-4), or other external input mid-run; never "the next trigger", which starts a new run | blocked |
  | Terminal | `succeeded` | `changed` or `no_change`; a *propose and finish* run (TOOL-4) succeeds with its pending actions in its output | completed, with disposition changed or already_satisfied |
  | Terminal | `failed` | `budget_exhausted` (naming the budget), `budget_unenforceable` (a budget that cannot be enforced or computed: a hard budget with no known-safe input bound, BUD-3, or any budget, estimated included, for which no amount can be computed, BUD-4; a configuration error, so retrying as-is cannot help), `model_error`, `tool_error`, `invalid_output` or `wrap_up_failed` (LOOP-3); each reason states whether retrying as-is can help | failed, with failure class; `timed_out` is `failed` with the active-time budget or the deadline (BUD-2) |
  | Terminal | `cancelled` | the cause the host supplied: superseded or operator | cancelled, with cancel reason |

  A shutdown preserves the disposition: a worker shutting down while the
  run waits for approval reports `waiting`, not `runnable`. An activation
  that loses ownership reports nothing authoritative about the run (RUN-4,
  HOST-6); the run's status is whatever its last owner committed. A daemon
  reports each run's outcome when that run ends, or when the daemon stops
  mid-run. **Departs from** morris#568 item 1, which
  lists budget-exhausted as its own terminal state: it is a failure here,
  and final in the MVP. **Plan for** extending a budget-exhausted run with
  more budget; that must continue the same run, keeping its identity and
  action identities, never start a new run from its checkpoint, which
  would give its recorded write calls new idempotency keys (TOOL-5).
  **Departs from** ADR 0032's four axes, which
  this table maps onto with fewer moving parts.
- **RUN-4 Must: the result is a claim.** The run reports what it believes
  happened; the host records the outcome and may override it with its own
  observations (for example, an activation that reports success after its
  lease was revoked). The outcome carries enough for the host to tell.
  Changing a reported outcome afterwards cannot undo a committed write,
  which is why ownership is enforced at the commit itself (HOST-6).
- **RUN-5 Must: checkpoints are opaque to the host.** The host stores and
  returns them but does not read their contents. A checkpoint carries a
  version so an agent type can refuse or migrate an old one.
- **RUN-6 Must: a run is explainable and replayable from its execution
  record** (REC-1), without calling the model or repeating any external
  read or write. Model responses alone are not enough: tool results,
  policy decisions, approvals and every other input the runtime could not
  reproduce are recorded too.

### 3.3 Agent types and manifests (MAN)

- **MAN-1 Must: a manifest per agent type** declaring name, version,
  states and transitions (STM-1), granted tools, skills, supported trigger
  kinds, the instance config schema and a default budget. (morris#568
  item 2) Each granted tool has a content identity: a digest of its
  definition (name, input and output schemas, description, effect
  classification and write contract, TOOL-2 and TOOL-12) and of the
  version of the code or server that implements it, which changes
  whenever any of them does. TOOL-4 binds approvals to it.
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
  limiter blocks until capacity is free or the activation's context or
  time budget ends; being rate limited never fails a run on its own. A
  request the limiter can never admit (larger than the configured
  capacity, which the bucket reports as permanent) fails at once instead
  of waiting.

### 3.5 Tools (TOOL)

- **TOOL-1 Must: one tool registry, exposure per consumer.** Tools have
  typed JSON Schema input and output. Each consumer (an agent type, a state
  within it, or a host's chat loop) sees only the tools its grant names. An
  agent type may register dedicated tools only it can use. (morris#568
  item 6)
- **TOOL-2 Must: effect classification.** Every tool is classified `read`
  or `write`, and every write declares its write contract (TOOL-12).
  **Plan for** Maestro's fuller classification of where the effect happens
  (host side, inside the agent's resource, external) (Maestro v2
  `boundary/family`).
- **TOOL-3 Must: one mandatory policy boundary.** Every tool call,
  including an agent type's dedicated tools and MCP tools, passes through a
  host-supplied policy hook before its effect. The hook answers `allow`,
  `deny` or `pending` (approval required). A denial is returned to the
  model as data and the run continues. The boundary is structural: there is
  no path to a tool's effect that skips it (ADR 0030 §1). The default hook
  allows everything.
- **TOOL-4 Must: approval through policy, not per-tool modes.** When the
  hook answers `pending`, the runtime records a pending action, bound to a
  digest of the exact tool name and arguments together with the content
  identity of what would execute it (the agent type's version and the
  tool's content identity, both from MAN-1), and does not execute it.
  The agent type declares, per tool or per state, which of two things
  follows:
  - *Propose and finish.* The pending action is part of the run's output,
    and the run can succeed (RUN-3); approving and carrying it out happen
    outside the agent, which never sees the outcome. (The intake agent
    works this way: approval and export are human actions in its app.)
  - *Suspend and resume.* The run checkpoints and the activation ends with
    the run `waiting`, or a daemon waits in-process. When the decision is
    accepted (STM-8, on either delivery path), the runtime executes the
    approved action, with its original action identity and idempotency
    key, only if the digest still matches, and returns the result to the
    model. The digest is recomputed on resume from the recorded call and
    the agent type and tool now in force. If it no longer matches (the
    agent type or the tool changed while the run waited), the pending
    action is invalidated and recorded as such, never executed, and the
    model receives a denial as data, as for any policy denial (TOOL-3), so
    the run continues rather than stalling. An accepted rejection is
    handled the same way: the call is recorded as rejected, never
    executed, and the model receives a denial as data.

  A host that can get a decision quickly (a chat user) may simply block
  inside the hook and return `allow` or `deny`; the library needs nothing
  more for that, but the time spent blocked counts as active time (BUD-2).
  A host that wants the wait excluded returns `pending` instead, and the
  runtime waits for the decision through the control plane (HOST-4). **Departs from** morris#568 item 6, which gives write tools
  their own propose mode: approval is a policy outcome here, so no tool
  implements it.
- **TOOL-5 Must: idempotency keys.** Every write call carries an
  idempotency key that is distinct for each logical tool invocation and
  stable across redelivery, so a retried run does not repeat a side effect
  and two writes in one model turn (two `submit_extraction` calls for
  different statements, say) never share a key. The runtime derives it
  from the run's identity, which survives activations (RUN-1), the step,
  and the call's identity within that step (for example, its position in
  the recorded turn), never from the step alone. Write calls execute only
  from a model turn already recorded (STM-6), so a continued run issues
  the same recorded calls with the same keys rather than asking the model
  again, and only the calls STM-6 says may be issued.
  Reusing an action identity with different arguments is rejected.
  Idempotency prevents repeating the same action; it does not stop a stale
  worker from making a different one, which is fencing's job (HOST-6).
  **Departs from** morris#568 item 9, which asks for one key per (run,
  step): a step can hold several writes.
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
  the run (or chat turn, LOOP-4) and its principal. The tool asks the host for whatever credential
  that principal needs when it executes. Where a tool executes, in-process
  or forwarded, is the host's choice. (morris#568 item 4)
- **TOOL-9 Must: result redaction.** The host has a hook to redact tool
  results before the model sees them.
- **TOOL-10 Must: bounded output.** Tool output is capped, and a truncated
  result tells the model how to get the rest (buggrit `tools.go`).
- **TOOL-11 Must: non-text results.** A tool can return document or image
  content, not only text (section 7). The intake agent's `get_document`
  returns the PDF itself for visual reading.
- **TOOL-12 Must: an enforceable write contract.** Every write tool states
  how it prevents a duplicate effect: idempotency supported by the
  destination, an atomic local transaction, or a reconciliation probe that
  can find out whether the effect happened (Maestro v2's `Reconcile`). A
  write that may have committed before its acknowledgement was lost is
  never blindly repeated: its outcome is recorded as *unknown*, distinct
  from *known not to have happened*, and stays so until the probe or a
  person resolves it. A write tool that can offer none of the three is
  refused registration unless the agent type explicitly accepts the risk.

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
- **LOOP-3 Must: explicit completion policies.** One loop engine serves
  every consumer, and each use declares what completes it. A model state's
  policy requires a transition: after a text-only or empty answer the loop
  nudges the model once, and when the state's budget nears its end the
  loop forces the tools that leave the state, letting the agent type relax
  validation for that wrap-up turn (buggrit `stage.go`; Maestro issue
  #317, where an architect deadlocked because the model never called its
  exit tool). Forced wrap-up has a bounded number of attempts; if the
  provider or model cannot comply, the run fails with `wrap_up_failed`
  rather than looping. A chat policy completes on a valid text answer and
  never nudges.
- **LOOP-4 Must: usable by a host's chat loop.** The same registry, policy
  layer and loop serve a host's chat, so a host has one tool path, not
  two. (morris#568 item 7) A chat turn has no state machine or checkpoint,
  but it has a lightweight identity the host supplies (a conversation and
  turn ID) that stands in for the run wherever a contract needs one: the
  run context (TOOL-8), action identities and idempotency keys (TOOL-5),
  event identity (HOST-1) and the execution record (REC-1).
- **LOOP-5 Must: summarization is available, off by default.** An agent
  type can enable `maestro-llms`' compaction per model state. Compaction
  never separates a tool call from its result (maestro-llms ADR 0030, as
  amended by ADR 0038), its model calls count against the run's budgets
  (BUD-1), and the execution record and event stream record each
  compaction.

### 3.8 Budgets (BUD)

- **BUD-1 Must: budgets are totals per run**: tokens, model turns, active
  time, an elapsed-time deadline and, when the host supplies prices, cost.
  Defaults come from the manifest; the host can override them per run.
  Consumption is part of the run's state and carries across activations
  (RUN-1). Zero means no limit. Rate limits are not budgets (LLM-4).
  Budgets come in two kinds, and only two:
  - *Hard* (the default): enforced so consumption never exceeds the
    budget. Token and cost budgets are enforced by admission before each
    provider attempt (BUD-3); the turn budget by counting turns before each
    one; the time budgets by cancelling the activation's context (BUD-2).
    A configured hard budget
    that is not enforced is a defect (buggrit's `MaxTokensPerPass` is the
    counterexample).
  - *Estimated*: the single exception, chosen explicitly by the host for
    a token or cost budget when no safe input bound exists (BUD-3). It is
    admitted on the estimate, reported as estimated wherever it appears,
    and may overshoot by at most one attempt's error.

  Exhausting either kind fails the run with `budget_exhausted` (RUN-3),
  naming the budget: a token, cost or turn budget when the next attempt or
  turn cannot be admitted, an estimated budget when observed consumption
  reaches it, active time when its counted time runs out, the elapsed
  deadline when it passes (BUD-2).
- **BUD-2 Must: two clocks.** *Active time*, the default time budget,
  counts while a worker is executing the run, including model calls, tool
  execution and limiter waits. It excludes every wait for external input
  that the runtime itself performs (STM-8), whether the activation ends
  (serverless) or waits in-process for the host to deliver the input
  (a daemon, HOST-4). A wait held inside host code the runtime merely
  calls (a host blocking inside the policy hook, TOOL-4, or in a tool or
  credential provider) is invisible to the runtime and counts as active
  time; such code receives the activation's context, so both clocks still
  cut it off. A separate optional *deadline* bounds elapsed calendar time
  across waits. Active time keeps a reserve for wrap-up, and the model is
  shown the clock (buggrit design §9: slow reasoning models spent whole
  budgets on single turns). Both are enforced by cancelling the
  activation's context, not by admission, through two separate
  mechanisms, so a slow model call, tool call or limiter wait is cut off
  rather than overshooting (tool calls receive that context):
  - the elapsed-time deadline is a fixed context deadline, which keeps
    running through every wait;
  - active time is a timer the runtime pauses on entering an in-process
    wait for input and re-arms with the remaining active time on leaving
    it.

  When the elapsed-time deadline passes during an in-process wait, the
  worker fails the run with `budget_exhausted` naming the deadline; a run that is `waiting` between activations has no worker, so
  the host does the same; and an activation that starts after the
  deadline fails the run before doing any work.
- **BUD-3 Must: hard budgets are enforced by admission before each
  provider attempt**, not only by observing usage afterwards. Before every
  attempt, including each retry, the runtime reserves the attempt's worst
  case (a known-safe upper bound on the complete request's input,
  including document and image content and provider overhead, plus its
  output cap, with the output cap lowered to what the budget leaves) and
  refuses an attempt that cannot fit. An estimate is not a bound, even one
  biased upward (`maestro-llms`' middleware estimator is explicitly
  approximate): with 1,000 tokens left, an input estimated at 200 permits
  800 output tokens, and an input that actually costs 400 overshoots the
  budget before usage is reconciled. Where the bound comes from (a
  provider's token-counting endpoint, a conservative bound valid for the
  model's tokenizer, a provider's documented formula for images and
  pages) is a design decision. When no adequate bound is available, a hard
  budget fails closed, refusing the attempt and failing the run with
  `budget_unenforceable` (RUN-3), not `budget_exhausted`, since nothing
  was consumed; the host's alternative is
  to configure that budget as *estimated* (BUD-1). After
  the attempt, observed usage replaces the reservation. A failed attempt
  whose consumption is unknown is charged its full reservation.
  Compaction and wrap-up calls are admitted the same way. Because
  `maestro-llms` retries inside the injected client, the injected-client
  contract must give the runtime a hook at each attempt (section 7).
- **BUD-4 Must: the library does not price, and unknowns stay explicit.**
  It reports tokens per model; a cost budget uses a pricing function the
  host supplies. Unknown usage or an unpriced model is an explicit unknown,
  never zero. An attempt whose consumption is unknown is charged the
  amount admitted for it: the worst-case reservation for a hard budget,
  the estimate for an estimated one (BUD-1, BUD-3). A budget for which no
  amount at all can be computed (a cost budget with no price for the
  model, say) fails closed with `budget_unenforceable` (RUN-3) rather than
  silently becoming unenforceable.

### 3.9 Host seams (HOST)

The host implements these; the library calls them. (morris#568 item 8)

- **HOST-1 Must: telemetry events.** State transitions, model turns, tool
  calls, policy decisions, usage and compactions are emitted as events for
  observation. Events have a stable identity (run, sequence, unbroken
  across activations) so a host can deduplicate redelivered events. A nil
  sink records nothing. Telemetry is optional and may be redacted,
  disabled or delivered with weaker guarantees; it is never the recovery
  record (REC).
- **HOST-2 Must: redaction of telemetry and logs.** The host can classify
  or redact event content before it leaves the run, so telemetry can carry
  hashes and IDs instead of document text or amounts (the intake agent's
  confidentiality requirement, its N-5). The same rule covers logs: the
  library logs through a logger the host injects (`log/slog`), never
  writes tool arguments, tool results or model content to a log itself,
  and gives agent types and tools the same injected logger, so one host
  policy governs every log line.
- **HOST-3 Must: stores**, defined in section 4: checkpoints, artifacts,
  run records, execution records, instance ownership, queued triggers,
  and an object store per namespace the host assigns, which several
  instances may share (STORE-2).
- **HOST-4 Must: control plane** for lease, heartbeat, cancellation, and
  delivering the input a `waiting` run needs (an approval decision, other
  external input), either to an activation waiting in-process or as the
  input of the next activation, each delivery carrying the input's
  identity; delivery may be retried, and the runtime accepts each input
  once (STM-8). Liveness comes from heartbeats,
  not from output silence.
- **HOST-5 Must: credential provider** (TOOL-8) and **policy hook**
  (TOOL-3), both keyed by the opaque run context.
- **HOST-6 Must: ownership is enforced at the durable commit.** A
  separate check before the effect is not enough: a worker can pass it,
  lose ownership, and then commit. So ownership is validated together with
  every durable write an activation makes, with no exceptions: tool
  effects, checkpoint publication, execution-record appends, run-record
  changes (status and reason, accepted inputs, STM-8) and any other write
  a later requirement adds. A write that skips this check is a defect. For a database effect, the
  ownership check and the effect are serialized against ownership
  transfer, for example by locking the ownership row or comparing an owner
  epoch within the same transaction at an isolation level that actually
  serializes them; placing both statements in one transaction is not by
  itself enough. A checkpoint is published only by compare-and-set on
  (instance, owner, sequence). For an external effect, the destination or
  the host must provide an equivalent guarantee, such as accepting a
  fencing token; where none can be established, the write declares it and
  the outcome reports the uncertainty explicitly. Fencing and idempotency
  (TOOL-5) are separate guarantees and both are required (buggrit
  `fence.go`; ADR 0032 binding item 9).
- **HOST-7 Should: cancellation order.** A forced end closes admission of
  new tool calls, lets admitted calls finish or reach their deadline, and
  only then emits the result (ADR 0032 binding item 8).

### 3.10 Delivery (DEL)

- **DEL-1 Must: at-least-once runs.** A run may be delivered more than
  once. Idempotency keys (TOOL-5), the write contract (TOOL-12), fencing
  (HOST-6) and conditional writes to the object store (STORE-2) keep a
  retried run from repeating a side effect.
- **DEL-2 Should: telemetry delivery guarantees by kind.** Usage events
  are delivered at least once; heartbeat and progress are best effort.
  Incomplete telemetry delivery is reported, not hidden (buggrit's
  telemetry writer marks a run complete only if every event was written).

### 3.11 The execution record (REC)

- **REC-1 Must: a protected execution record per run**, separate from
  telemetry. It contains, or references by content identity, everything
  needed to recover and to explain the run: the trigger, the resolved
  configuration, the content of the manifest, prompts, skills and tool
  definitions in force, every model request and response, every tool call
  and result, each call's permission and attempt marker (STM-6, REC-2),
  policy decisions,
  pending actions and approvals, action identities with their
  outcomes (including *unknown*, TOOL-12), budget consumption, and any
  other input the runtime cannot reproduce. Content identities point at
  content that is retained and retrievable, not just at hashes.
- **REC-2 Must: write-ahead.** A required step boundary, each tool call's
  permission (STM-6), each pending action with its TOOL-4 digest, and each
  tool call's attempt (STM-6) are durably recorded before any effect or
  wait that depends on them. If recording fails, dependent effects do not proceed. (buggrit's telemetry writer deliberately drops
  events under failure; that delivery behavior is acceptable for
  telemetry and never for this record.)
- **REC-3 Must: protected, not redacted.** Telemetry redaction (HOST-2)
  does not apply to the execution record, which keeps the content recovery
  needs. The host protects it as sensitive data (access control,
  encryption at rest, retention policy).
- **REC-4 Must: replay consumes the record.** Replaying or reconstructing
  a run uses the recorded observations instead of repeating model calls or
  external reads and writes (TEST-1).

### 3.12 Testability (TEST)

- **TEST-1 Must: replay harness.** Drive an agent type from an execution
  record (model responses, tool results, policy decisions) against fake
  host seams, deterministically and without network. (morris#568 item 10)
- **TEST-5 Must: crash-recovery tests.** The harness can stop an
  activation at any durable boundary (after an effect commits but before
  its acknowledgement or the next checkpoint is recorded, for instance),
  continue the run in a new activation, and assert that no effect is
  duplicated and no work is lost. This tests recovery and idempotency, not
  fencing, because the stopped worker never returns.
- **TEST-6 Must: stale-owner tests**, at two pause points. The property
  checked in both is the same: no durable write HOST-6 fences (effect,
  checkpoint, execution-record append, run-record change such as setting
  `waiting` or accepting an input) lands for A after B's ownership
  transfer has committed.
  - *Before the commit*: A is paused before it starts the commit; the
    harness transfers ownership to B and lets B commit, then resumes A,
    and asserts that none of A's fenced writes land. This fails if
    ownership is not checked at commit time at all.
  - *Inside the commit*: A is paused after it has read the ownership
    record and before its write, while B attempts the transfer. Either
    serialization order is correct and the test accepts both: B's transfer
    waits until A's commit finishes (as with the row lock HOST-6 permits),
    so A's write lands while A is still the owner and B's transfer follows
    it; or B's transfer commits first and A's write is rejected. The test
    fails only if A's write lands after B's transfer has committed, which
    detects a check and write not serialized against the transfer (HOST-6)
    and which the first pause point cannot.

  Both pause points run against every durable reference store (the file
  and Postgres stores, STORE-4) and against any host store that claims to
  meet HOST-6, through a test-only pause hook each store adapter exposes
  at those two points. Fakes and the in-memory store serialize trivially,
  so passing against them is not evidence of fencing.
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
  instance, status and reason, trigger, consumed budgets, timestamps.
  *Instance ownership*: current owner and epoch, the subject of HOST-6's
  commit-time check. *Checkpoints*: per instance, sequenced, opaque.
  *Execution records* (REC). *Telemetry events*: run, sequence, kind,
  payload. *Artifacts*: content-addressed blobs with metadata. *Queued
  triggers*: per instance, in arrival order, for triggers that arrive while
  a run is in flight (MODE-4). Its run record is created conditionally on
  the queued trigger's identity (compare-and-set, as in STORE-2), so a
  retried creation finds the existing run instead of making a second one,
  and the trigger is removed only once that record exists; a restart
  neither loses it nor gives it a second run.
- **STORE-2 Must: an object store per namespace.** Keys map to JSON or
  bytes, in a namespace the host assigns. Instances of the same configured
  agent may share a namespace (several documents processed at once are
  several instances, ADR 0002); an instance cannot read a namespace it was
  not given. Every write is conditional on a version number
  (compare-and-set), because redelivered runs and concurrent instances are
  normal (DEL-1). Keys can be listed by prefix. That covers the intake
  agent's needs: "has this file hash been seen?" is a lookup, and "records
  awaiting review" is a prefix listing.
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
  schedule or a new file in a folder, idle between them, and start a new
  run on each, per STM-7 and STM-8). A trigger that arrives while the
  instance has a run in flight is never dropped while the process lives:
  the host either queues it for that instance, starting its run when the
  instance is idle, or starts it on a new instance (STM-9). With a durable
  store (file or Postgres) the standalone host queues by default, in the
  queued-trigger record (STORE-1), so the trigger also survives a restart.
  With the in-memory store (STORE-4) a queued trigger is lost on restart;
  that store is for daemons whose state need not outlive the process, and
  choosing it accepts that loss.
- **MODE-5 Must: credentials stay outside agent code.** The standalone
  host implements the credential provider from environment variables or a
  secrets file. Only the host reads them; agent types and tools still go
  through TOOL-8.
- **MODE-6 Must: graceful shutdown.** SIGINT and SIGTERM stop activations
  through their contexts (cause: shutdown) at the next step boundary,
  reporting each run's actual disposition (`runnable` or `waiting`, RUN-3),
  with exit codes distinguishing complete, incomplete and usage errors
  (buggrit's `0/1/2`).
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
MVP because it has users waiting for it and exercises the hard parts of the
library (recovery, idempotent writes, multimodal input) on a small surface.
It is not free of outside dependencies: it needs four upstream changes
(listed under *Delivery plan* below). It lives in this repository as `cmd/addepar`, both as a usable tool and as
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
highlighting evidence in the review UI) and page rendering (section 7). The
intake agent calls
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
| PDF text with word positions, page rendering (Poppler, through `maestro-cms`) | Upstream: [maestro-cms#25](https://github.com/SnapdragonPartners/maestro-cms/issues/25) (section 7) |
| `submit_extraction` verified in code, with reasons on failure and a resubmission cap | LOOP-2, STM-3 |
| Run complete when every statement is verified, set aside or sent to manual review | STM-4 |
| Approval and export are human actions outside the agent | TOOL-4 (propose and finish) |
| A submitted record is never stored twice, even after a crash | TOOL-5, TOOL-12, HOST-6, REC-2 |
| Hash-based duplicate detection; records accumulate until export | STORE-2 |
| Logs and telemetry carry hashes and IDs, never amounts or text | HOST-2 |
| Extraction stored with model ID, prompt version and transcript; no re-run to explain a value | REC-1, RUN-6, MAN-4 |
| Several statements at once | Several instances sharing a namespace (STM-9, STORE-2) |
| Token use recorded per statement | REC-1, HOST-1 |
| Scoring against an answer key | TEST-4 |

The intake agent's review UI, verifier, exporter and decimal handling are
the agent's own code, not library requirements.

### Delivery plan

The MVP's scope and order of work are in [MVP.md](MVP.md); where the
two differ, it governs.

**Prerequisites from other repositories** (section 7, each filed as an
issue): image and document content and non-text tool results in
`maestro-llms`; attempt-level admission and a safe input bound in
`maestro-llms`; word positions and page rendering in `maestro-cms`. These
gate LLM-2, TOOL-11, BUD-3 and the review UI's evidence highlighting.

**Prove the difficult vertical slice first**, before the Postgres store
and the review UI. The slice uses the file store (STORE-4): a crash test
needs a durable store, and the file store needs no database:

1. Read one representative statement through its text layer in
   `pdftotext`'s layout mode, which keeps each label on the same line as
   its numbers (the verification checks need that pairing, not word
   positions); `maestro-cms` adds the option (maestro-cms#25).
2. Extract a record, verify it in code, and commit the submission durably.
3. Interrupt the activation after the commit but before its
   acknowledgement and checkpoint are recorded (TEST-5).
4. Continue the run in a new activation and show the submission is not
   duplicated and no work is lost.
5. Separately, run both TEST-6 interleavings against the submission,
   using the file store's pause hook
   (activation A paused before its commit, and inside it after reading
   ownership, while activation B takes over), accepting either valid
   serialization order, and show that none of A's fenced writes
   (submission, checkpoint, execution-record append, run-record change)
   lands after B's transfer has committed. Steps 3 and 4 never
   bring a stale worker back, so they cannot show fencing; step 5 does,
   including the serialization HOST-6 requires.

The slice proves the state machine, execution record, write contract,
recovery (steps 3 and 4) and fencing (step 5) while the upstream multimodal work proceeds. Visual
extraction (the PDF sent to the model) and evidence highlighting then
need their own acceptance evidence once their prerequisites land; passing
the text-layer slice does not stand in for it. The Postgres store, which
must pass TEST-5 and TEST-6 in its turn, and the review UI follow. MCP tools, the wire protocol and
subagents remain later work.

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

| Need | Gap | Requirement | Issue |
|---|---|---|---|
| Image and document content | `maestro-llms`' `ContentPart` has only text, tool call, tool result and reasoning parts | LLM-2 | [maestro-llms#147](https://github.com/SnapdragonPartners/maestro-llms/issues/147) |
| Non-text tool results | `maestro-llms`' `ToolResult.Content` is a string, in `llms` and in `toolloop` | TOOL-11 | [maestro-llms#148](https://github.com/SnapdragonPartners/maestro-llms/issues/148) |
| Budget admission before each provider attempt, including retries, and a known-safe input bound | Retries run inside the injected client, and `maestro-llms` ADR 0031 gave attempts an observer, which cannot refuse one, rather than a middleware slot; the existing estimator is approximate, not a bound | BUD-3 | [maestro-llms#149](https://github.com/SnapdragonPartners/maestro-llms/issues/149) |
| An MCP client that turns an MCP server's tools into `toolloop` tools | None. `maestro-llms` ADR 0011 excludes MCP adapters from `toolloop` itself, so this is a proposal for a separate leaf package with its own ADR there (the official Go MCP SDK is the likely dependency) | TOOL-6 | [maestro-llms#150](https://github.com/SnapdragonPartners/maestro-llms/issues/150) |
| PDF word positions and page rendering | `maestro-cms`' `pdftotext` engine returns plain text per page | The intake review UI's evidence highlighting (section 6) | [maestro-cms#25](https://github.com/SnapdragonPartners/maestro-cms/issues/25) |

**Not upstream work.** Three loop needs that an earlier draft listed here
are met without changes to `maestro-llms`: ending a turn at a
transition-causing call uses the existing `ToolResult.Stop`
(`maestro-llms` ADR 0017), whose return-statement semantics STM-3 adopts
(later siblings are not executed); forced wrap-up (LOOP-3) is built here, as buggrit does, by running
the loop again with the exit tools forced; and recording a model turn
before its tool calls execute (STM-6, REC-2) needs no pre-request hook,
because a fallible decorator around the injected client records each
response before the loop sees it. `toolloop` already executes sibling
calls sequentially, as ADR 0002 requires.

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
  (`internal/telemetry`). For telemetry only: its writer drops events
  under failure by design, so it cannot serve as the execution record
  (REC-2).
- Prompt and tool content hashing (`identity.go`).
- The head fence before each side effect (`internal/github/fence.go`).
- The replay and benchmark harness (`internal/bench`).
- The engine takes interfaces; env vars and front-end specifics stay in
  `cmd/`.
- A strictly decoded config file with a `config validate` command.

**Avoid from buggrit:** the agent reading its own API key
(`internal/llm/llm.go`); a fixed stage set; configured but unenforced
budgets; durable state in side channels instead of explicit checkpoints;
results passed through closure side effects; a fence checked before the
effect rather than with it (HOST-6).

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
skills, approval as a policy outcome, read/write classification and the
write contract, the execution record and replay from it, per-attempt
budget admission, the object store, and the three-mode wrapping.

## 9. Open questions

- **Q-1 buggrit's telemetry schema.** Should the Postgres store reuse or
  share buggrit's telemetry schema (runs, events stored as request
  deltas, idempotent writes) for HOST-1's telemetry events? Likely yes; to
  be confirmed during design. It does not serve as the execution record
  (REC-2). (Sharing a database with `maestro-llms`' bucket is settled:
  STORE-5.)
