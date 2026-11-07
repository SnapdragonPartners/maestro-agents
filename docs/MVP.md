# MVP scope

Status: **Plan** (2026-11-07). This is a working plan, not a decision
record: it changes as the MVP is built and is retired when the MVP ships.
Lasting decisions it leads to go in ADRs.

## What the MVP is

The MVP is one standalone executable, `cmd/addepar`, that performs the
data extraction, transformation and output tasks of the *Capital Statement
Intake* requirements (kept outside this repository) for their MVP phase:
one PDF statement in; a verified valuation record, its evidence, an
operator's review and approval, and an Addepar CSV row with its evidence
manifest out. That document's own later phases (registry mapping, history,
the Addepar API) are not part of it.

The library is built as far as that executable needs, against the full
[requirements](requirements.md). Nothing is cut from the requirements;
what the MVP does not need is **deferred**, not dropped.

## How deferral works

Every requirement falls in one of three classes:

- **Build.** Implemented and tested in the MVP.
- **Contract.** The types and interfaces are defined in the MVP so the
  deferred work fills them in without changing them. Where code must exist
  for the contract to compile or be honest, it is a **skeleton**: a real
  declaration whose unimplemented path returns a clear "not implemented"
  error or refuses at registration. Hosts and agent types can see the
  shape; nothing pretends to work.
- **Deferred.** No code in the MVP. The design must not rule it out, and
  each design ADR says how it leaves room.

**No disposable code.** The MVP never contains code that simulates a
deferred capability (a fake approval flow, a stand-in Postgres, a mock
fencing test) only to be thrown away. A simpler real mechanism that fully
meets a requirement in the MVP's setting is not a simulation: the file
store's exclusive process lock really does guarantee one owner per
instance (HOST-6) while one process uses the store.

