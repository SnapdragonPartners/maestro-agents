# 0009. Budgets

- **Status:** Amended by ADR-0010
- **Date:** 2026-11-10

## Context

BUD-1 to BUD-4 ask for per-run budgets of tokens, turns, active time, an
elapsed deadline and cost, hard by default, enforced by admission before
each provider attempt (BUD-3) and by cancelling the activation's context
(BUD-2). [ADR 0004](0004-execution-record-and-recovery.md) put each
attempt's reservation in its `model_attempt` entry (items 2, 6), consumed
budgets in the checkpoint (item 3) and the charging of interrupted attempts
in recovery (item 13); [ADR 0007](0007-agent-types-and-the-run-controller.md)
left `Outcome.Consumed` empty and ADR 0008 left budgets to their own work.

Admission per attempt, retries included, and a known-safe input bound need
`maestro-llms#149`. As of this ADR its first part (`ratelimit.All` with an
optional `Rollback`, maestro-llms PR 159) is in review and the bound
interface is not yet started. MVP.md (BUD-1, BUD-3) plans for that
interval: each model call is a single provider attempt, admitted on its
estimate; hard turn and time budgets are enforced; a token budget must be
configured as estimated; a hard one fails closed. This ADR records how
budgets are built on that plan, shaped so that admission moves into a
budget limiter once #149 lands.

It amends ADR 0007 items 4 (the activation input carries the budgets, item
2), 8 (the checkpoint carries the budgets and their consumption, items 2
and 7) and 16 (`Outcome.Consumed`, item 7), and builds what ADR 0004 items
2, 3, 6 and 13 and ADR 0008's consequences left for budgets. As the ADR
index requires, the amended ADR records the amendment in its status line
and in the index, and nowhere else; this paragraph is the list of what is
amended.

## Decision

### The budgets

1. **`agent.Budgets`** holds `Tokens` (a `TokenBudget`: `Limit` and
   `Estimated`), `Turns`, `ActiveTime`, `Deadline` (an absolute time) and
   `Cost`. Zero means no limit (BUD-1). `Reason.Budget` names a budget as
   `tokens`, `turns`, `active_time`, `deadline` or `cost`, for
   `budget_exhausted` and `budget_unenforceable` alike.
2. **The host passes the budgets with the run's first activation**, in
   `ActivationInput.Budgets` (amends ADR 0007, item 4). They are recorded
   in `run_started` and in every checkpoint (amends ADR 0007, item 8), and
   fixed for the run: a later activation reads them from its checkpoint and
   ignores the input's, as it does `Config` (ADR 0007, item 7). Defaults
   from the manifest come with manifests (MAN-1). A negative budget, or an
   estimated token budget with no limit, fails the run with `state_error`,
   as an invalid configuration does. The run's inputs (budgets and
   configuration) are prepared before anything can refuse them, so a start
   refused for its budgets records them with its ending (REC-1, ADR 0007
   item 6).
3. **Cost is a contract** (MVP.md, BUD-1): `Pricing` is the host's pricing
   function, `Price(model, usage) (cost, ok)`, where `ok` false is an
   unpriced model, an explicit unknown (BUD-4). Nothing calls it yet. A run
   that sets a cost budget fails at its start with `budget_unenforceable`
   naming `cost`, before any work, rather than running unenforced.

### Admission

4. **A turn is a logical call to the model client**, counted when it is
   admitted, whether it then succeeds, fails or is interrupted, so a run
   that keeps dying mid-call cannot exceed its turn budget. Once #149's
   retries run inside the client, a retry is a further attempt of the same
   turn: it is admitted against the token budget, not counted as a turn.
   Until then a turn is one attempt.
5. **The recording client admits each attempt** before committing its
   `model_attempt`, after the step limit and the context's end are checked
   (ADR 0008, item 7):
   - the turn budget refuses the turn once the turns admitted reach it
     (`budget_exhausted`, `turns`);
   - **a hard token budget fails closed**: with no known-safe input bound
     available until #149, its first attempt fails the run with
     `budget_unenforceable` naming `tokens`, and no attempt is made;
   - **an estimated token budget** reserves maestro-llms'
     `DefaultEstimator` input estimate and refuses the attempt
     (`budget_exhausted`, `tokens`) when that estimate leaves no output
     token, or, for a request with a thinking budget
     (`Reasoning.BudgetTokens`), no output cap above it (Anthropic's
     thinking budget must be below `MaxTokens`), so lowering the cap never
     turns a valid request into one the provider rejects. A prompt whose
     own cap is not above its thinking budget is invalid with or without a
     budget and is sent as it is: it is not a budget's refusal, and the
     runtime does not predict what a provider accepts. Otherwise it lowers `MaxTokens` to what the budget leaves after
     the input, keeping the prompt's own cap where that is lower.

   **An attempt records the parameters it sent** (ADR 0008, item 8): when
   admission lowered the cap, its `Params` are the state's parameters with
   the lowered cap, and `StateParams` the state's own, which a recovered
   first turn restores, so later attempts are admitted from the prompt's
   cap, not a lowered one.

   The reservation (`Reservation`: tokens, the output cap sent, and
   `Bound`, false until a bound exists) is recorded in the `model_attempt`
   entry. With no token budget the attempt is still reserved on the
   estimate, so an attempt whose usage is unknown is charged something. A
   refusal is an ending made while the model state runs, so it carries the
   state's pending results (ADR 0008, item 13).
