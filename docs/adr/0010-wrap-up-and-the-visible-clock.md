# 0010. Wrap-up and the visible clock

- **Status:** Accepted
- **Date:** 2026-11-10

## Context

LOOP-3 asks that, when a model state's budget nears its end, the loop force
the tools that leave the state, let the agent type relax validation for
that wrap-up turn, and bound the wrap-up's attempts, failing the run with
`wrap_up_failed` rather than looping. BUD-2 asks that active time keep a
reserve for wrap-up and that the model be shown the clock, and TOOL-7 lists
the visible clock among tool middleware. buggrit's `internal/review/stage.go`
and `clock.go` build all of this for one process: a context deadline less
the reserve ends the investigation, a final `toolloop` run forces the
terminal tool, a hook relaxes validation, and each tool result ends with
the time used.

[ADR 0008](0008-model-states.md) item 14 built the nudge and left forced
wrap-up to budgets; [ADR 0009](0009-budgets.md) built budgets and left the
reserve, forced wrap-up and the visible clock to this ADR. Here wrap-up has
to survive recovery: it is decided, recorded and resumed like everything
else a model state does (ADR 0004).

buggrit forces its terminal tool with `tool_choice`, which works on the
OpenAI-compatible models it runs on (DeepSeek, GLM, Qwen). Anthropic
rejects a forced tool choice (`any` or `tool`) with manual extended
thinking, and Claude Opus 5.5, Sonnet 5.5 and Fable 5.1 and 5.2 reject it
on every request; buggrit's own benchmark judge, which runs on Claude
Opus 5.5, uses auto with a nudge for that reason (`internal/bench/judge.go`).
The host chooses the model, and the runtime cannot know which providers
accept a forced choice.

