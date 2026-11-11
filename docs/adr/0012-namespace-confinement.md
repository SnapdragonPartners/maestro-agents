# 0012. Namespace confinement

- **Status:** Accepted
- **Date:** 2026-11-11

## Context

STORE-2 gives each agent an object store "per namespace the host assigns":
instances of one configured agent may share a namespace (several documents
processed at once are several instances, [ADR
0002](0002-serial-execution-within-an-agent-instance.md)), and "an
instance cannot read a namespace it was not given". Until now nothing
enforced that. `agent.Env` read any namespace agent code named, and a code
state's transition or an atomic local transaction's result could carry
operations in any namespace. [ADR 0005](0005-storage-one-fenced-commit.md)
item 7 and [ADR 0008](0008-model-states.md) item 4 left confinement to
"the namespaces a grant names", for manifests, and [ADR
0011](0011-manifests.md) split it out to its own work.

The design first drafted for it had each agent type declare logical
namespaces, which the host bound to physical ones and granted per state or
per tool. That design was rejected for its cost against present need, not
because it cannot work: agent code could name a logical handle that the
host binds and the runtime records, without learning physical names. But
within one instance, a per-state grant isolates little, since a code
state can read whatever the type's other states read, and no consumer yet
needs more than one namespace. The intake agent, the MVP's one consumer,
needs two things from the object store: "has this file hash been seen?"
(a lookup) and "records awaiting review" (a prefix listing). Both fit in
one namespace, under two key prefixes.

It amends, item by item:

- ADR 0003 item 12 (what `ActivationInput` carries): by item 1, it also
  carries the run's namespace.
- ADR 0004 item 2 (what `run_started` holds): by item 2, it also records
  the run's namespace.
