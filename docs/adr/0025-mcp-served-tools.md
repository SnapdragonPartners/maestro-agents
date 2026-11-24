# 0025. MCP-served tools

- **Status:** Proposed
- **Date:** 2026-11-23

## Context

TOOL-6 asks that the registry hold tools served by an external MCP
server beside in-process Go tools, with the same grants, policy,
middleware and events. `docs/MVP.md` deferred it because Morris was to
run agents compiled in. That changed on 2026-11-18 (issue #23,
morris#707): every Morris agent runs on a private worker with no
database credentials, and reaches Morris only through Morris's APIs. The
tools that touch Morris's data (document read and creation, a ledger)
are served by Morris over MCP (`maestro-llms`' `mcpserver`, Streamable
HTTP), with OAuth tokens scoped to (run, principal, granted tools).
Tools that belong to the agent type and need no Morris data stay in
process. One model state offers both kinds.

ADR 0024 item 17 fixed the outline: the host passes a per-run tool
source in `ActivationInput` and owns its transport and token; a manifest
grants served tools by name; a start whose granted tool the server does
not offer fails with `config_error`; calls carry the run context; a
failure is a classified tool error; the replay harness stands in for the
server. This ADR decides the details.

The client exists: `maestro-llms/mcp` (a nested v0 module, `mcp/v0.1.0`,
over the official Go SDK; maestro-llms ADR-0049). `mcpclient.Connect`
opens a `Session` over `StreamableHTTP(endpoint, *http.Client)` or a
local `Command`; `Session.Tools` lists the server's tools as
`[]toolloop.Tool`, failing closed on a tool it cannot offer; a tool's
`Execute` returns a transport or protocol failure as an error and a
result with `isError` as a model-visible `IsError` result. It sources no
credentials: an HTTP server's travel in the caller's `http.Client`.

What the runtime already guarantees for an in-process tool, and must
keep for a served one: each call's permission, attempt and result are
recorded write-ahead (ADR 0008); the policy decision is bound to the
tool's content identity, which the manifest lists and recovery enforces
(ADR 0011 item 3); a write declares a contract, and an attempted write
with no result is reissued only if its contract makes that safe
(TOOL-12); telemetry projects the committed entries (ADR 0016).

It amends, item by item:

- ADR 0003 item 12 and ADR 0007 item 4 (what `ActivationInput`
  carries): by item 3, it also carries the run's tool sources.
- ADR 0004 item 2 (what `run_started` holds) and ADR 0007 item 8 (the
  checkpoint's contents): by item 4, they also record the digest of the
  artifact holding the run's served tool definitions.
- ADR 0008 item 3 (a tool is `Tool[D]` with its `Execute`): by item 1, a
  tool may instead be served, with `Tool.Source` and no `Execute`.
- ADR 0008 item 8 (the entry payloads): by item 6, `ResultEntry` also
  records a served call's failure class.
- ADR 0008 item 15 (a tool's returned error fails the run with
  `tool_error`): by item 6, a served call's error fails the run only for
  a write the server failed or never answered; otherwise it is a
  model-visible error.
- ADR 0016 item 2 (the fields an event carries): by item 6, a tool
  result's event also carries a served call's failure class, as its
  `ErrorKind`.
- ADR 0011 item 3 (a tool's content identity): by item 2, a served
  tool's identity covers its source, effect, contract and version, not
  its definition, which is fixed per run instead.
- ADR 0011 item 4 (tools from outside the agent type join by name, their
  version from the host or the server): by items 1 and 2, a served tool
  is declared by the agent type, and its version is the type's.

## Decision

1. **A served tool is declared by the agent type, not discovered.** An
   agent type grants a served tool in a model state's `Tools` with
   `agent.ServedTool[D](source, name string, effect Effect, contract
   WriteContract) Tool[D]`, which sets the new field `Tool.Source` and
   `Definition.Name`, and no `Execute`. `source` names a tool source the
   host provides (item 3); `name` is the tool's name on that server and
   the name the model sees. The type declares the effect and contract,
   as for any tool: an MCP annotation such as `readOnlyHint` is a hint
   from the server, and the runtime's reissue and duplicate-effect rules
   cannot rest on it. Registration refuses a served tool that sets
   `Execute`, a description or a schema (the server provides them,
   item 4), that declares `ContractAtomicLocal` (it returns no object
   operations), or that is named in `WrapUp.Tools` (a served result
   cannot transition, so it cannot leave the state). The tool-name rules
   are unchanged: a name names one tool in the type, across both kinds.

2. **Its identity covers the declaration; the definition is fixed per
   run.** The manifest lists a served tool with its source, effect,
   contract and version and no schema, and its content identity is the
   hash of those and its name (`toolIdentity` gains the source; an
   in-process tool's identity is unchanged). The permission records that
   identity and the policy decides on it, as today. The definition the
   model is offered (description and input schema) comes from the
   server, so it is fixed at the run's start instead (item 4).

3. **The host provides tool sources per activation.**
   `ActivationInput.ToolSources map[string]ToolSource`, keyed by the
   source names the type's tools declare. `agent.ToolSource` is the
   runtime's seam, with no MCP type in it:

   ```go
   type ToolSource interface {
       // Tools lists the definitions the source offers now.
       Tools(ctx context.Context) ([]llms.ToolDefinition, error)
       // Call runs one call and returns what the model receives.
       Call(ctx context.Context, call *ServedCall) (toolloop.ToolResult, error)
   }

   type ServedCall struct {
       Run            RunContext
       Fence          store.Fence // the activation's claim
       IdempotencyKey string      // a write's; empty for a read
       ToolCall       llms.ToolCall
       Action         ActionID
       WrapUp         bool
   }
   ```

   The host builds the source for the run's principal and owns it: its
   transport, its token and the token's renewal (an `http.Client` whose
   transport refreshes), reconnecting after a dropped session, and
   closing it. The runtime never closes a source. A source is supplied
   per activation, since that is the unit the host hands the runtime; a
   host may keep one session for a run's activations or open one each
   time. The lifetime Morris asks for (the session's is the run's) is
   the host's to keep.

4. **The start checks every granted served tool and records its
   definition.** When a run starts, the runtime lists each source its
   type declares and requires every served tool the type grants, in any
   state, to be offered: a missing source, a source whose listing fails,
   or a granted tool the source does not offer fails the start with
   `config_error`, each problem an `agent.StartProblem` with the new
   check `CheckToolSources` (`"tool_sources"`). A listing that names a
   tool twice, or offers a granted tool with a description that is not
   valid UTF-8 or a schema that is not valid JSON, is a problem too: the
   record could not keep the definition exactly. The definitions of the
   granted tools (name, description, compacted schema) are recorded in an
   artifact whose digest `run_started` holds (`RunStarted.Served`), so
   the model is offered the same definition on
   every turn of the run and the record shows what it was offered.
   Tools a source offers but the type does not grant are ignored: a
   grant is by name (TOOL-1). A later activation lists its sources again
   and compares: a granted tool missing, or with a different definition,
   fails the run with `tool_error` (the model was told one contract; a
   call would go to another), as does a source the activation lacks or
   one whose listing fails. A schema that differs only in formatting is
   not a change: schemas are compared compacted.
   `Registry.CheckStart` makes no network call and does not run this
   check; a host that wants it before proposing a run calls
   `Registry.CheckToolSources(ctx, agentType, sources) []StartProblem`,
   the same code path the start runs.

5. **A served call takes the in-process path.** It is granted, offered,
   refused if not offered, decided by the policy, recorded (permission,
   attempt, result), redacted, capped, wrapped up and projected to
   telemetry exactly as an in-process call (TOOL-6's "same grants,
   policy, middleware and events"). Only the execution differs: the
   runtime calls `ToolSource.Call` with the call's run context (TOOL-8),
   action identity and, for a write, its idempotency key (TOOL-5), so
   the host can tie the call to the run, the server can re-check the
   principal, and an idempotent destination can deduplicate. A served
   call is never given the credential provider: its authority is the
   source's, which the host built for the run's principal.

   The call also carries the activation's fence (instance, owner and
   owner epoch, ADR 0003 item 8), on every call, reads included. A host
   that keeps the store can then refuse a call from a superseded claim,
   one whose epoch is not the instance's current one, as the store
   refuses that claim's commit; a superseded claim reads nothing either.
   The fence check is on the epoch alone, and the fence is never part
   of the idempotency key: a later claim that issues the same call again
   (a read after recovery, and an idempotent write if reissue is ever
   built, item 6) carries a new owner and epoch but the same action
   identity, so the same key. The destination accepts it because its
   epoch is current and deduplicates it by the key. Staleness is the
   epoch's to tell, deduplication the key's; putting the fence, or any
   attempt marker, into the key would defeat deduplication across
   claims. A destination's refusal of a stale epoch reaches the
   runtime as whatever class the source gives it (for an MCP server's
   JSON-RPC error, `source_error`), and that class never reaches the
   record: the store refuses the superseded claim's commit of the call's
   result by the same epoch check (ADR 0005), so the activation ends
   with an error and no outcome (ADR 0003 item 14). The run's current
   claim then finds at most the call's attempt, if the superseded claim
   committed it before losing the instance, and resolves it as recovery
   does (ADR 0008 item 18: a read is issued again, a write follows its
   contract). No result is recorded for the refused call.

6. **Failure is classified, never a crash.** A result with `IsError`
   (MCP's `isError`) is model-visible, as for any tool: the server's
   tool chose to tell the model. An error from `Call` falls in one of
   three classes, which the source reports and the runtime records as
   `ResultEntry.Failure`:

   - **`unreachable`**: the call was never sent (the server could not be
     reached, the session was closed before the request went out). The
     source returns an error wrapping `agent.ErrSourceUnreachable`.
     Nothing happened, so a retry cannot duplicate an effect;
   - **`source_error`**: the server answered the call with an error (a
     JSON-RPC error: an unknown tool, invalid parameters, a failing
     handler) or a malformed result. The source returns an error
     wrapping `agent.ErrSourceError`. The server has spoken, so asking
     again would not help;
   - **`unknown`**: any other error, chiefly a call sent and never
     answered (a timeout, a session dropped in flight), whose effect is
     unknown.

   What follows depends on the class and the tool's effect:

   | | read | write |
   |---|---|---|
   | `unreachable` | model-visible error | model-visible error |
   | `source_error` | model-visible error | run fails, `tool_error` |
   | `unknown` | model-visible error | `ResultUnknown`, run fails, `tool_error` |

   A model-visible error is recorded as executed with `IsError` and a
   fixed message per class (that the tool's server could not be reached,
   or that it failed); the model may call again or wrap up, and each
   retry is a turn, so the turn and time budgets bound it. A read cannot
   have had an effect, so every class is model-visible. An unreachable
   write is too, since it was never sent: the model's next call is a new
   action with a new key, and there is nothing for it to duplicate. A
   write the server failed, like an in-process tool's returned error,
   fails the run: its effect is the server's to report, and it reported
   a failure, not a result. A write sent and not answered is TOOL-12's
   unknown outcome. Reissuing an idempotent write under its recorded key
   from a later activation, rather than failing, is left for when a
   consumer needs it.

   A context the runtime ended (cancel, a budget's clock, shutdown) is
   that cause's outcome, as today, not a tool failure. The telemetry
   event for the result carries the class as its code.

7. **The replay harness's stand-in is a source.** Since the seam is an
   interface with no MCP in it, a test supplies any `ToolSource`. The
   new package `agent/agenttest` provides `agenttest.Source`: scripted
   definitions and per-tool handlers, failure injection (unavailable,
   dropped mid-run, a changed definition) and a log of every
   `ServedCall` received, so an agent type's served tools are testable
   without a server. When the replay harness (TEST-1) is built, it
   serves recorded results through the same seam.

8. **The MCP adapter is a nested module, built in a later PR.**
   `github.com/SnapdragonPartners/maestro-agents/mcptools`, its own
   `go.mod` in `mcptools/`, at v0 (tags `mcptools/vX.Y.Z`), following
   `maestro-llms`' `mcp` module (its ADR-0049 item 1): only programs
   that require it take on `maestro-llms/mcp` and the MCP SDK with its
   JSON Schema, JWT and OAuth dependencies, and MCP's pace of change
   stays outside the root module's API. It requires the root module at
   a released version with no `replace`, so it follows the release that
   carries this ADR's seam (v0.2.0); CI also builds and tests it against
   the root at HEAD through a temporary, uncommitted `go.work`, so a
   root change that breaks it fails its own PR. The SDK meets the
   dependency bar: a protocol implementation, the official SDK, owned
   through `maestro-llms`.

   `mcptools.New(dial func(ctx) (*mcpclient.Session, error)) *Source`
   adapts a session to `ToolSource`: it dials on first use, redials
   after a failure that dropped the session, and closes with
   `Source.Close`. It sends the `ServedCall`'s run context, fence
   (instance, owner and epoch, item 5), action and idempotency key in
   the call's MCP `_meta`, and maps the client's
   errors to item 6's classes. `mcpclient` cannot yet do either (it sets
   no `_meta` and does not tell an unsent call from an unanswered one),
   which is asked of `maestro-llms` in maestro-llms#194, since every
   consumer passing per-call context or retrying safely needs it; the
   adapter's PR follows it.

9. **Not in this ADR.** The standalone host gains no tool-source
   configuration (no consumer asks; a later ADR adds it with the
   multi-agent host, #32). Exposing a registry over MCP (TOOL-6's
   reverse) stays planned. Approval for served tools follows TOOL-4
   when it is built. A server's tool-list-changed notification is not
   followed mid-run: item 4's comparison at each activation is the only
   check.

## Consequences

- An agent type grants Morris's tools by name beside its own, and
  Morris's worker passes one source per activation built for the run's
  principal; nothing else about running the agent changes.
- `ActivationInput` gains a field, `RunStarted` and `ResultEntry` gain
  fields, `Tool` gains `Source`, the manifest's tool entry gains
  `source`; existing records and in-process identities are unchanged.
- A server that changes a granted tool's definition mid-run fails
  every run in flight that grants it, on its next activation. That is
  deliberate: the policy, the record and the model all saw the old one.
  So does a new version of the agent type that grants a served tool the
  run's start did not record: its definition was never fixed for the
  run.
- A read's failure, or a write that never left, costs a turn rather
  than the run; a write the server failed or never answered never
  silently repeats.
- The root `go.mod` gains nothing; `mcptools` is released on its own,
  after the root release that carries the seam.

## References

- Issue #23; maestro-llms#194; morris#707; ADR 0024 item 17; `docs/contract.md` section 7
- Requirements TOOL-1 to TOOL-8, TOOL-12, TEST-1
- ADRs 0008, 0011, 0016; maestro-llms ADR-0049
