# Contributing

Contributions are welcome. Bug fixes, Docker API compatibility work, new service emulators, cookbook scenarios, tests, and documentation improvements are all useful.

## Development setup

Requirements:

- Go 1.23 or newer
- Python 3.11 or newer for the integration helpers
- Docker Compose v2 for the Compose compatibility suite
- `golangci-lint` v2 for linting
- `pre-commit` for local hooks

Run the normal validation path:

```bash
make verify
```

Run the Compose compatibility suite:

```bash
COMPOSE_BIN="docker compose" make compose-topology
```

Install local Git hooks:

```bash
pre-commit install
```

## Pull requests

Keep changes focused. Add or update tests when behavior changes.

A pull request should pass:

```bash
make fmt-check
make test
make race
make vet
golangci-lint run ./...
```

For Docker API changes, include a regression test that shows the expected request, response code, and resulting daemon state.

For service-emulator changes, add deterministic coverage for the healthy path and at least one failure path.

## Compatibility policy

`docker-zero` intentionally implements a subset of the Docker Engine API. New endpoints should only be added when they are required by a real client, orchestrator, Docker Compose workflow, or a reproducible test case.

Do not silently accept unsupported Docker operations. Return a clear mock-incomplete error and record it in the ledger.
