# maestro-agents

A Go foundation library for building agents. Agents built on it run as
standalone executables and can be managed by orchestrators, including
[Maestro](https://github.com/SnapdragonPartners/maestro). LLM access goes
through [maestro-llms](https://github.com/SnapdragonPartners/maestro-llms).

## Status

**Building the MVP** ([docs/MVP.md](docs/MVP.md)). The store interface and
its reference stores (memory and files) exist, in `store/`, and so do agent
types, the run controller for code and model states, budgets, forced
wrap-up with the visible clock, manifests and namespace confinement, in
`agent/`; skills, then the intake agent, come next, each with an
[ADR](docs/adr/README.md).

## Layout

The library lives at the module root and in its subpackages. Agent
executables built on it live under `cmd/<agent>/`.

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
