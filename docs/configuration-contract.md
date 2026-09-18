# Configuration contract

Create/inspect preserves requested configuration instead of replacing user,
working directory, entrypoint, healthcheck, and restart policy with unrelated
values. The command/entrypoint is reflected in `Path`/`Args`. Explicitly exposed
ports are merged with the service model's ports. Requested networking options
are also available under `DockerZero.RequestedNetworkingConfig`.

**Preserved metadata is not executed behavior.** Unknown non-default settings,
process context, arbitrary command/environment settings, mounts/resource limits,
and custom healthcheck commands are stored but reported in create `Warnings`
and inspect `DockerZero.Configuration.metadata_only`. Health normally comes
from the cookbook fixture, not a real healthcheck process. `Test: ["NONE"]`
disables health reporting. Docker Zero never executes a requested process.

A create using metadata-only behavior carries
`X-Docker-Zero-Unsupported: true` even when it returns 201. The existing matrix
runner therefore classifies the run as **mock_incomplete**, not a valid passing
behavioral test. Inspection still lets a metadata-specific test verify exactly
what configuration its controller sent.

For fail-fast CI, launch the engine with `--strict`. Such create requests then
return 501 **before** reserving a container name or allocating resources. Zero
or empty unmodeled options sent by SDK serializers do not cause a rejection.
This is strict classification of configuration, not a claim of complete Docker
API fidelity. Unconfigured exec, archives, and unknown images always fail
closed independently of this flag.

Restart policy defaults to `no`; valid names are `no`, `always`,
`unless-stopped`, and `on-failure`. MaximumRetryCount is nonnegative and applies
only to `on-failure`. See the restart-policy contract for the simulator's
observation-driven scheduling limits.

The default Compose ConsoleSize `[0,0]` and empty LogConfig are neutral, not
simulation gaps. Stdout/stderr attachment flags are preserved as configuration;
requesting an unsupported attach endpoint still fails closed. The exact Redis
`["redis-server", "--replicaof" (or "--slaveof"), host, port]` command is modeled
as replication configuration. Other commands/flags remain metadata-only; no
host command is executed.
