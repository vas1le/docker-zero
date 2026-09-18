<p align="center">
  <img src="assets/docker-zero.svg" width="144" alt="docker-zero project icon">
</p>

# docker-zero

**Mock Docker Engine for deterministic Docker API, Docker Compose, container orchestration, network, port, DNS, Redis, Nginx, and fault-injection testing without running real containers.**

[![CI](https://github.com/vas1le/docker-zero/actions/workflows/ci.yml/badge.svg)](https://github.com/vas1le/docker-zero/actions/workflows/ci.yml)
[![CodeQL](https://github.com/vas1le/docker-zero/actions/workflows/codeql.yml/badge.svg)](https://github.com/vas1le/docker-zero/actions/workflows/codeql.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.23%2B-00ADD8.svg)](go.mod)

`docker-zero` is a stateful test double for the Docker Engine API. It exposes Docker-compatible HTTP endpoints over a Unix socket and emulates containers, images, networks, volumes, published ports, service discovery, health state, and selected service protocols in memory.

It is intended for testing software that controls Docker: orchestrators, deployment agents, CI/CD tooling, operators, installers, platform services, and failure-recovery logic.

It does **not** create real containers, namespaces, cgroups, image layers, or container processes.

## Why

A real orchestration test can spend most of its time pulling images, starting services, waiting for health checks, and cleaning up infrastructure. That makes failure-path testing slow and expensive.

`docker-zero` keeps the Docker client side real while replacing the daemon and workloads with deterministic state machines:

```text
Docker CLI / Docker Compose / SDK / orchestrator
                       |
                       | Docker Engine HTTP API
                       v
                 docker-zero.sock
                       |
        +--------------+---------------+
        |              |               |
   container state   networks        ports
        |              |               |
        +------- service emulators -----+
                 Nginx / Redis
```

The system under test can continue using `DOCKER_HOST` and the normal Docker API surface.

## Main capabilities

- Docker Engine API mock over a Unix socket.
- Real Docker Compose client compatibility for the tested API surface.
- Stateful container create, inspect, start, stop, pause, unpause, restart, remove, and health behavior.
- Images, networks, volumes, labels, and Compose resource discovery.
- Per-container virtual IP allocation and network-scoped service aliases.
- Fixed and ephemeral host-port publishing with real collision detection.
- Multiple replicas with independent IPs, runtime state, and service state.
- Active-endpoint protection when deleting networks.
- Nginx HTTP service emulation.
- Redis RESP2 service emulation and behavioral master/replica state.
- Deterministic failure scenarios through service cookbooks.
- Ordered JSONL ledgers for Docker calls, service requests, and state transitions.
- Explicit unsupported-operation reporting instead of silently pretending an API exists.

## Quick start

Build:

```bash
git clone https://github.com/vas1le/docker-zero.git
cd docker-zero
make build
```

Start the healthy baseline:

```bash
./dist/docker-zero-linux-amd64 \
  --socket /tmp/docker-zero.sock \
  --seed 0 \
  --ledger-dir ./runs
```

Point a Docker client or orchestrator at it:

```bash
export DOCKER_HOST=unix:///tmp/docker-zero.sock
```

For example:

```bash
docker compose up -d
docker compose ps
```

Compose itself is not reimplemented. The real Docker Compose client parses `compose.yaml` and calls the mock Docker Engine API.

`--container-endpoints auto` is the default. In this mode docker-zero attempts direct virtual container-IP listeners where the host permits them and still provides published-port listeners. `--container-endpoints on` is strict: if a virtual endpoint cannot bind, startup fails. On Linux, strict emulation of container ports below 1024 requires the host to allow unprivileged low-port binding or the process to have the corresponding privilege/capability.

Archive upload, download and stat (`PUT`/`GET`/`HEAD /containers/{id}/archive`)
are explicitly unsupported (HTTP 501 with the unsupported marker). Volume metadata
is not a container filesystem, and `docker cp` cannot be validated by this simulator.

## Exec fixtures

Exec never runs a host process. By default, commands are rejected with HTTP 501
and `X-Docker-Zero-Unsupported: true`, rather than returning a fabricated exit 0.
An external cookbook can declare exact argument-vector matches in each seed:

```json
"exec": [
  {"cmd": ["sh", "-c", "exit 42"], "stdout": "", "stderr": "migration failed\n", "exit_code": 42}
]
```

Fixture results complete synchronously on exec start. Attached stdout and stderr
use separate Docker stream frames; inspect reports the configured exit code.
Detached start returns no output. Exec requires a running, unpaused container at
both create and start, and an instance can only run once. TTY, stdin, environment,
user, working-directory and privileged exec options are explicitly unsupported.
These fixtures test a controller's handling of an outcome, not the command itself.

## Scenarios

Cookbooks live under `cookbooks/` and define deterministic behavior for a service type.

The shipped convention is:

```text
seed 0 = healthy baseline
seed 1 = transient failure / recovery
seed 2 = crash / restart path
```

Run one global scenario:

```bash
./dist/docker-zero-linux-amd64 --seed 2
```

Override a specific service or container:

```bash
./dist/docker-zero-linux-amd64 \
  --seed 0 \
  --scenario nginx=2,redis-zero=1
```

## Validation

Configuration check:

```bash
./dist/docker-zero-linux-amd64 --check --cookbook-dir ./cookbooks
```

Core tests and quality gates:

```bash
make verify
```

`make verify` includes formatting, pinned `golangci-lint`, unit tests, the race detector, `go vet`, Python syntax checks, Docker API integration, doctor checks, and the orchestration matrix harness.

Compose compatibility:

```bash
COMPOSE_BIN="docker compose" make compose-topology
```

Extended local stress pass:

```bash
make retest
```

## Development quality gates

The repository uses:

- `gofmt` and `go vet`;
- `golangci-lint` v2;
- Go race detector;
- Go fuzz tests;
- `govulncheck`;
- CodeQL;
- Dependabot for Go modules and GitHub Actions;
- `pre-commit` hooks for contributor-side checks;
- GitHub Actions for tests, Compose compatibility, cross-architecture builds, and releases.

Install local hooks:

```bash
python3 -m pip install pre-commit
pre-commit install
```

## Versioning and releases

The project uses semantic versioning.

`VERSION` contains the source release version. Release tags use the same value with a `v` prefix:

```text
VERSION: 0.4.2
tag:     v0.4.2
```

The release workflow rejects a tag that does not match `VERSION`, then runs validation and builds static Linux `amd64` and `arm64` binaries with SHA-256 checksums.

## Image-to-service matching

Cookbook `image_names` select services by image repository, never by container
name or substring. For example, `redis` covers its tags and digests and the
`docker.io/library/redis` alias, but not `myorg/redis` or `notredis`. Register
private image repositories explicitly. A tagged/digest mapping takes precedence
over a bare repository mapping; equally specific mappings to different cookbooks
are rejected as ambiguous.

## Container configuration fidelity

Create requests retain user, working directory, entrypoint, healthcheck, restart
policy, and additional configuration for inspection. Meaningful settings that the
simulator cannot execute (such as filesystem mounts, resource limits, or a custom
health command) return a create warning and `X-Docker-Zero-Unsupported: true`, even
when the metadata is accepted with HTTP 201. Inspect repeats the marker and lists
these fields under `DockerZero.MetadataOnly`. The matrix runner classifies such a
run as **mock_incomplete**, not a passing compatibility test.

A custom healthcheck is never replaced by the built-in healthy result: its command
is stored, but `State.Health` is omitted because it was not executed. A `NONE`
healthcheck disables health reporting; omitted/null healthchecks inherit the
cookbook fixture. Omitted restart policy defaults to `no`.

## Restart policy and recovery tests

The default/`no` policy leaves a crashed container exited until an explicit
start or restart. Seeded automatic recovery requires `always`, `unless-stopped`,
or `on-failure`; the latter only retries nonzero exits and enforces a nonzero
`MaximumRetryCount`. Manual stop/kill suppress automatic recovery until another
explicit start/restart. Explicit starts reset the retry budget, not the total
restart count.

Recovery timing is still **observation-driven fixture timing**: eligible cookbook
edges advance on inspect/list requests. This is not Docker's real-time backoff,
success-duration reset, or daemon-restart/persistence behavior. To test controller
recovery, use policy `no` and assert the controller's actual restart/rollback calls
in the ledger; do not count policy-driven fixture recovery as controller success.

## Current scope

`docker-zero` is not a general-purpose container runtime and does not aim to implement every Docker feature.

Not implemented as real kernel/runtime features:

- namespaces and cgroups;
- image filesystem execution;
- process execution inside containers;
- a real Docker embedded DNS server at `127.0.0.11`;
- Redis persistence or Redis wire-level replication;
- Docker event streaming (`GET /events`);
- Docker disk-usage accounting (`GET /system/df`);
- arbitrary Engine endpoints not required by a tested client or compatibility case.

When a Docker API operation is unsupported, the mock reports it explicitly with `X-Docker-Zero-Unsupported: true` and records the request in the ledger.

## Open source

`docker-zero` is intended to be a reusable open-source test tool. Fork it, modify it, embed it in test infrastructure, add protocol emulators, or use it in commercial and non-commercial projects under the MIT License.

## Contributing

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md).

Good contribution areas include Docker API compatibility, additional deterministic failure modes, new service emulators, Compose behavior, fuzzing, and regression tests based on real orchestration failures.

## License

MIT. See [LICENSE](LICENSE). You can use, modify, redistribute, and include `docker-zero` in commercial or non-commercial projects under the license terms.

## Search terms

Docker mock, mock Docker Engine, fake Docker daemon, Docker API emulator, Docker Compose mock, container runtime simulator, Docker testing, orchestrator testing, container orchestration testing, fault injection, Docker network mock, Docker DNS mock, Redis mock, Nginx mock, integration testing, DevOps testing, DevSecOps testing.
