# maestro-agents Repository Guide

This file guides Claude Code and other coding agents (Codex reads it as
`AGENTS.md`, a symlink to this file) working in this repository.

## Project

`maestro-agents` is a Go foundation library for building agents. The agents
built on it are both **standalone executables** and **manageable by
orchestrators**, including Maestro and Morris's planned orchestration layer.

- Module path: `github.com/SnapdragonPartners/maestro-agents`
- Go 1.26 (toolchain pinned in `go.mod`)
- LLM access goes through `maestro-llms`; `maestro-cms` may be used for
  content and retrieval
- Agent executables live in this repository under `cmd/<agent>/`, beside the
  library

Current state: **MVP, step 2 (the vertical slice)**. The store interface,
the memory and file stores and the conformance suite exist (`store/`), and
so do agent types, the run controller for code and model states,
budgets, forced wrap-up with the visible clock, manifests, namespace
confinement, and skills and prompt templates (`agent/`); budget admission
through `maestro-llms`' limiter, then the intake agent, come next. The requirements are in
[docs/requirements.md](docs/requirements.md), and what the MVP builds
of them in [docs/MVP.md](docs/MVP.md). The run model, execution record
and storage design are ADRs 0003 to 0006, the runtime's API ADRs 0007 to
0013; further design arrives with the code, each piece with its ADR.

Load-bearing decisions so far:

- An agent instance executes serially, with one execution owner; parallelism
  comes from separate instances, and later from subagents
  ([ADR 0002](docs/adr/0002-serial-execution-within-an-agent-instance.md))
- A run has no stored `running` status: executing and ready are derived
  from the run's status and the instance's ownership lease; an activation
  returns an outcome only when its commit is confirmed, and otherwise an
  error, after which the host rereads the run record before retrying
  ([ADR 0003](docs/adr/0003-run-model-identities-and-activation-results.md))
- The execution record is a per-run write-ahead log; a checkpoint is its
  snapshot at each step boundary, and recovery replays at most one step
  ([ADR 0004](docs/adr/0004-execution-record-and-recovery.md))
- Every durable write an activation makes goes through one fenced, atomic
  commit; atomic-local-transaction tools return operations rather than
  writing; the library does not encrypt at rest
  ([ADR 0005](docs/adr/0005-storage-one-fenced-commit.md))
- The store interface is `store.Store` (committer, control plane, reader);
  both reference stores run on one engine (`internal/storecore`), and a
  host store proves itself with `store/storetest`
  ([ADR 0006](docs/adr/0006-store-interface.md))
- An agent type is `agent.Type[D]`, a state machine over its own data
  type; `Controller.Activate` runs a claimed run, and a code state's only
  durable output is its transition with object operations, committed with
  the checkpoint; a code state's defect fails the run with `state_error`
  ([ADR 0007](docs/adr/0007-agent-types-and-the-run-controller.md))
- A model state is `State.Model`: a prompt and tools over `toolloop.Run`
  with the host's client from `ActivationInput.Client`; each attempt, turn
  and call (permission, attempt, result) is recorded write-ahead, a tool's
  result can transition, recovery resolves a recorded turn's calls without
  asking the model again, and TEST-5 runs in `storetest`
  ([ADR 0008](docs/adr/0008-model-states.md))
- Budgets are `ActivationInput.Budgets`, recorded with the run's start
  and fixed for the run; the recording client admits each turn (a logical
  call, counted even if it fails or is interrupted) and its tokens,
  recording the reservation in `model_attempt`; until `maestro-llms#149`
  a hard token budget fails closed with `budget_unenforceable`, so token
  budgets are estimated; the clocks cancel the activation's context;
  consumption is checkpointed and rebuilt from the tail
  ([ADR 0009](docs/adr/0009-budgets.md))
