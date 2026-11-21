# maestro-agents

A Go foundation library for building agents. Agents built on it run as
standalone executables and can be managed by orchestrators, including
[Maestro](https://github.com/SnapdragonPartners/maestro). LLM access goes
through [maestro-llms](https://github.com/SnapdragonPartners/maestro-llms).

## Status

**Building the MVP** ([docs/MVP.md](docs/MVP.md)). The store interface and
its reference stores (memory and files) exist, in `store/`, and so do agent
types, the run controller for code and model states, budgets, forced
wrap-up with the visible clock, manifests, namespace confinement, skills
and prompt templates, the model client with attempt-level budget
admission, and the host seams for telemetry, logging and credentials, in
`agent/`. The standalone host, which runs an agent type from a JSON
configuration and which every agent's `main` calls, is in `standalone/`
([ADR 0017](docs/adr/0017-the-standalone-host.md)), with in-process
runs, submitted runs and the folder watch for programs that embed it. The
scoring harness, `scoring/`, runs an agent over a set of inputs and
compares its outputs with an answer key ([ADR
0020](docs/adr/0020-the-scoring-harness.md)). The first agent built on
the library, the capital-statement intake agent (`addepar`), now lives
in [morris-agents](https://github.com/SnapdragonPartners/morris-agents),
where each agent is its own Go module; the external contract every host,
agent module and app wires against is [ADR
0024](docs/adr/0024-the-external-contract.md), with its reference in
[docs/contract.md](docs/contract.md). Design decisions
are in the [ADRs](docs/adr/README.md).

## Layout

The library lives at the module root and in its subpackages. Agents
built on it live in their own modules, such as those in
[morris-agents](https://github.com/SnapdragonPartners/morris-agents);
an executable kept here would live under `cmd/<agent>/`.

## Development

Requires Go 1.26 (the toolchain version is pinned in `go.mod`).

```sh
make deps     # install golangci-lint, govulncheck and the pre-push hook
make build    # compile everything; agents build to bin/<agent>
make test     # unit tests with the race detector
make lint     # go vet + golangci-lint
make vuln     # govulncheck
```

Contributor and agent workflow (branching, pre-push review, PR conventions)
is in [CLAUDE.md](CLAUDE.md), also available as `AGENTS.md`.

## License

MIT; see [LICENSE](LICENSE).
