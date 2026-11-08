# Architecture Decision Records

This directory holds the **Architecture Decision Records (ADRs)** for
`maestro-agents`: short documents that each capture one significant
architectural decision, its context, and its consequences.

## When to write one

Write an ADR when a technical decision materially affects security,
isolation, data handling, the public API, how agents are run or managed by
orchestrators, or long-term architecture, or when it is hard to reverse or
non-obvious. A structural change lands with its ADR in the same PR.

## Format

Lightweight Nygard style. One decision per file, named
`NNNN-kebab-title.md`, numbered in creation order (four digits, zero
padded). Each ADR has **Status**, **Date**, **Context**, **Decision**,
**Consequences** and **References**.

- ADRs are **append-only**. To change a decision, add a new ADR and set the
  old one's status to `Superseded by ADR-NNNN` (or `Amended by ADR-NNNN`
  for a partial change). Recording an amendment in the old ADR's status
  line, and in the index below, is the only edit an accepted ADR gets.
- Status values: `Proposed`, `Accepted`, `Amended by ADR-NNNN`,
  `Superseded by ADR-NNNN`, `Deprecated`.
- Keep them terse and binding. An ADR is a decision, not an essay.
- Retrofitting an ADR for a decision already made is fine; date it to when
  the decision was taken.

Copy [0001](0001-record-architecture-decisions.md)'s structure as the
template.

## Index

| ADR | Title | Status |
|---|---|---|
| [0001](0001-record-architecture-decisions.md) | Record architecture decisions | Accepted |
| [0002](0002-serial-execution-within-an-agent-instance.md) | Serial execution within an agent instance | Accepted |
| [0003](0003-run-model-identities-and-activation-results.md) | Run model: identities, stored status and activation results | Amended by ADR-0007 |
| [0004](0004-execution-record-and-recovery.md) | Execution record and recovery | Accepted |
| [0005](0005-storage-one-fenced-commit.md) | Storage: one fenced commit, and the reference stores | Amended by ADR-0006 |
| [0006](0006-store-interface.md) | The store interface | Accepted |
| [0007](0007-agent-types-and-the-run-controller.md) | Agent types and the run controller | Accepted |
