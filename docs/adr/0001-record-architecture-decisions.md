# 0001. Record architecture decisions

- **Status:** Accepted
- **Date:** 2026-11-06

## Context

`maestro-agents` is a foundation library for agents. Several consumers with
different needs will depend on it: standalone agent executables built in
this repository, the Maestro orchestrator, and Morris's planned
orchestration layer. It builds on the first-party `maestro-llms` (and
possibly `maestro-cms`) and will take in elements of `buggrit` and Maestro.

Decisions about the agent API, the boundary with orchestrators and what is
taken from those projects will be made incrementally, across many PRs, by
human and AI contributors who each start without the history. Code records
*what* was chosen, not *why*, what was rejected, or which limitation is
deliberate. Without that record a later contributor relitigates a settled
question or "fixes" a deliberate limitation. Reviewers (Codex before push,
buggrit after) also need a record to tell an intended design from a defect.

## Decision

Adopt **Architecture Decision Records**. Every significant, hard-to-reverse
or non-obvious architectural decision gets a numbered ADR in `docs/adr/`, in
the format described in [README.md](README.md). ADRs are append-only; a
decision changes only through a new ADR that supersedes or amends it.

`CLAUDE.md` carries the durable rules and the load-bearing decisions in
brief, linking to the ADR that holds each one's rationale; the two are kept
in sync.

## Consequences

- A small documentation cost per decision, in exchange for decisions that
  stay decided and limitations that stay understood.
- A structural change is expected to land with its ADR in the same PR.
- Reviewers treat a decision recorded in an ADR as intended, not as a
  defect; drift between the code and the ADRs is a defect.

## References

- [README.md](README.md): format and index
- `CLAUDE.md`: durable rules and workflow
- `../maestro-llms/docs/adr/`: the sibling library's log, which this one follows
- Michael Nygard, "Documenting Architecture Decisions"