**Upstream work is assumed.** `maestro-llms`
[#147](https://github.com/SnapdragonPartners/maestro-llms/issues/147),
[#148](https://github.com/SnapdragonPartners/maestro-llms/issues/148) and
[#149](https://github.com/SnapdragonPartners/maestro-llms/issues/149) and
`maestro-cms` [#25](https://github.com/SnapdragonPartners/maestro-cms/issues/25)
land during the MVP; the plan sequences around them rather than working
around them. `maestro-llms`
[#150](https://github.com/SnapdragonPartners/maestro-llms/issues/150) (the
MCP client) is not needed by the MVP.

## The MVP's setting

These facts about the MVP's deployment are what let parts of the library
wait:

- **One process per store.** The standalone host takes an exclusive lock
  on its store directory, so no second process shares it. Several
  instances (several statements at once, N-6) run concurrently inside
  that one process.
- **No ownership transfer while an activation is live.** The host gives
  an instance to a new activation only after the previous one has
  returned, so a stale owner cannot exist. This is what defers TEST-6, not
  HOST-6: every durable write still carries and checks the owner epoch.
- **No waits for input.** The intake agent's approval is an action in its
  review UI on the stored record, after the run has succeeded. The agent's
  tools never produce a pending action, so the policy hook's `pending`
  answer, both TOOL-4 modes and the `waiting` status are not exercised.
  (The requirements' section 6 maps this to *propose and finish*; that is
  the right concept, but the intake agent needs no runtime support for
  it.)
- **Its only writes are local.** `submit_extraction`, `set_aside` and
  `request_manual_review` write to the agent's own store as atomic local
  transactions (TOOL-12), so a write's outcome is never *unknown*.

## Requirement by requirement

### State machine, runs and manifests

| ID | Class | MVP |
|---|---|---|
| STM-1, 2, 4, 5 | Build | |
| STM-3 | Build | Transitions from tool results, later siblings marked not executed |
| STM-6 | Build | Write-ahead turns, permissions and attempt markers; recovery cases for not attempted, read reissue, executed, not executed, denied. **Contract:** all seven permission states and the *unknown* outcome exist in the types; *unknown* is recorded if it ever arises and the run fails with `tool_error`, and its resolution by probe or person is deferred |
| STM-7 | Build | One-shot and long-running activations |
| STM-8 | Contract | `waiting` exists in the status type; nothing enters it |
| STM-9 | Build | |
| STM-10 | Deferred | Subagents |
| RUN-1 | Build | Without the awaited input (STM-8) |
| RUN-2, 4, 5, 6 | Build | |
| RUN-3 | Build | Every status and reason in the type; `waiting` is never produced. Extending a budget-exhausted run is deferred |
| MAN-1 to 4 | Build | |

### Model access, tools, skills and the loop

| ID | Class | MVP |
|---|---|---|
| LLM-1, 3 | Build | |
| LLM-2 | Build | Once `maestro-llms#147` lands |
| LLM-4 | Build | In-memory bucket; the Postgres bucket comes with the Postgres store |
| TOOL-1 | Build | Exposure to agent types and states; exposure to a chat loop comes with LOOP-4 |
| TOOL-2 | Build | `read` and `write`; Maestro's fuller classification is deferred |
| TOOL-3 | Build | `allow` and `deny`; default hook allows |
| TOOL-4 | Contract | The hook can answer `pending` and the manifest can declare a mode; a type declaring either mode is refused at registration, and a `pending` answer for a type with none fails the run as a configuration error, until approval is built |
| TOOL-5 | Build | |
| TOOL-6 | Deferred | MCP tools; the registry must not assume tools are in-process |
| TOOL-7 | Build | Policy, idempotency, redaction, the visible clock and output caps; per-tool circuit breaking is deferred |
| TOOL-8 | Build | Run context and credential provider; the intake tools need no credentials |
| TOOL-9, 10 | Build | |
| TOOL-11 | Build | Once `maestro-llms#148` lands |
| TOOL-12 | Build | Declarations, registration refusal and the atomic-local-transaction contract. **Contract:** the reconciliation-probe interface, with no MVP implementer |
| SKL-1, 2, 4 | Build | |
| SKL-3 | Build | Compiled-in source; other sources deferred |
| LOOP-1, 2 | Build | |
| LOOP-3 | Build | The model-state policy; the chat policy comes with LOOP-4 |
| LOOP-4 | Deferred | Host chat loops. The identity used for run context, action identities, events and the execution record must accept a chat turn's identity later |
| LOOP-5 | Deferred | Summarization (off by default, unused by the intake agent) |

### Budgets

| ID | Class | MVP |
|---|---|---|
| BUD-1 | Build | Tokens, turns, active time and the deadline, hard and estimated. Cost budgets are deferred (**contract:** the pricing-function interface) |
| BUD-2 | Build | Both clocks and the visible clock. The active-time pause is not needed while there are no in-process waits |
| BUD-3 | Build | Once `maestro-llms#149` lands; until then the intake configuration uses estimated budgets |
| BUD-4 | Build | |

### Host seams, delivery, execution record and tests

| ID | Class | MVP |
|---|---|---|
| HOST-1, 2, 3, 5 | Build | |
| HOST-4 | Build | Cancellation, lease and heartbeat as a single-process host provides them. Delivery of input to a `waiting` run is deferred with STM-8 |
| HOST-6 | Build | Owner epoch checked with every durable write; the file store serializes check and write under its lock |
| HOST-7 | Build | |
| DEL-1 | Build | |
| DEL-2 | Deferred | Telemetry delivery guarantees by kind |
| REC-1 to 4 | Build | |
| TEST-1, 2, 3, 4, 5 | Build | TEST-5 against the file store |
| TEST-6 | Deferred | Until a store allows transfer between live workers (Postgres, or a file store shared by processes); runs against each such store then |

### Storage and run modes

| ID | Class | MVP |
|---|---|---|
| STORE-1, 2, 3 | Build | |
| STORE-4 | Build | Memory and file stores; Postgres deferred |
| STORE-5 | Deferred | With the Postgres store |
| MODE-1 | Build | The standalone host is itself a compiled-mode host |
| MODE-2, 3, 5, 6 | Build | |
| MODE-4 | Build | One-shot and folder-watch triggers, with queued triggers; schedule triggers deferred |
| MODE-7 | Build | For the telemetry sink; the shared limiter comes with Postgres |
| MODE-8 to 11 | Deferred | Wire protocol |

## The intake agent's own requirements

All of the intake requirements' MVP rows are in scope: I-1 to I-3, E-1 to
E-6, V-1 with the seven verification checks, R-1 to R-4, X-1, X-2 and N-1
to N-7. Two depart from that document:

- **N-8 and build step 2 (tools over MCP).** The agent's four tools are
  in-process Go tools. Morris runs the agent compiled in (morris#569), so
  it does not need MCP to reach them; exposing them over MCP is deferred
  (TOOL-6's reverse direction).
- **Prototype language.** Go, not Python (requirements section 6).

**Open:** N-5 asks for data at rest to be encrypted. Whether the file
store encrypts or the MVP relies on the operating system's disk
encryption is decided in the storage design.

## Order of work

1. **Design ADRs** for the run model and execution record, and for
   storage. They settle the questions the requirements PR deferred: the
   stored status of a run being executed, an atomic commit across records
   in the file store (which the atomic-local-transaction write contract
   needs), and how an activation reports a failure of the execution-record
   store.
2. **The vertical slice**, steps 1 to 4 of the requirements' delivery plan
   (section 6), on the file store and the text layer: extract, verify,
   commit, crash, continue, no duplicate. Step 5 (TEST-6) is deferred as
   above.
3. **The verifier and exporter**, with no model (the intake requirements'
   build step 1), alongside the slice: the seven checks, CSV and manifest,
   hand-written records for the five samples and the mutation suite.
4. **The extraction agent** on the full library: visual reading once
   LLM-2 and TOOL-11 land, the headless runner and the folder trigger.
5. **The scoring harness** (TEST-4) against the 3/31/2026 answer key.
6. **The review UI**, embedded and served on localhost, with evidence
   highlighting from `maestro-cms#25`.
7. **The trial** with the operators, for one reporting cycle.

## After the MVP

In no fixed order: the Postgres store with STORE-5 and TEST-6; approval
(STM-8, TOOL-4, HOST-4's input delivery); MCP tools; host chat loops;
summarization; cost budgets; subagents; the wire protocol.
