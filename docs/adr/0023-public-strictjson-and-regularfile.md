# 0023. Public `strictjson` and `regularfile` for agents in other modules

- **Status:** Accepted
- **Date:** 2026-11-19

## Context

Agents are leaving this repository. Each one, starting with the intake
agent (`cmd/addepar`), moves to `SnapdragonPartners/morris-agents` as a
Go module of its own, and `maestro-agents` keeps the shared library: the
runtime, the stores, the standalone host and, later, the local
multi-agent host and the app contract. Morris's worker, a Mac app with
agents compiled in, and each agent's own CLI import the agents from
there.

The intake agent imports two of this module's `internal/` packages:
`internal/strictjson`, the strict JSON decoder for operators' files
(ADR 0017, item 5) and, since ADR 0021 item 13, every extraction record
and every tool's and review action's arguments; and
`internal/regularfile`, which opens an operator's file only if it is
regular, never hanging on a FIFO (ADR 0017, item 11). Go forbids an
`internal/` import from another module, so the agent cannot move while
they stay there. Copying them into the agent's module would give two
decoders that drift, when ADR 0021 item 13's point is that one decoder
reads a model's and a person's input alike, and the host and its agents
must refuse the same files.

## Decision

1. **`strictjson` and `regularfile` are public packages at the module
   root**: `github.com/SnapdragonPartners/maestro-agents/strictjson` and
   `.../regularfile`. The exported API keeps its shape
   (`strictjson.Decode`; `regularfile.Open`, `Check`, `ReadCapped`,
   `PlusOne` and `ErrTooLarge`) and the tests move with the packages.
   Two things change: the package comments name agents in general
   rather than the intake agent; and `strictjson.Decode`, which a caller
   in another module may now misuse, refuses a destination that is not a
   non-nil pointer with `*json.InvalidUnmarshalError`, as `encoding/json`
   does, where it panicked, with one test added for it. `regularfile`'s
   behavior is unchanged.

2. **They are the decoder and file opener for agents.** An agent built
   on this library decodes its operators' files and its tools' and
   actions' arguments with `strictjson.Decode`, and opens an operator's
   file with `regularfile`, so it refuses what the standalone host
   refuses.

3. **The rest of `internal/` stays internal.** `internal/storecore` and
   `internal/jsonschema` are reached only through `store/`, `agent/` and
   `standalone/`; no agent imports them, and the module's public
   surface grows only by these two packages.

4. **Their API follows the module's version.** They are stable under
   the same rule as the rest of the module: before v1, a minor release
   may change an API, and its release notes say so. Tag `v0.1.0`, the
   first, is cut from `main` once this lands and is what an agent's
   module first requires.

### Amendments

- **ADR 0021, item 13:** the decoder it names, `internal/strictjson`, is
  now the public package `strictjson`; the decision is unchanged.

## Consequences

- An agent's module needs no copy of either package, and a fix to the
  decoder reaches the host and every agent with one version bump.
- Both packages are now public API: an incompatible change is a
  breaking change for agents in other modules, recorded in the release
  notes, where before it was a refactor inside one module.
- Neither adds a dependency: both use the standard library alone.

## Not decided here

- The rest of the split: moving the intake agent to `morris-agents`,
  removing `cmd/addepar` from this repository, and the local
  multi-agent host and app contract extracted from `addepar serve`.
  Each lands with its own ADR.

## References

- [ADR 0017](0017-the-standalone-host.md), items 5 and 11
- [ADR 0021](0021-review-writes-approval-and-export.md), item 13
- Go's internal packages: <https://go.dev/doc/go1.4#internalpackages>
