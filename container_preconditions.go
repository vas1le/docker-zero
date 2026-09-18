package main

import "fmt"

// Exec creation and exec start must both check the live container state. The
// container may have stopped, paused, or disappeared between those requests.
func (c *Container) checkExecState() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.removed || !c.Running || c.Restarting || c.Dead {
		return fmt.Errorf("container %s is not running", c.Name)
	}
	if c.Paused {
		return fmt.Errorf("container %s is paused; unpause the container before exec", c.Name)
	}
	return nil
}
