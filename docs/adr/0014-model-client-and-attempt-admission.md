# 0014. The model client and attempt-level admission

- **Status:** Accepted
- **Date:** 2026-11-12

## Context

BUD-3 admits a hard budget before each provider attempt, every retry included, on
a known-safe upper bound of the attempt's usage. [ADR 0009](0009-budgets.md)
could not: retries run inside the host's client, out of the runtime's
sight, and `maestro-llms` had no input bound. So the MVP's client made one
attempt per call, hard token budgets failed closed with
`budget_unenforceable`, and token budgets had to be estimated (MVP.md
BUD-1, BUD-3).

`maestro-llms` v1.3.0 (#149) closes the upstream gap:

- `ratelimit.All` composes limiters, with an optional `Rollback`;
- `ReservationRequest.BoundUnits` carries an upper bound from the new
  `middleware.InputBounder`, which `DefaultEstimator` implements;
- maestro-llms ADR-0044 verifies that `MaxTokens` caps billed output,
  reasoning included, on Anthropic, OpenAI and Gemini.

A permanent `*llms.LimitError`, and any error that is neither a provider
nor a limit error, is not retried and does not count against the circuit
breaker. This library takes v1.4.0, which adds only embedding inputs.

The bound is the one agreed for BUD-3 on 2026-11-08: for text, its UTF-8
bytes plus per-request, per-message, per-part and per-tool margins (plus
Anthropic's hidden tool prompt), and the output cap as the output bound.
Images and documents are a bound only where a provider documents a
billing formula; maestro-llms' default estimator gives none, and neither
are PDF page figures or `count_tokens`, which are estimates. So a hard
token budget can be enforced on a text request to Anthropic, or to OpenAI
or Google without tools. A request with media, or with tools on OpenAI or
Google, has no bound, and a host budgeting one configures the budget as
estimated.

The open question was where the runtime sits in the host's chain.
Retries happen inside it (validation, retry, timeout, circuit, rate
limit), the host builds it once and shares it across runs, and the
budget is per run. Maestro's own chain is the most complex in use
(`pkg/agent/factory_llms.go`): it moves metrics outermost, and wraps the
chain in an error-mapping boundary and an empty-response validator.

It amends, item by item:

- ADR 0004 item 6 (a decorator around the injected client records each
  attempt; until #149 the client makes one attempt per call): by items 3
  and 4, the attempt layer inside the client's chain records each
  provider attempt, retries included, and a retried attempt is resolved
  by its retry, with no `model_failure` entry.
- ADR 0008 item 6 (the client is `ActivationInput.Client`, an
  `llms.ChatClient`): by item 1, it is a `*agent.ModelClient`.
- ADR 0009 item 4 (a turn is a logical call; until #149 a turn is one
  attempt): by item 5, a turn is one or more attempts, and its first
  attempt's commit counts it.
- ADR 0009 item 5 (the recording client admits each attempt; a hard token
  budget fails closed at its first attempt; `Reservation.Bound` is false
  until a bound exists): by items 5 and 6, the recording client admits the
  call (the turn budget, and the output cap lowered to what a token budget
  leaves) and the attempt layer admits each attempt; a hard budget is
  admitted on the attempt's bound, with `Bound` set, and refuses with
  `budget_unenforceable` only a request that has no bound.
- ADR 0009 item 7 (on recovery each `model_attempt` counts its turn): by
  item 5, only an attempt numbered 1, or recorded before attempts were
  numbered.
- ADR 0009's consequences (the host builds its client without retry
  middleware; token budgets must be estimated; when #149 lands admission
  moves into a budget limiter composed by `ratelimit.All`, whose `Reserve`
  commits the attempt and whose `Rollback` returns a reservation never
  sent): by items 1, 2 and 6, and the rejected alternatives below.

As the ADR index requires, the amended ADRs record the amendment in their
status line and in the index, and nowhere else; this list is the record
of what is amended.

## Decision

### The client

1. **`ActivationInput.Client` is a `*agent.ModelClient`**, which only
   `agent.NewModelClient(base, ModelClientConfig)` builds, over a
   `maestro-llms` provider client. Any other client fails to compile, so
   a host cannot hand the runtime a chain whose attempts it cannot see.
   A nil client stays allowed, for agent types with no model states. A
   model state reached with none fails the run with `model_error`, as
   before (ADR 0008, item 6). The host builds one per model route and
   shares it across runs and controllers. A call made outside any
   activation passes through unrecorded and unbudgeted.
2. **The chain's order is fixed:**

   `Outer` → validation → retry → timeout → circuit → rate limit → attempt → metrics → base

   `ModelClientConfig` holds the host's settings: `Limiter` (its rate
   limiter), `Estimator` (nil: `DefaultEstimator`), `Observer` (the
   innermost metrics layer, which sees each provider attempt), `Retry`
   (nil: maestro-llms' default), `Circuit`, `Timeout`, and `Outer`, the
   host's own layers, outermost first. Maestro's chain fits: its outermost
   metrics and its two wrappers are `Outer` layers. The order is not
   configurable: the attempt layer must sit inside the retry loop and
   inside the host's rate limiter. Being inside the rate limiter means a
   rate-limit wait or refusal happens before an attempt is admitted or
   recorded, so a refused request is never charged to a budget. Being
   outside the metrics layer means the observer sees only attempts
   actually sent. A layer inside the chain, if a host ever needs one, is
   a later addition.
3. **An outer layer must pass the request on unchanged.** It may observe
   a call, retry it and map its errors. The runtime records the request
   the model state built, so before each attempt the attempt layer
   compares the request it receives with that one. A difference refuses
   the attempt before it is recorded or sent, and fails the run with
   `model_error`. An outer layer that retries resends the same request,
   so each resend is a further attempt of the same turn.

### Attempts

4. **The attempt layer admits and records each provider attempt.** The
   recording client (ADR 0008, item 7) puts its model run in the call's
   context, and the layer finds it there. Before each attempt, retries
   included, the layer:
   - charges the attempt it replaces, if any, its reservation, since
     that attempt failed, so its usage is unknown (ADR 0004 item 6, BUD-4);
   - admits the attempt against what the token budget now leaves (item
     6);
   - commits its `model_attempt` entry before sending it.

   The first attempt's commit carries the state's pending results (and
   their checkpoint, if they complete the turn) and the request's
   artifacts, as before. A retry's entry repeats the first's request by
   reference, with its own number and reservation.

   A refusal, a failed commit or a changed request returns an error that
   maestro-llms neither retries nor counts against the circuit breaker,
   and the run's ending is decided as for any other refusal (ADR 0008,
   item 13).

   The recording client still commits the `turn` or the final
   `model_failure` once the call returns. A call that reached no attempt
   was refused before the attempt layer: by validation, an open circuit,
   the rate limiter or the context. It commits only its pending results,
   and the loop sees its error, so the run fails with `model_error`, or
   ends by its context's cause.
5. **A turn is a logical call; its attempts are numbered.**
   `ModelAttemptEntry.Attempt` numbers a call's provider attempts from 1.
   The first attempt's commit counts the turn, and a retry counts none.
   On recovery, the fold counts a turn only for an attempt numbered 1, or
   for an unnumbered one (recorded before this ADR). It charges an attempt
   followed by another attempt its reservation, as it charges an
   interrupted one (ADR 0004, item 13). A call refused before any attempt
   counts no turn.

### Admission

6. **Token admission happens in two places.**

   **Before the call**, the recording client:
   - refuses the call with `budget_exhausted` naming `turns` once the
     turn budget is spent;
   - with a token budget, lowers `MaxTokens` to what the budget leaves
     after the request's input. The input is its bound for a hard budget
     and its estimate for an estimated one. ADR 0009's thinking-budget
     guard is unchanged: a cap that cannot exceed the thinking budget
     refuses the call instead.

   **Before each attempt**, the attempt layer admits it against what is
   left then:
   - a **hard** budget reserves the request's bound (input plus output
     cap, `Reservation.Bound` set) and refuses an attempt whose bound
     exceeds what is left (`budget_exhausted`, `tokens`). A request with
     no bound fails the run with `budget_unenforceable` naming `tokens`
     before anything is sent. That covers an estimator that is not an
     `InputBounder`, and a request with images or documents, tools on a
     provider other than Anthropic, provider parameters or an unsupported
     provider. A bounder's error fails the run with `model_error`.
   - an **estimated** budget reserves the estimate and refuses an attempt
     whose estimated input leaves no output token (ADR 0009, item 6).
   - with no token budget the estimate is still reserved, so an attempt
     whose usage is unknown is charged something (ADR 0009, item 5).

   Since the first attempt's cap uses everything the budget leaves, a
   failed attempt is charged its whole reservation, and a hard-budgeted
   call can retry only if the budget has room for both. That is BUD-3's
   rule (a failed attempt whose consumption is unknown is charged its full
   reservation), not a defect. An agent type that wants retries under a
   hard budget sets a `MaxTokens` well below its budget.
