package main

import "fmt"

func (c *Container) execPreconditionLocked() error {
	if c.removed || !c.Running || c.Restarting || c.Dead {
		return fmt.Errorf("container %s is not running", c.Name)
	}
	if c.Paused {
		return fmt.Errorf("container %s is paused, unpause the container before exec", c.Name)
	}
	return nil
}

func (c *Container) kill() (ContainerStateSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	before := c.stateSnapshotLocked()
	if c.removed || !c.Running || c.Restarting || c.Dead {
		return before, fmt.Errorf("container %s is not running", c.Name)
	}
	c.manuallyStopped = true
	c.applyPatchLocked(StatePatch{Status: stringPtr("exited"), ExitCode: intPtr(137)})
	return before, nil
}
