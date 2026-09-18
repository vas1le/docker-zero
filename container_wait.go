package main

import (
	"encoding/json"
	"net/http"
)

// A buffered, one-shot result preserves an exit even if the container is
// immediately restarted. Cancellation removes the subscription without spawning
// a goroutine or retaining the disconnected client until a future exit.
type containerWaiter struct {
	condition string
	result    chan ContainerStateSnapshot
}

func (c *Container) registerWait(condition string) (<-chan ContainerStateSnapshot, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	waiter := &containerWaiter{condition: condition, result: make(chan ContainerStateSnapshot, 1)}
	if c.removed || (condition == "not-running" && !c.Running) {
		waiter.result <- c.stateSnapshotLocked()
	} else {
		if c.waiters == nil {
			c.waiters = make(map[*containerWaiter]struct{})
		}
		c.waiters[waiter] = struct{}{}
	}
	return waiter.result, func() {
		c.mu.Lock()
		delete(c.waiters, waiter)
		c.mu.Unlock()
	}
}

func (c *Container) notifyWaitersLocked(removed bool) {
	for waiter := range c.waiters {
		if !removed && waiter.condition == "removed" {
			continue
		}
		waiter.result <- c.stateSnapshotLocked()
		delete(c.waiters, waiter)
	}
}

func (c *Container) markRemoved() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Running {
		c.applyPatchLocked(StatePatch{Status: stringPtr("exited"), ExitCode: intPtr(137)})
	}
	c.removed = true
	c.notifyWaitersLocked(true)
}

func (api *DockerAPI) handleContainerWait(w http.ResponseWriter, r *http.Request, c *Container) {
	condition := r.URL.Query().Get("condition")
	if condition == "" {
		condition = "not-running"
	}
	switch condition {
	case "not-running", "next-exit", "removed":
	default:
		writeDockerError(w, http.StatusBadRequest, "invalid wait condition: "+condition)
		return
	}
	result, cancel := c.registerWait(condition)
	defer cancel()
	advance := c.advance("docker.wait")
	api.engine.syncRuntimeForState(c)

	// Docker clients use the response headers as the registration handshake,
	// before issuing the start/stop that will satisfy the wait.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = http.NewResponseController(w).Flush()
	select {
	case state := <-result:
		var waitError any
		if state.Error != "" {
			waitError = map[string]string{"Message": state.Error}
		}
		doc := map[string]any{"StatusCode": state.ExitCode, "Error": waitError}
		advance.After = state
		api.engine.logContainerEvent(c, "container.wait", advance, map[string]any{"condition": condition}, doc)
		_ = json.NewEncoder(w).Encode(doc)
	case <-r.Context().Done():
		api.engine.logContainerEvent(c, "container.wait.cancelled", advance, map[string]any{"condition": condition}, nil)
	}
}