7. **Charging is unchanged** (ADR 0009, items 7 and 8): observed usage
   from the `turn`, a reservation for an attempt that failed, was retried
   or was interrupted. The host's rate limiter keeps its own accounting
   through its reservation's `Commit` and `Release`, as maestro-llms'
   rate-limit middleware drives them.

## Rejected alternatives

- **A budget `ratelimit.Limiter` composed with the host's by
  `ratelimit.All`** (ADR 0009's plan). A limiter sees only a
  `ReservationRequest`, which holds token figures and not the request,
  but the attempt's entry must record the request write-ahead. A limiter
  also cannot see a layer that changed the request. The attempt layer
  gets the same placement, inside retry and after the rate limiter's
  wait, with the request in hand. `Rollback` is not needed: nothing after
  the attempt layer can refuse an attempt before it is sent.
- **Accepting any `llms.ChatClient`, and failing closed when a token
  budget meets a client the runtime does not recognize.** That refusal
  would arrive late, after work had been done, and only for runs with a
  token budget, while a foreign chain's retries would still go unrecorded
  and uncharged for every run. Requiring the library's client fails at
  compile time instead.
- **Recognizing a host decorator through `Unwrap`.** `Outer` layers
  cover the same need, and the compiler checks them, with no wrapper
  around the attempt layer for the runtime to look through.
- **Building the chain per activation from the host's configuration.**
  That would reset the circuit breaker's state per activation, and stop
  the rate limiter from being shared across runs.
- **A configurable chain order.** No host needs one: Maestro's
  difference is outermost metrics, an `Outer` layer here.

## Consequences

- **A host change:** a host builds its client with `agent.NewModelClient`
  and maestro-llms' retries; the "build without retry middleware" rule is
  gone. Hard token budgets work for requests with a bound. The intake
  agent's requests carry PDFs and images, so its token budget stays
  estimated.
- Each retry is on the record before it is sent, and charged. A run that
  keeps failing and retrying spends its token budget, but not its turns.
- An unlimited token budget needs no bound: with no token budget, every
  client works, and attempts are still recorded and reserved on the
  estimate.
- Not yet built: a layer inside the chain, a bound for media (a host can
  supply its own `InputBounder` where its provider documents a formula),
  and cost budgets (BUD-4).

## References

- [docs/requirements.md](../requirements.md): BUD-1, BUD-3, BUD-4,
  LLM-1, section 7 (maestro-llms#149)
- [MVP.md](../MVP.md): BUD-1, BUD-3
- [ADR 0004](0004-execution-record-and-recovery.md): items 6 and 13
- [ADR 0008](0008-model-states.md): items 6, 7 and 13
- [ADR 0009](0009-budgets.md): items 4 to 8, consequences
- `maestro-llms` v1.3.0: `ratelimit.All`, `middleware.InputBounder`,
  ADR-0042, ADR-0043, ADR-0044; v1.4.0
- Maestro: `pkg/agent/factory_llms.go` (`buildMaestroLLMsClient`)
