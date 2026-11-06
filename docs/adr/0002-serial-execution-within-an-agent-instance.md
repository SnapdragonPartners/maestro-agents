# 0002. Serial execution within an agent instance

- **Status:** Accepted
- **Date:** 2026-11-06

## Context

The library builds agents as state machines whose state is checkpointed at
every step boundary and restored in a later worker, possibly another
process (see [the requirements](../requirements.md), STM-6 and STM-7). Runs
are delivered at least once, and every write carries an idempotency key
derived from the run, the step and the call's position within the step
(TOOL-5).

If one instance could run states, model turns or tool calls concurrently,
each of those would need answers to hard questions: which branch's state a
checkpoint captures, how concurrent writes order their idempotency keys,
how two branches merge their state data, and which branch owns the
checkpoint sequence. Maestro v1's concurrent agents shared mutable state
through untyped maps and a dispatcher, and recovery did not survive a
restart. The consumer review of the requirements (on PR #2) proposed
removing the problem at its root, and a human operator agreed.

## Decision

1. **One execution stream per instance.** An agent instance executes
   serially: state processing, model turns and tool calls happen one at a
   time, including sibling tool calls returned in one model response, which
   execute in their recorded order.
2. **One execution owner.** At any moment an instance has at most one
   active owner (one worker holding its lease), and only that owner may
   advance the instance's checkpoint sequence.
3. **Concurrency is between instances.** Independent instances may run
   concurrently, in one process or across processes. Within an instance,
   heartbeats, cancellation and transport handling may run concurrently
   with the execution stream, but never advance workflow state themselves.
4. **Agent-level parallelism uses subagents (post-MVP).** Work that wants
   parallelism spawns separately identified child instances, each with its
   own state, checkpoints and owner, rather than sharing mutable workflow
   state. Spawning goes through the host, which controls admission and
   authority. The library plans for convenient spawning, roughly analogous
   to starting goroutines, with facilities to collect children's results
   and to cancel them.
5. **Not in the MVP.** Subagent spawning is post-MVP. The intake agent
   needs no parallelism within a document, and its implementation must not
   acquire subagent machinery.

## Consequences

- Checkpoints, idempotency keys and the execution record each describe one
  linear sequence, so recovery reasons about one order of events.
- Sibling tool calls no longer run in parallel. The cost is small: model
  turns dominate wall-clock time (buggrit measured about a minute per
  turn), and the model can still batch several calls into one turn.
- Parallel stages such as buggrit's concurrent find and verify passes
  become two subagent instances once spawning exists.
- Processing several inputs at once (several statements arriving together)
  means several instances; data they share goes through the object store,
  whose compare-and-set writes make concurrent instances safe (STORE-2).
- A design that needs shared mutable state between concurrent branches of
  one instance is out of bounds; it must be restated as instances that
  communicate through the host.

## References

- [docs/requirements.md](../requirements.md): STM-9, STM-10, TOOL-5, STORE-2
- Maestro [ADR 0032](https://github.com/SnapdragonPartners/maestro/blob/main/docs/adr/0032-agent-execution-contract.md): fenced execution authority (binding item 9)
- PR #2 consumer review: the source of this decision