- ADR 0005 item 7 (an atomic local transaction reads "a read-only view of
  the namespaces its grant names"): by items 3 and 4, it reads the run's
  one namespace, and its operations must be in it.
- ADR 0007 item 4 (what the activation input carries): by item 1, the
  run's namespace too.
- ADR 0007 item 6 (what ends a run before its start commit lands): by
  item 5, a missing namespace, or one the store refuses, ends it too, with
  `config_error`, and the ending records the start.
- ADR 0007 item 8 (the checkpoint's contents): by item 2, the run's
  namespace too.
- ADR 0007 item 10 (a code state's transition carries object operations;
  any other kind fails the run): by item 4, an object operation outside
  the run's namespace fails it too, with `state_error`.
- ADR 0007 item 13 (`Env` exposes object reads): by item 3, they read the
  run's namespace only and take no namespace argument, and `Env` builds
  operations in it.
- ADR 0008 item 4 (a tool's read view is the run's `Env`; confinement to
  "the namespaces a grant names comes with manifests"): by items 3 and 4,
  the view is the run's namespace, and an operation outside it fails the
  run with `tool_error`.
- ADR 0011 item 8 (what fails a run with `config_error`): by item 5, a
  missing namespace, one that is not valid UTF-8, and one the store
  refuses.

## Decision

1. **A run has exactly one namespace, assigned by the host.**
   `ActivationInput.Namespace` names it. Instances given the same
   namespace share its objects (STORE-2's shared reference data, such as
   dedup hashes); a run reaches no other. The host's configuration of an
   instance is where its namespace comes from; the runtime enforces it per
   run, so consistency across an instance's runs is the host's (the
   runtime does not compare one run's namespace with another's).
2. **The namespace is fixed when the run starts**, like the configuration
   and the budgets (RUN-1; [ADR 0007](0007-agent-types-and-the-run-controller.md)
   item 7, [ADR 0009](0009-budgets.md) item 2). It is recorded in
   `run_started` (`RunStarted.Namespace`) and in every checkpoint; later
   activations take it from the checkpoint and ignore the input's. A
   checkpoint that records none was not written by this runtime, and the
   activation is refused (`ErrNotActivatable`) rather than guessing one.
3. **Agent code never names a namespace.** `Env.Object(ctx, key)` and
   `Env.ListObjects(ctx, prefix)` read the run's namespace, and
   `Env.PutJSON`, `Env.PutBytes` and `Env.Delete` build operations in it.
   `Env` does not expose the namespace's name: there is nothing for agent
   code to do with it. Confinement is structural for reads: no read can
   name another namespace.
4. **Every object operation agent code returns is checked** at the one
   place each reaches a commit: a code state's transition (with the other
   checks of ADR 0007 item 10) and an atomic local transaction's result
   (with the other checks of ADR 0008 item 4). An operation built by hand
   (`store.PutObjectJSON` and the like) in another namespace fails the run:
   `state_error` for a code state, `tool_error` for a tool, the error
   naming the namespace. Nothing the state or tool returned lands.
5. **A namespace is required.** A run started with none, or with one that
   is not valid UTF-8, fails at its start with `config_error`, before
   `Init` runs: an absent namespace is more likely a host failure than a
   type that keeps no objects, so there is no "no objects" mode. Each
   store has its own limits on a namespace (the reference stores', on its
   encoded length), so the start also reads one key in it
   (`namespace-probe`): a namespace the store refuses as invalid fails
   with `config_error`, the host's configuration to fix, not the agent
   type's `state_error` at its first read. A store failure on that read
   is a store failure (ADR 0003, item 15).

## Consequences

- Breaking for agent code: `Env.Object` and `Env.ListObjects` lose their
  namespace argument, and operations are built with `Env`'s helpers. Hosts
  must set `ActivationInput.Namespace` on every run's first activation.
- STORE-2's "cannot read a namespace it was not given" holds for agent
  code by construction, and its writes by item 4's check. A host's own
  code, holding the store, is not confined; nor are tools that do not
  write through the object store (external effects, TOOL-12).
- One extra point read per run start (item 5's probe).
- Key prefixes organize a namespace; they do not confine. Every run given
  a namespace can read and write all of it, so `department/A/` beside
  `department/B/` in one namespace is a convention, not a boundary.
- One namespace per run partitions access into disjoint sharing groups.
  It does not fit data shared on overlapping scopes: for example, records
  private to one department beside a registry shared by all of them,
  where a step must write both in one commit with its checkpoint. A tool
  cannot bridge that, since its operations must be in the run's namespace
  (item 4), and a separate service gives up the single commit. That case
  is narrower than it looks: when the registry guards an external effect
  (a billing API, say), it cannot be the authority, since other people
  and agents act on the destination too. The destination's own check or
  idempotency key is what prevents the duplicate (TOOL-5, TOOL-12,
  HOST-6), and an internal registry is then advisory and need not commit
  atomically with the checkpoint. What remains is purely internal state
  shared on overlapping scopes. Read-only reference data another agent
  type maintains is served by a tool or host-supplied data, unless direct
  shared access is a deliberate, versioned contract.
- The extension path, when a consumer has that need: keep the run's
  default namespace and add further namespaces the agent type declares
  under logical names, which the host binds and the runtime records, fixed
  for the run, with operations in any of them joining the same fenced
  commit (so all must be in the same store). Restricting which tools reach
  which namespace is separate work, needed only for a tool-level boundary;
  giving every tool every namespace adds no protection. Until a consumer
  needs it, the single namespace stands.
- Separate namespaces would not separate every copy of the data: a
  transition's and a result's entries record their operations, values
  included, in the execution record, whose retention is its own.
- The manifest says nothing about namespaces: which namespace a run uses
  is the host's deployment decision, not part of the agent type's
  behavior identity (ADR 0011, item 11).

## References

- [docs/requirements.md](../requirements.md): STORE-2, STORE-3, RUN-1, TOOL-5, TOOL-12, HOST-6,
  RUN-3, MAN-2, REC-1
- [ADR 0005](0005-storage-one-fenced-commit.md), item 7
- [ADR 0007](0007-agent-types-and-the-run-controller.md), items 4, 6, 8,
  10, 13
- [ADR 0008](0008-model-states.md), item 4
- [ADR 0011](0011-manifests.md), item 8
