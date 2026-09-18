# Restart-policy contract

A crash fixture no longer bypasses `HostConfig.RestartPolicy`. The default is
`no`: an exited container remains exited until an explicit start/restart.
Inspect/list calls cannot manufacture a controller's recovery action.

`always` and `unless-stopped` permit scenario-defined automatic recovery after
an exit. `on-failure` additionally requires a nonzero exit status and respects
MaximumRetryCount (zero means no cap). Retry accounting is separate from the
historical RestartCount; an explicit start resets the automatic retry budget.
Manual stop/kill suppresses automatic restart until an explicit start/restart.
Never-started and dead containers cannot be started by observation transitions.

The existing seed-2 observation-driven path is retained **only when a policy
permits it**. Automatic transitions have `:restart_policy` in the ledger's
transition field. Controller restart requests remain separate `container.restart`
events. Tests requiring controller intervention should use policy `no` and assert
that a start/restart action occurred, not merely that health eventually improved.

## Deliberate limits

This simulator does not reproduce Docker's background restart manager, backoff,
10-second timing rules, process execution, or daemon-restart persistence. Retry
budgets span an explicit start session rather than elapsed healthy runtime.
Automatic restart timing is driven by the cookbook's request sequence, not a
background clock. `always` and `unless-stopped` therefore have identical behavior
within a running simulator session. Test timing and daemon-reboot semantics
against a real Docker Engine.

Docker's reference behavior: https://docs.docker.com/engine/containers/start-containers-automatically/