- Every model state declares `WrapUp.Tools`, its exits: when a time
  budget's remaining is within the state's reserve, or the turns left
  within its wrap-up turns, the next turns offer only those tools (tool
  choice auto: some providers reject a forced one), with `Call.WrapUp`
  set, and a model call in flight at that point is cut off; the bound
  spent fails the run with `wrap_up_failed`; each tool result that does
  not transition ends with the budget status, recorded as sent (refusals
  and denials carry none)
  ([ADR 0010](docs/adr/0010-wrap-up-and-the-visible-clock.md))
- An agent type is its manifest: it declares its trigger kinds, its
  configuration's JSON Schema (a subset, validated by
  `internal/jsonschema`; other keywords are refused at registration) and
  default budgets; `Registry.Manifest` returns it with each tool's content
  identity (definition, effect, contract and `Tool.Version`, defaulting to
  the type's), which the permission records and recovery enforces; a run refused for its trigger kind, configuration or
  budgets, or a `pending` verdict, fails with `config_error`; `run_started`
  records the manifest's digest and the trigger kind
  (`store.Run.TriggerKind`)
  ([ADR 0011](docs/adr/0011-manifests.md))
- A run has exactly one object-store namespace, `ActivationInput.Namespace`,
  required, recorded in `run_started` and the checkpoint and fixed for the
  run; agent code never names one: `Env.Object`/`ListObjects` read it and
  `Env.PutJSON`/`PutBytes`/`Delete` build operations in it, and an
  operation outside it fails the run (`state_error` from a code state,
  `tool_error` from a tool); a missing namespace, or one the store refuses,
  fails the start with `config_error`
  ([ADR 0012](docs/adr/0012-namespace-confinement.md))
- Skills are `Type.Skills`, an `fs.FS` of `<name>/SKILL.md` (strict
  `name`/`description` header) plus reference files, and templates
  `Type.Templates`, an `fs.FS` of `text/template` files; both are read,
  checked and hashed at registration and listed in the manifest with
  their identities; a model state's `Skills` grant offers the built-in
  `read_skill` tool, whose description indexes them (not the system
  prompt) and whose identity follows their content; `Env.Render`
  renders a template
  ([ADR 0013](docs/adr/0013-skills-and-prompt-templates.md))

### Related repositories

Sibling checkouts live next to this one (`../<name>`):

| Repository | Relationship |
|---|---|
| `maestro-llms` | Dependency: provider-neutral chat, embeddings, tool loop, limiter |
| `maestro-cms` | Possible dependency: content ingestion, chunking, embedding, retrieval |
| `buggrit` | Source of elements to incorporate; also reviews this repo's PRs |
| `maestro` | Source of elements to incorporate; an orchestrator that manages agents |
| `morris` | Its planned orchestration layer will manage agents; this workflow follows Morris's |

What is taken from `buggrit` and `maestro`, and how, is decided per piece
in an ADR when that work starts. Code is not copied wholesale: each piece
is brought in deliberately, fitted to this library's API, and its
differences from the original recorded.

## Durable Rules

- Create a branch and pull request for all code changes, and follow the
  **Branch And Review Workflow** below: **review before push**, and Claude
  never merges. Every human approval in this guide may come from any human
  operator; GitHub credentials for the repository are what qualify one.
  After a branch's first push, review fixes are pushed without asking once
  the tests and Codex pass, whichever reviewer raised them; a human
  operator merges
- Never bypass hooks with `--no-verify`; fix failures. The only exception,
  **push only**: a documentation-only push (zero code changes) that a human
  operator confirms for that specific push (approval is not reusable; one line of
  code voids it)
- Prefer Go standard library patterns over framework-heavy choices
- If a new technical decision materially affects security, isolation, data
  handling, the public API, the orchestrator boundary or long-term
  architecture, capture it in an ADR ([docs/adr](docs/adr/README.md))

## Dependency Discipline

Prefer the Go standard library where practical. Avoid adding third-party
dependencies for simple functionality: as a rule of thumb, if the needed
behavior can be recreated clearly in roughly 200 lines of straightforward,
well-tested Go, implement it locally rather than importing another package.
The goal is not minimalism for its own sake; it is to keep the import tree
small, auditable and easy to reason about. That matters more here than in
an application: every dependency of this library becomes a dependency of
every agent and every orchestrator that imports it.

A third-party dependency is appropriate when **both**:

- the domain is complex or risky to implement correctly (protocol
  implementations, cryptography, database drivers, cloud SDKs); and
- the package is mature, maintained and well-used, and materially reduces
  risk.

When adding a dependency, state in the PR body why it meets this bar, and
confine it to an adapter package rather than the core where possible.
Shared SnapdragonPartners libraries (`maestro-llms`, `maestro-cms`) are
first-party and exempt from the "well-used third-party" test; how far their
types may reach into this library's public API is an ADR decision. LLM
provider SDK versions are owned by `maestro-llms`; this repository inherits
them and does not pin or bump them independently.

## Directory Layout

```
doc.go               # Root package `agents`: package documentation
agent/               # Agent types, registry, manifests, run controller, model states, budgets, wrap-up, namespaces, skills, templates, record entries (ADR 0007-0013)
store/               # Store records, operations, errors and interface (ADR 0005, 0006)
store/memstore/      # In-memory reference store
store/filestore/     # File reference store: JSON files, journal, directory lock
store/storetest/     # Store conformance suite, TEST-5 and TEST-6 included
internal/storecore/  # The engine both reference stores share
internal/jsonschema/ # The JSON Schema subset validator for configuration schemas (ADR 0011)
cmd/<agent>/         # (planned) Standalone agent executables
docs/requirements.md # Library requirements (input to the design ADRs)
docs/MVP.md          # MVP scope: what the MVP builds, what is deferred
docs/adr/            # Architecture Decision Records
hooks/               # Git hooks (installed by `make install-hooks`)
.github/             # CI, CodeQL, buggrit, Dependabot
```

Library packages will sit at the module root or in subdirectories as the
API takes shape; update this section in the PR that adds them.

## Makefile Targets

- `make build` — Compile all packages, and build each `cmd/<agent>` to `bin/<agent>`
- `make test` — Unit tests with the race detector and coverage (no network, no keys)
- `make test-integration` — Tests behind the `integration` build tag (may need network or keys; each skips without its prerequisites)
- `make lint` — `go vet` + `golangci-lint run` (read-only; also checks formatting and imports)
- `make vuln` — `govulncheck ./...`
- `make test-coverage` — HTML coverage report in `coverage/`
- `make tidy` — `go mod tidy`
- `make deps` — Check Go, install golangci-lint, govulncheck and the git hooks
- `make install-hooks` — Install the pre-push hook (`make lint` + `make test`)
- `make clean` — Remove build artifacts

Tool versions are pinned in the Makefile and in `.github/workflows/ci.yml`;
keep them in sync. For local work against unreleased sibling changes, use a
`go.work` file (ignored by git); never commit a `replace` directive.

## Branch And Review Workflow

Substantive work follows a **review-before-push** loop: the heavy review
happens on the local branch *before* the push, not after. Post-push
automated checks are a lighter final proofread.

**Roles:**

- **Claude** authors: docs, ADRs, plans, specs, code and fixes.
- **Codex** is the primary reviewer; it reviews committed local artifacts and code **before push** (this is where the heavy lifting happens).
- **A human operator** orchestrates, resolves contention, accepts and merges. Any human with GitHub credentials for the repository is a human operator; those credentials are the qualification and the enforcement. A human operator authorizes the external effects: a branch's first push, opening a PR, commenting on GitHub and merging. After the first push, fixes for review findings are pushed without asking once the tests and Codex pass (step 9).
- A PR is **Accepted** only after both Codex and a human operator approve.
- Claude↔Codex communication goes through the **Counterpoint** review tool (`mcp__counterpoint__review`) when it is available in the session; otherwise it routes through the human operator manually. Do not ask for a relay when the tool is available.

If Claude and Codex do not converge after reasoned attempts, preserve both
positions and escalate the decision to a human operator rather than
cycling indefinitely.

**Steps:**

1. Work on a branch off `main` (`feat/<slug>`, `fix/<slug>`, `docs/<slug>`, `chore/<slug>`). Never work directly on `main`.
2. Make the scoped change and run verification proportionate to its risk: for code, `make lint`, `make test`, and `make test-integration` (integration tests run locally; do not punt them to CI); for documentation alone, `make lint` (the pre-push hook runs `make test`).
3. Commit **locally**. (The only git hook is pre-push, `make lint` + `make test`; there is no pre-commit hook.) Never bypass it with `--no-verify` except per the documentation-only push exception in Durable Rules.
4. Write branch notes (see *Branch notes* below) and submit the exact local commit for review through Counterpoint (see *Submitting for review*).
5. Address every finding with a fix or a reasoned response Codex accepts. Commit each review round locally and submit the new commit with updated notes.
6. Continue until Codex has no open findings, a human operator explicitly overrides one, or Claude has escalated a disputed finding to a human.
7. **Stop for a human's approval.** Quote Codex's approval (or remaining findings) to the human operator you are working with. **Do not make a branch's first push before Codex and a human operator both approve; that push is a human gate, not a routine step.**
8. After authorization, push and open the PR to `main` per the PR Body Conventions below. The **post-push** automated checks (CI, GitHub code scanning via CodeQL, and a [buggrit](https://github.com/SnapdragonPartners/buggrit) review, which runs automatically on every push to a PR) are a lighter **final proofread**: treat their findings substantively, but the heavy review has already happened pre-push. buggrit (`.github/workflows/buggrit.yml`, configured by `.github/buggrit.json`) posts each finding as its own thread and re-checks its findings on every push. Its pass is finished when the `buggrit / review` check completes; a pass can take up to about an hour.
9. Address every post-push review thread with a fix or a reasoned reply. Push any fix once the verification step 2 names for it and Codex pass, without asking a human, whichever reviewer raised the finding (buggrit, code scanning, Copilot, a human). Reply to the thread and mark it resolved (except a buggrit thread you fixed, which stays open for buggrit to verify), then check again for new feedback (see *Post-push reviewer feedback* below). Every buggrit finding gets a response, not necessarily a fix (see *buggrit findings* below).
10. **A human operator gives final approval and merges. Claude never merges**, not even an approved, green PR, unless a human operator explicitly says to.

Keep at most **one feature/development branch open at a time**; parallel
branches are allowed only for bug fixes, or when a human operator
explicitly authorizes parallel work.

### Responding to findings

Codex is a reviewer, not an authority, but **every Codex finding, whatever
its severity, is resolved before pushing**. Severities:

- **P0** — data loss, corruption, critical security exposure, or an unusable result.
- **P1** — a concrete correctness, robustness, architectural or maintainability defect.
- **P2** — a real issue that doesn't make the result unusable, such as documentation drift, a missing edge-case test, or a misleading name or comment.
- **P3** — a minor nit.

These follow buggrit's review prompt
([`internal/review/prompt.go`](https://github.com/SnapdragonPartners/buggrit/blob/v0.6.0/internal/review/prompt.go),
as of v0.6.0), so a severity means the same from either reviewer, with one
deliberate wording difference: buggrit's prompt calls a P2 "a real issue
that doesn't block merge", while here a P2 is blocking, as defined below. A
finding is handled by the severity its reviewer gave it; where a buggrit P2
reads as a nit, say so in the reply, and defer it rather than relabel it (a
Codex finding is never deferred unilaterally; see *buggrit findings*).

For Codex every severity is resolved before pushing. For buggrit, P0–P2 are
blocking and P3 advisory (see *buggrit findings* below), where "blocking"
means a finding is fixed, or deferred to tracked work, before the PR
merges; it is never just dismissed.

A finding is resolved by a fix, or by a reasoned response in the next
round's notes that Codex accepts. A finding Claude judges wrong, out of
scope or disproportionate gets that response rather than a change: cite the
ADR, an existing test, or the deliberate design choice. When pushback is
correct on the merits, add a regression test alongside it. If Codex
reaffirms a disputed finding, **stop**, state both positions, and bring the
decision to a human operator before another round. Do not implement a
change Claude believes is wrong in order to end the loop.

**A repeated finding is a class.** When two or three findings, from any
reviewer, share one shape (the same trust boundary, the same missing check),
stop patching instances: name the class, fix it at one choke point with a
test per case, and say so in the branch notes. Then ask Codex, in that
round's notes, to enumerate every input at that boundary and treat any
unhandled case as blocking, rather than reviewing only the delta.

### Submitting for review

- Call `mcp__counterpoint__review` directly with the absolute repository path (derive it from the current checkout with `git rev-parse --show-toplevel`; never hard-code it), the branch, the exact local commit, and the branch notes. The commit must be the branch tip and the checked-out HEAD of a **clean** worktree, so commit before calling. The call blocks for the whole Codex turn, typically several minutes (up to ~21 minutes worst case).
- In a **read-only** review (default) the reviewer reads the worktree, so **do not touch the worktree while the review runs**. In a **build-capable** review (`build: true`) the reviewer works in a disposable, offline checkout of the commit; the only obligation is not to rewrite the branch meanwhile.
- Ask for a build-capable review when a test run is material evidence: code changes, especially to concurrency, the agent lifecycle, the orchestrator protocol, tool execution, or anything that touches a trust boundary. Keep documentation-only rounds and small follow-ups read-only. The build checkout runs with network disabled, so `make test-integration` (which may need network or keys) cannot run there; its results are the author's evidence: record the command and outcome in the notes.
- The reviewer starts each round fresh; its memory is the last three rounds' verdicts, which Counterpoint quotes into the prompt. Write later rounds' notes as a delta, but **keep the disposition of every prior finding in each round's notes**, since older rounds are evicted and the reviewer is told to re-validate rather than trust its earlier output. A new or renamed branch starts a new review thread.
- If the tool reports the thread is unavailable, stop and tell the human operator rather than working around it; the error says what to do (usually archive the thread in the Codex app so the next call can take it over).
- If the tool is not available in the session, write the branch notes and ask the human operator to submit them to Codex manually, then wait for the relayed findings.
- Treat the tool result as Codex's review: address every finding as above, and quote the approval or the findings to the human operator when stopping at the push gate.

### Branch notes

Branch notes are the handoff artifact, not a generic summary. Codex verifies
the notes against the commit and repository rather than trusting the
author's claims, so verification claims must be backed by the commands and
outcomes in the notes. Every round's notes **lead with the exact commit hash
and the branch's push state** (`not pushed`), then include:

- review round number;
- whether the change is code, documentation, or both;
- verification commands and outcomes (what step 2 names for the change, plus any manual QA);
- a concise account of material changes and why;
- design references (ADRs) and the controlling sentence for any non-obvious decision;
- consumer impact: anything an agent or orchestrator using this library must change;
- resolution of each prior review finding (fixed in commit X / reasoned pushback / deferred with reason);
- important design choices and rejected alternatives;
- known limitations, intentional gaps (what this PR does NOT yet do), and open questions; and
- branch status: local commit count when useful, and confirmation that it is unpushed and awaiting approval.

The same notes seed the PR body once push is authorized.

## Testing And Merge Discipline

- New feature work must include robust tests
- Do not treat failing tests as unrelated just because the connection is not obvious
- Failing tests on a branch are treated as part of that branch until resolved
- We do not waive failures by claiming they are unrelated
- We fix the code or we fix the test
- A branch is not ready to merge while relevant unit or integration tests are failing
- Prefer **one feature PR in flight at a time**: land (review → merge) the current PR before opening the next one that builds on it. When a change is genuinely large, still split it, but merge the pieces in sequence, or keep them touching disjoint files so they are truly independent. A docs-only companion may ride along with its code.

## Pull Request Body Conventions

PRs are reviewed by humans and by AI tooling (Codex, buggrit, Copilot).
**AI reviewers see only the diff and the PR body**: no commit history, no
conversation context, no local files. The PR body has to carry enough
design context for the reviewer to judge intent:

1. **Summary**: what changed and why, in plain language.
2. **Design references**: clickable links to every relevant ADR (`docs/adr/NNNN-*.md`). When a PR implements an ADR amendment or a non-obvious decision, **quote the controlling sentence directly in the body** so the reviewer doesn't have to fetch the file.
3. **Multi-PR context**: when the change is part of a sequence, list prior PRs by number and call out explicitly what this PR does NOT yet do, so intentional gaps are not flagged as bugs.
4. **Consumer impact**: API or behavior changes an agent or orchestrator must adapt to.
5. **Test plan**: the results of what step 2 of the Branch And Review Workflow names for the change, plus any manual QA still pending.

End the body with the Claude Code attribution line; end commits with the
`Co-Authored-By` trailer.

### Post-push reviewer feedback

Apply the same standard as *Responding to findings* above: agree where the
reviewer is right; push back with concrete reasoning where they are not,
citing the ADR, an existing test, or the deliberate design choice. When
pushback is correct on the merits, add a regression test alongside it. When
a finding identifies a real bug, fix it in a dedicated commit ("Address PR
#N review feedback") rather than bundling it into unrelated work.

**After pushing the fixes, resolve the corresponding threads on GitHub**:
reply to each review thread noting the commit that addressed it, then mark
it resolved (GraphQL `resolveReviewThread`); a summary comment is not
enough. Resolving threads is how reviewers confirm each point was handled.

**buggrit's threads are the exception.** Resolving a buggrit thread settles
the finding: buggrit stops checking it. So to fix one, push the fix and
leave the thread open; buggrit verifies the fix on the next push and
resolves the thread itself. Resolve a buggrit thread only when not fixing
it, after replying with the reason. Close any GitHub issues a change closes
once the fix lands.

**buggrit findings: every one gets a response.** **P0–P2 are blocking; P3s
are advisory**, but P3s have turned up substantial correctness issues
elsewhere, so they are read, not ignored. Each fix and recheck costs up to
an hour, so for each finding judge whether another cycle is worth it: the
question is whether you are in a cycle of diminishing returns on minor
issues. The responses:

- **Fix it.** Push the fix once the tests and Codex pass, without asking a human first (step 9), reply naming the commit, and leave the thread open for buggrit to verify.
- **Deferred.** Not worth another long cycle now: reply saying where it goes (a tracking issue, linked, or the next PR in the arc) and resolve the thread. A blocking (P0–P2) finding that isn't fixed is deferred this way, never just dismissed.
- **Won't fix.** For an advisory (P3) finding not worth a change: reply with the reason and resolve the thread.
- **Challenged.** A finding that is wrong at any severity: reply with the reasoning (the ADR, an existing test, the deliberate design choice) and resolve the thread. A real defect still gets fixed or deferred.

None is left unanswered. When fixes for minor findings keep drawing new edge
cases, that is the signal to defer or won't-fix rather than run another
cycle. For a Codex finding there is no unilateral defer or won't-fix: the
same weighing goes into the next round's notes as the reasoned response,
and if Codex reaffirms the finding a human decides (*Responding to
findings*). Weigh an edge case by what it can do: one that threatens
stability or security is dealt with; a cosmetic or unrealistic one matters
less. This is not *A repeated finding is a class*: that rule is for a real
defect recurring at one boundary, which gets one fix at a choke point.

## Project Skills

`.claude/skills/` holds guidance for a particular kind of work here, loaded
when that work starts. **It is committed**: the rest of `.claude/` is
machine-local, but a skill is for everyone working in this repository.
There are none yet. Adding one: a directory under `.claude/skills/` with a
`SKILL.md` carrying `name` and a `description` that says *when to use it*.
Keep it pointing at the canonical document rather than restating it; a
skill that duplicates a doc will drift from it.