It amends ADR 0007 item 13 (`Env` gains the budget status, item 9), ADR
0008 items 8 and 11 (a wrap-up turn's parameters and the tools it offers,
items 4 and 6), 10 (the checkpoint counts wrap-up turns, item 6) and 14
(the completion policy gains forced wrap-up, items 1 to 5), and ADR 0009
item 5 (an attempt's `StateParams`, item 6). As the ADR index requires, the
amended ADRs record the amendment in their status lines and in the index,
and nowhere else; this paragraph is the list of what is amended.

## Decision

### Declaring a wrap-up

1. **Every model state declares `ModelState.WrapUp`**: `Tools`, the tools
   that leave the state (required: at least one, each declared in the
   state's `Tools`, each once); `Message`, the user message that starts
   the wrap-up (default `DefaultWrapUpMessage`); `Reserve`, the time held
   back from each time budget (default a tenth of each); and `Turns`, the
   bound on wrap-up turns (default `DefaultWrapUpTurns`, 3, as buggrit).
   A missing or invalid declaration is refused at registration
   (`ErrInvalidType`). The reserve is the agent type's, per state, since
   the agent type knows what its wrap-up costs; the host sets the budgets.

   Rejected: offering every tool when `Tools` is empty (requiring a call
   does not require a transition, so it would spend the bound and fail), and
   "the next call is terminal, whatever it names" (whether a call
   transitions is its tool's result, which the runtime cannot impose).
   Several exits are allowed because a state can have several ways out
   (the intake agent submits, or sends the document to manual review).

### When a wrap-up begins

2. **A state wraps up when the run's consumption reaches its wrap-up
   point** on any budget the run sets (`WrapUpPoint`): the active time
   left is within `Reserve` (the active-time budget less the reserve,
   default a tenth of the budget); the time left to the deadline is within
   `Reserve` (default a tenth of the time from the run's creation to its
   deadline); or the turns left are within `Turns`, so the wrap-up always
   has its full bound within the turn budget. It is checked when the loop
   starts and **before each model attempt**, in the recording client, after
   the context and the step limit (ADR 0008, item 7) and before admission
   (ADR 0009, item 5). A state entered with a budget already within its
   reserve wraps up from its first turn.
3. **A wrap-up never ends** until the state transitions or the run ends.
   Consumption only grows (ADR 0009, item 7), so recovery reaches the same
   decision from the record; in addition the wrap-up turns a checkpoint
   counts, and any wrap-up attempt past the checkpoint (landed, failed or
   interrupted), carry it (item 6), so a version with a different reserve
   does not leave a wrap-up half way.
4. **A model call in flight at the wrap-up point is cut off there.** Before
   the wrap-up, the client's call runs under a context whose deadline is
   the earlier of the two clocks' points, with its own cause; when that
   cause ends the call while the activation's context is live, the attempt
   is recorded as a `model_failure` ("cut off at the wrap-up point"),
   charged its reservation (ADR 0009, item 8), and the loop starts again in
   wrap-up. Nothing of the turn landed, so the transcript is unchanged.
   **Tool calls are not cut off at the wrap-up point**, only by the hard
   clocks (ADR 0009, item 9): cutting a write mid-call would leave an
   unknown outcome (TOOL-12) to save a reserve the agent type can size.
   This is buggrit's lesson (design §9): a slow reasoning model can spend
   the whole budget on one turn.

   Rejected: a token-budget trigger. Sizing a wrap-up turn from an ordinary
   one is not legitimate (the wrap-up's input is the whole transcript), and
   a wrap-up turn is admitted like any other, its output cap lowered to
   what the budget leaves (ADR 0009, item 5).

### The wrap-up turn

5. **A wrap-up turn offers only the wrap-up's tools**, in declaration
   order, **with tool choice auto**, whatever the state's own (which could
   name a tool the wrap-up does not offer, or forbid tools). LOOP-3's
   forcing is the restriction, the message and the bound, not
   `tool_choice`: a forced choice would fail the run with `model_error` on
   the providers that reject one (Context), so it is not used. The wrap-up
   message is appended to the request that starts the wrap-up, and again
   after a wrap-up turn that called no tool; a resubmission after a tool
   error gets no message, as buggrit's does not. A call naming a declared tool
   the turn did not offer is refused by `toolloop` with its usual text
   (`tool "x" is not available`), recorded as a refused result, and
   refused alike by recovery, which also refuses a call naming a tool the
   attempt's recorded parameters did not offer, whatever the recovering
   version declares (amends ADR 0008, item 11). **A call executing
   in a wrap-up turn sees `Call.WrapUp`**, so its tool can relax what it
   accepts from a model out of budget: this replaces buggrit's
   `beforeWrapUp` hook, since tools here hold no state between executions.
   Rejected: an opt-in to force the choice for models that accept it; the
   agent type does not choose the model, and one that rejects it fails the
   run. It can be added when a use case appears.
   **The bound counts completed wrap-up turns** (the first call, a
   resubmission or a text answer); before a further attempt with the bound
   spent, the run fails with **`wrap_up_failed`**, its pending results
   committed with the ending (ADR 0008, item 13). A failed or interrupted
   attempt does not count: it is a model failure or is recovered. **The
   step limit comes first**: this ending, like the nudge's (ADR 0008 item
   14, so amended), is decided before the next attempt and after the
   activation's step limit (STM-7), so a step that completes at the limit
   ends the activation runnable with `step_limit` however it completed (a
   tool error, a text answer, a turn recovered after a crash), and the next
   activation fails the run.
6. **The record carries the wrap-up.** A wrap-up attempt's
   `ModelAttemptEntry` has `WrapUp` set and records the parameters it sent
   (the wrap-up's tools and tool choice) as `Params`, with the state's own
   as `StateParams`, which a recovered first turn restores and later
   attempts are prepared from (amends ADR 0008 item 8, and ADR 0009 item
   5, which introduced `StateParams` for a lowered output cap). The
   checkpoint's model progress counts the state's completed wrap-up turns
   (`wrap_up`; amends ADR 0008, item 10). A recovered turn's calls execute
   with `Call.WrapUp` from its attempt. Older records, which lack both,
   read as no wrap-up, so the format stays 1.

### The visible clock

7. **Each tool result that does not transition ends with the budget
   status** (`BudgetStatus.String`), appended to its text or as a final
   text part of a result with parts, for example `[budget: active time
   4 min of 20 min used, wrap-up at 18 min; turns 7 of 30 used, wrap-up
   after 27]`. It shows only the budgets the run sets (tokens marked `~`
   when estimated; cost budgets are not built), so a run with none sees
   no change. **It is part of the recorded result**, as the model received
   it (ADR 0008, item 8), so recovery and replay send the same transcript.
   Refusals and denials (ADR 0008, items 11, 12) carry none: they never
   reach a tool. A transition's result is not sent to the model.
8. **A state can be shown the clock before its first result.**
   `Env.Budget()` returns the status as of when the `Env` was made, with
   the running model state's wrap-up point (nil in a code state), so a
   `Prompt` can state the budget, as buggrit's opening message does
   (amends ADR 0007, item 13).
9. **The clock is a runtime wrapper, not yet composable middleware.**
   TOOL-7's composition (policy, idempotency, redaction TOOL-9, output caps
   TOOL-10) is decided with redaction and caps; there is no opt-out until a
   use case appears.

## Consequences

- **Every model state must declare `WrapUp.Tools`**; an agent type
  without it no longer registers. A tool that leaves the state may read
  `Call.WrapUp` to accept a partial submission.
- A run with a time or turn budget now ends a model state through its
  exit tools near the budget's end, rather than by `budget_exhausted`,
  when the model complies; the run fails with `wrap_up_failed` when it
  does not within the bound. A turn budget smaller than the wrap-up's
  turns wraps up from the first turn.
- Tool results in a budgeted run carry the clock line; an agent type's
  tests that compare transcripts see it.
- A wrap-up costs at most one extra model attempt (the call cut off at the
  point), charged its reservation.
- Not yet built, and decided with their own work: composable tool
  middleware (TOOL-7, TOOL-9, TOOL-10), cost budgets and their clock line
  (BUD-4), and the active-time pause for in-process waits (BUD-2, STM-8),
  which will pause the wrap-up point's timer with the active-time one.

## References

- [docs/requirements.md](../requirements.md): LOOP-3, BUD-1, BUD-2, TOOL-7
- [MVP.md](../MVP.md): LOOP-3, BUD-2, TOOL-7
- [ADR 0004](0004-execution-record-and-recovery.md): the write-ahead record
  and recovery
- [ADR 0007](0007-agent-types-and-the-run-controller.md): item 13 amended
- [ADR 0008](0008-model-states.md): items 8, 10, 11 and 14 amended
- [ADR 0009](0009-budgets.md): item 5 amended; the clocks (item 9) and
  charging (item 8) applied
- buggrit `internal/review/stage.go` (investigate, nudge, forced wrap-up,
  `wrapUpTurns`) and `clock.go` (the footer); design §9; `internal/bench/judge.go`
  (auto with a nudge on Claude Opus 5.5)
- Anthropic: [extended thinking](https://docs.claude.com/en/docs/build-with-claude/extended-thinking)
  (tool choice with thinking), [What's new in Claude Opus 5.5](https://platform.claude.com/docs/en/models/opus-5-5/whats-new-opus-5-5)