6. **An estimated budget is exhausted at the next admission.** BUD-1 fails
   an estimated budget "when observed consumption reaches it": the runtime
   checks that when the next attempt is admitted, which then cannot fit. A
   run whose last turn reaches the budget and then needs no further model
   call is not failed; its overshoot is within the one attempt BUD-1
   allows.

### Charging and consumption

7. **`Consumption`** is the tokens charged, the turns admitted, the active
   time spent, whether the token figure includes an estimate
   (`TokensEstimated`), and the same per model route (BUD-4), with each
   route's observed input and output tokens and its count of attempts of
   unknown usage. It is checkpointed as of the entries each checkpoint
   covers (amends ADR 0007, item 8), recorded in every `outcome` entry and,
   in the same commit, in the run record's consumed budgets (ADR 0003 item
   13, ADR 0005 item 3), so a host reading the store sees it, and returned
   as the typed `Outcome.Consumed` (amends ADR 0007, item 16).
   Recovery rebuilds it from the checkpoint and the tail: each
   `model_attempt` counts its turn (live, the turn is counted with the
   attempt's commit, after the checkpoint the state's pending results may
   complete in that commit, so that checkpoint never counts it), each `turn`
   charges its usage, and each
   `model_failure`, and each attempt with neither, charges its reservation
   (ADR 0004, items 6 and 13), before any new attempt is admitted. An
   attempt recorded before budgets were built has no reservation; its
   usage is unknown and uncharged, and the figure is marked estimated.
8. **An observed usage is charged** all input plus billable output,
   reasoning included (`InputTokens + max(BillableOutputTokens,
   OutputTokens + ReasoningTokens)`), or the provider's `TotalTokens` where
   that is larger, since Gemini's counts tool-use prompt tokens its input
   does not (maestro-llms ADR-0016, ADR-0034). A usage that reports nothing
   is unknown and charged the reservation (BUD-4). maestro-llms errors
   carry no usage, so a provider failure is charged its reservation. A
   response the runtime cannot record (ADR 0008, item 8) completed a
   provider call: it is committed as a `model_failure` carrying the usage
   it reports (ADR 0004 item 2's "any usage reported"), charged that
   usage, before the run fails with `model_error`.

### The clocks

9. **The deadline is a context deadline and active time a timeout** of
   what is left of it, each with a `*BudgetExhausted` cause naming the
   budget, so ADR 0007 item 14's ending by cause fails the run with
   `budget_exhausted`, and a model call, tool call or store read in flight
   is cut off rather than overshooting (BUD-2). An activation that starts
   with either spent ends at once, after `activation_started` and before
   any agent-type code; a first activation still records the run's start
   with its ending (ADR 0007, item 6).
10. **Active time is measured from the clocks' start**, after the
    activation's reads and before its first commit, and recorded
    cumulatively in each checkpoint and on **every execution-record
    entry**: each payload embeds `Clock` (`active`), stamped where the
    runtime adds entries, so no kind of entry can omit it. Recovery takes
    the latest the checkpoint and tail record, so a crash loses at most the
    time since the last commit (one model or tool call). There are no in-process waits yet, so active
    time never pauses (MVP.md, BUD-2).

## Consequences

- A host sets budgets per run at its first activation and reads
  consumption from every `Outcome`. Until #149 lands it must configure
  token budgets as estimated, and build its client without maestro-llms'
  retry middleware (MVP.md, BUD-1): retries inside the client are attempts
  the runtime neither admits nor sees, so the tokens of a retried failure
  would go uncharged.
- An agent type should set `MaxTokens` in a budgeted state's prompt.
  Without it, admission lowers the cap to everything an estimated budget
  leaves, and an attempt whose usage turns out unknown (a provider that
  reports none, a failure, a crash mid-call) is charged that whole
  reservation (BUD-3, BUD-4), which exhausts the budget.
- Hard turn, active-time and deadline budgets are enforced; a configured
  hard budget is never silently unenforced (BUD-1): a hard token budget
  and any cost budget fail closed with `budget_unenforceable`.
- When #149 lands, admission moves into a budget limiter in the client's
  limiter slot, composed with the host's rate limiter by `ratelimit.All`:
  its `Reserve` commits the `model_attempt` with the reservation, its
  `Rollback` returns a reservation never sent, and the bound interface
  makes hard token budgets admissible. The turn stays counted in the
  recording client (item 4). That work amends this ADR.
- Not yet built, and decided with their own work: the active-time reserve
  for wrap-up and forced wrap-up near a budget's end (BUD-2, LOOP-3, ADR
  0008 item 14), the visible clock (BUD-2, TOOL-7), cost budgets (BUD-4),
  the active-time pause for in-process waits (BUD-2, STM-8), usage
  telemetry (HOST-1) and manifest defaults (MAN-1).

## References

- [docs/requirements.md](../requirements.md): BUD-1 to BUD-4, RUN-1,
  RUN-3, LOOP-3
- [MVP.md](../MVP.md): BUD-1 to BUD-4
- [ADR 0004](0004-execution-record-and-recovery.md): the reservation in
  `model_attempt` (items 2, 6), consumption in the checkpoint (item 3),
  interrupted attempts (item 13)
- [ADR 0007](0007-agent-types-and-the-run-controller.md): items 4, 8 and
  16 amended as listed in the Context; item 14 applied
- [ADR 0008](0008-model-states.md): the recording client (item 7), endings
  carrying pending results (item 13)
- `maestro-llms`: issue #149, PR 159 (`ratelimit.All`, `Rollback`),
  `middleware.DefaultEstimator`, ADR-0016, ADR-0034
