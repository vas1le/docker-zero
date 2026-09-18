package main

func patchStartsContainer(patch StatePatch) bool {
	if patch.Status == nil {
		return false
	}
	return *patch.Status == "running" || *patch.Status == "restarting" || *patch.Status == "paused"
}

// A scenario describes WHEN an automatic restart is observed. It cannot grant
// permission to restart a stopped container: that comes from the requested
// policy and the user's explicit lifecycle actions. There is no background
// clock/backoff scheduler; the retry budget spans one explicit start session.
func (c *Container) canRestartAutomaticallyLocked() bool {
	if c.removed || c.manuallyStopped || c.Status != "exited" || c.StartCount == 0 {
		return false
	}
	switch c.RestartPolicy.Name {
	case "always", "unless-stopped":
		return true
	case "on-failure":
		return c.ExitCode != 0 && (c.RestartPolicy.MaximumRetryCount == 0 || c.automaticRestarts < c.RestartPolicy.MaximumRetryCount)
	default:
		return false
	}
}

func (c *Container) markManualStop() {
	c.mu.Lock()
	c.manuallyStopped = true
	c.mu.Unlock()
}
