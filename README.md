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

In a second terminal, from the repository root, run the included `compose.yaml`.
It creates an Nginx HTTP fixture and a Redis fixture; no real containers run.
A Docker CLI with the Compose plugin is required, but no Docker daemon is needed.
Scope `DOCKER_HOST` to each command so this demo cannot target your real daemon:

```bash
DOCKER_HOST=unix:///tmp/docker-zero.sock docker compose -p docker-zero-demo up -d
DOCKER_HOST=unix:///tmp/docker-zero.sock docker compose -p docker-zero-demo ps
curl --fail http://127.0.0.1:18080/health
# Optional, when redis-cli is installed:
redis-cli -h 127.0.0.1 -p 16379 PING
DOCKER_HOST=unix:///tmp/docker-zero.sock docker compose -p docker-zero-demo down
```

The ports bind only to `127.0.0.1`. If either is occupied, set
`DOCKER_ZERO_HTTP_PORT` and/or `DOCKER_ZERO_REDIS_PORT` in that terminal before
running Compose, and use those ports in the probe commands. Stop the fixture
engine with Ctrl-C when finished. Compose itself is not reimplemented: the real
Compose client parses the file and calls the mock Docker Engine API. The
`make compose-topology` suite exercises this exact example as well.

`--container-endpoints auto` is the default. In this mode docker-zero attempts direct virtual container-IP listeners where the host permits them and still provides published-port listeners. `--container-endpoints on` is strict: if a virtual endpoint cannot bind, startup fails. On Linux, strict emulation of container ports below 1024 requires the host to allow unprivileged low-port binding or the process to have the corresponding privilege/capability.

## Scenarios

Cookbooks live under `cookbooks/` and define deterministic behavior for a service type.
Their `image_names` entries match exact image repositories (all tags/digests) or
explicit tag/digest references. Container names never select a service model.
Private or custom image names require an explicit `image_names` entry; substring
matches such as `company/not-nginx` are deliberately rejected.

The shipped convention is:

```text
seed 0 = healthy baseline
seed 1 = transient failure / recovery
seed 2 = crash / policy-controlled restart path
```

After a seed-2 crash, the default policy `no` leaves the container exited until
an explicit start/restart request. To test a **daemon-policy fixture**, request
`HostConfig.RestartPolicy.Name: "always"`, `"unless-stopped"`, or `"on-failure"`.
Inspect/list calls can step that permitted automatic restart; this does not prove
that a deployment controller issued a recovery action. `on-failure` respects
nonzero exit status and `MaximumRetryCount`. Manual stop/kill suppresses automatic
recovery until an explicit start/restart. Timing/backoff and daemon-reboot policy
semantics are not emulated.

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

## Current scope

Versioned requests must be within the advertised API range (`1.24`–`1.43`).
Out-of-range or malformed numeric versions are rejected before any operation.
Unversioned `/_ping` and `/version` remain available for client negotiation.
This range describes accepted request versions, not complete implementation of
every feature in those Docker API versions.

`kill` simulates SIGKILL only (the default, `9`, `KILL`, or `SIGKILL`). Other
signals return an explicit unsupported error rather than silently killing the
container. Killing a non-running container returns a conflict without changing
its exit code.


`docker-zero` is not a general-purpose container runtime and does not aim to implement every Docker feature.

Not implemented as real kernel/runtime features:

- namespaces and cgroups;
- image filesystem execution or archive copy/stat operations (`GET`, `PUT`, and `HEAD /containers/{id}/archive` return HTTP 501 with the unsupported marker);
- process execution inside containers (`exec` returns HTTP 501 with the unsupported marker, never a fabricated exit code);
- a real Docker embedded DNS server at `127.0.0.11`;
- Redis persistence or Redis wire-level replication;
- Docker event streaming (`GET /events`);
- Docker disk-usage accounting (`GET /system/df`);
- arbitrary Engine endpoints not required by a tested client or compatibility case.

Container creation preserves the supplied configuration, including healthcheck definitions,
user, working directory, entrypoint, and restart policy. Retaining a field does **not**
mean docker-zero executes or enforces it: creation `Warnings` and inspect's
`DockerZero.MetadataOnlyFields` identify metadata-only fields. Health is driven by
the cookbook, not by executing the requested healthcheck command; `Test: ["NONE"]`
omits `State.Health`. The default restart policy is `no`.

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
