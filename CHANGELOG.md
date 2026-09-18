# Changelog

## v0.4.2 — 2026-09-12

External-review hardening and quality-gate correction.

- Made `golangci-lint` part of `make verify`, so local verification, CI, and tagged releases enforce the same lint gate.
- Resolved the complete pinned `golangci-lint v2.12.2` issue set without disabling the configured linters.
- Added explicit test cleanup helpers instead of ignoring close/shutdown errors in tests.
- Preserved the last Docker health state and failing streak across a clean container stop.
- Made container network reconfiguration transactional: allocation failure cannot partially detach or attach endpoints.
- Fixed short container-ID MAC generation bounds.
- Added an explicit AF_UNIX socket-path length diagnostic instead of leaking platform `bind: invalid argument` errors.
- `/events` and `/system/df` now report machine-classified unsupported operations rather than returning fabricated success.
- Added Docker pause/unpause behavior and rejects exec while a container is paused.
- Removed confirmed dead code and split the engine implementation into focused source files.
- Snapshot container-list metadata under the container lock to remove inconsistent concurrent reads.
- Added regression tests for the externally reported behavior defects.
- CI now verifies format, unit tests, race detector, vet, Python syntax, pinned golangci-lint, govulncheck, Docker API integration, real Docker Compose topology, cross-architecture builds, and CodeQL.

## v0.4.1 — 2026-09-03

Collision and destructive-network semantics hardening.

- Added direct Docker API regression coverage for deleting user-defined networks with running or stopped attached containers.
- Network removal with active endpoints now returns a daemon/server error and leaves the network, endpoint membership, DNS, container state, and service listener unchanged.
- Pre-defined `bridge` removal remains a distinct `403` forbidden operation.
- Active-endpoint diagnostics list attached container names deterministically to make orchestration failures actionable.
- Added global container-name collision regression coverage; a rejected create cannot replace the original container.
- Added duplicate network-name collision regression coverage; the original network and labels are preserved.
- Duplicate `network connect` of an existing endpoint is now rejected instead of silently succeeding, without interrupting a running service.
- Added real Compose cross-project collision tests for fixed published ports and global `container_name`.
- Added a real Compose cleanup-collision test where a foreign running container is attached to another project's network: `compose down` may remove its own container but cannot remove the still-used network or affect the foreign container.
- Documented Compose v5.5.0 behavior where the active-network resource error is printed as `Resource is still in use` while the command can still exit 0; tests verify daemon state rather than relying on the Compose exit code.

## v0.4.0 — 2026-09-02

Topology/runtime hardening for scaled Compose workloads.

- Replaced kind-global protocol endpoints with per-container runtime objects.
- Added unique virtual IP allocation per container and per user-defined network.
- Added Docker/Compose-driven `HostConfig.PortBindings` listeners.
- Added kernel-assigned ephemeral published ports and stable stop/start reuse.
- Added Docker-style fixed-port conflict failures for both fake-container and real host-process conflicts.
- Added network-scoped service/container alias resolution and a diagnostic `/__docker_zero/dns` endpoint.
- Added Docker network connect/disconnect membership, alias, IP, and DNS updates.
- Added independent scaled replicas; verified with a real 80-replica Compose project.
- Added Redis master/replica behavioral simulation with DNS discovery, `INFO replication`, `ROLE`, `REPLICAOF`/`SLAVEOF`, mutation propagation, READONLY replicas, and link down/up recovery.
- Made explicit Docker restart synchronous with runtime rebind, so a successful restart response means the published endpoint is live again.
- Updated container list/inspect documents to report the actual assigned networks, addresses, exposed ports, and published bindings.
- Added real Docker Compose topology regression suite and topology-specific Go tests.
- Fixed a runtime lifecycle synchronization defect discovered under concurrency testing (`sync.WaitGroup` reuse during overlapping runtime shutdown/start).
- Updated `--check` and CLI help to describe Docker/Compose-driven topology rather than obsolete static endpoints.
- Reuse released virtual subnet indices and container IPs so repeated create/remove cycles do not cause fake-only address-space exhaustion.
- Reject same-container duplicate host-port ownership across different container ports and roll back partial ephemeral assignments on failed start.
- Gate runtime shutdown so queued crash/restart sync work cannot reopen listeners after engine close.
- Track and close established Redis client connections when a fake Redis container stops.

## v0.3.1 — 2026-09-02

- Verified against the real upstream Docker Compose v5.5.0 Linux amd64 binary.
- Added Docker label/name/id/status filtering required by Compose project discovery.
- Added project-aware network and volume filtering.
- Added ContainerCreate `NetworkingConfig` / `HostConfig.NetworkMode` handling.
- Container list/inspect report actual Compose project network membership instead of hard-coded `bridge`.
- Added regression tests for Compose project filtering, network membership, and volume filtering.
- Verified cross-project isolation and `down -v` resource scoping.

## v0.3.0 — 2026-09-02

- Added strict cookbook parsing with exact file, JSON path, line, column, source excerpt, and caret diagnostics.
- Rejects unknown fields, duplicate JSON keys, trailing JSON, wrong types, invalid lifecycle states, invalid RESP2 replies, and incomplete seed sets.
- Added `--check` preflight for cookbooks, paths, ledger writeability, Unix socket safety, ports, direct container endpoints, overrides, and socket mode.
- Added explicit exit-code separation between configuration errors and internal runtime errors.
- Added internal panic recovery and `docker-zero.internal` ledger evidence.
- Added unsupported Docker API marker header and `mock_incomplete` matrix classification.
- Hardened matrix process supervision, output capture, timeout cleanup, mock-death detection, and ledger validation.
- Added portable `scripts/doctor.py` binary self-test.
- Corrected lifecycle transition ordering for start, stop, restart, and kill operations.
- Preserved cumulative restart count across later stop/start operations.
- Fixed Redis shutdown races for idle and accepted-but-not-yet-registered clients.
- Added unit, race, fuzz, state-invariant, black-box, and harness-classification regression tests.
