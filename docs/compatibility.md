# Simulation contract

Docker Zero tests software that controls Docker. It does not execute images,
commands, healthcheck programs, or provide a container filesystem.

## Wait

`POST /containers/{id}/wait` supports `not-running` (the default), `next-exit`,
and `removed`. It registers before flushing HTTP headers, blocks the response
body until the condition occurs, preserves the event's exit code across a fast
restart, and removes its subscription when the client cancels. An invalid
condition returns HTTP 400.

## Exec fixtures

Exec is fail-closed: an unconfigured command returns HTTP 501 and
`X-Docker-Zero-Unsupported: true`. There is no shell interpreter or implicit
success fallback. Add an optional `exec` array to the selected cookbook seed:

```json
"exec": [
  {
    "cmd": ["sh", "-c", "exit 42"],
    "stdout": "migration started\n",
    "stderr": "migration failed\n",
    "exit_code": 42
  }
]
```

Matching uses the exact argument vector, not a joined command string. The
fixture completes when `/exec/{id}/start` is called; inspection returns a null
exit code before start and the configured code afterwards. Attached stdout and
stderr use Docker's stream 1/2 framing. Detached starts return no stream data.
An exec instance can run only once. Exit codes must be 0–255; duplicate command
fixtures are rejected. The supplied cookbooks intentionally have no catch-all
exec fixtures.

TTY, stdin, privilege, user/environment/workdir overrides, and detach-key
semantics are not modeled and are explicitly rejected. Fixtures reproduce
configured outcomes only; they do not establish that a real command works.

## Archives

Container archive PUT, GET, and HEAD return HTTP 501 with the unsupported
marker. No upload is accepted, no filesystem mutation is implied, and these
requests do not advance scenario state. A successful Docker copy requires a
real Engine or a future explicitly implemented filesystem model.

## Requested configuration versus simulated behavior

Container creation retains the supplied configuration in `Config` and
`HostConfig` on inspection, including user, entrypoint, working directory,
healthcheck and resource settings. Restart policy defaults to `no`; explicit
policies and retry limits are retained. `ExposedPorts` includes image/model
defaults and requested declarations. Port bindings and network endpoints reflect
the simulator's allocated resources, not the originally requested allocation.

Retention does **not** execute a custom entrypoint or health command, enforce
memory/security limits, or create a filesystem. Custom settings that are only
metadata are named in create `Warnings`, the `X-Docker-Zero-Metadata-Only`
response header, and inspect's `DockerZero.MetadataOnlyFields`. Some service
fixtures interpret command/environment options, but general process execution
is not implemented. Requested advanced networking settings are retained under
`DockerZero.RequestedNetworkingConfig`, not falsely reported as allocated IPs.

Use `--strict` to reject metadata-only create settings with the unsupported
marker **before** allocating a container. This flag checks the create request's
configuration; it is not a claim of full Docker conformance. Exec and archive
operations fail closed regardless of this flag. Empty/default SDK fields are
not rejected merely for being present. `Healthcheck.Test: ["NONE"]` disables
reported health; other custom checks are stored, never executed. Service health
otherwise comes from the cookbook, including its synthetic default healthcheck.

`GET /__docker_zero/capabilities` describes these boundaries and the strict flag.
