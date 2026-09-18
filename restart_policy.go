package main

func patchStartsContainer(patch StatePatch) bool {
	if patch.Status != nil {
		return *patch.Status == "running" || *patch.Status == "restarting" || *patch.Status == "paused"
	}
	return (patch.Running != nil && *patch.Running) || (patch.Restarting != nil && *patch.Restarting)
}

// Automatic recovery remains a deterministic cookbook transition, not a real
// daemon restart scheduler. Retry budgets count automatic recovery edges since
// the last explicit start/restart; inspect never resets the budget.
func (c *Container) automaticRestartAllowedLocked() bool {
	if c.Status != "exited" || c.restartSuppressed || c.removed {
		return false
	}
	switch c.RestartPolicy.Name {
	case "always", "unless-stopped":
		return true
	case "on-failure":
		return c.ExitCode != 0 && (c.RestartPolicy.MaximumRetryCount == 0 || c.autoRestartAttempts < c.RestartPolicy.MaximumRetryCount)
	default:
		return false
	}
}

func (c *Container) suppressAutomaticRestart() {
	c.mu.Lock()
	c.restartSuppressed = true
	c.mu.Unlock()
}
