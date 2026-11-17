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
([ADR 0017](docs/adr/0017-the-standalone-host.md)). The intake agent,
`cmd/addepar`, runs on the standalone host: its verifier and exporter,
and the vertical slice (extraction, verification, crash recovery and
fencing on the file store, [ADR
0018](docs/adr/0018-the-intake-agent-type-and-the-vertical-slice.md)), with the
model reading the PDF itself beside its text ([ADR
0019](docs/adr/0019-the-extraction-agent-reads-the-pdf.md)). Design decisions
are in the [ADRs](docs/adr/README.md).

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
