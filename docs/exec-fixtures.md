# Explicit exec fixtures

Docker Zero does **not** execute shell commands or application processes. An
unconfigured exec returns HTTP 501 with `X-Docker-Zero-Unsupported: true`, which
makes the matrix runner classify the test as `mock_incomplete`, not passed.

Add exact command outcomes to a seed in an external cookbook, then launch with
`--cookbook-dir /path/to/cookbooks`. For example, inside `seeds["0"]`:

```json
"exec": [
  {
    "cmd": ["sh", "-c", "exit 42"],
    "stdout": "starting migration\n",
    "stderr": "migration failed\n",
    "exit_code": 42
  }
]
```

All arguments must match exactly. `exit_code` is required (0–255); duplicate
commands are rejected at cookbook validation. Each exec is single-use and
completes synchronously with the configured result. Inspection before start has
`ExitCode: null`; after start it reports the fixture's exit code. Attached,
non-TTY output supports the Docker SDK/CLI HTTP-to-TCP upgrade handshake and uses Docker stream 1 for stdout and stream 2 for stderr. Detached
execution returns no output but records the same result. Container state is
checked at both create and start.

TTY, stdin, execution-context/environment overrides, and unknown exec fields are
explicitly unsupported. A fixture verifies the **controller's reaction** to a
specified outcome; it does not prove that the command, migration or application
would produce that result in a real container. No built-in success fallback is
provided.
