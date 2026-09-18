package main

// Restart policy gates cookbook recovery edges. Observation remains the fixture
// clock; it must never substitute for a controller action under policy "no".
func (c *Container) permitsAutomaticRestartLocked() bool {
	if c.manuallyStopped || c.removed || c.Status != "exited" {
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

func resumesContainer(patch StatePatch) bool {
	if patch.Status != nil {
		return *patch.Status == "running" || *patch.Status == "restarting" || *patch.Status == "paused"
	}
	return (patch.Running != nil && *patch.Running) || (patch.Restarting != nil && *patch.Restarting)
}
