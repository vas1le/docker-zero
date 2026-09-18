package main

import (
	"encoding/json"
	"net/http"
)

type containerWaitError struct {
	Message string `json:"Message"`
}

type containerWaitResult struct {
	StatusCode int                 `json:"StatusCode"`
	Error      *containerWaitError `json:"Error"`
}

type containerWaiter struct {
	condition string
	result    chan containerWaitResult
}

// registerWait captures registration and the current state under the same lock.
// Buffered results preserve an exit even if another request immediately restarts
// the container. Cancellation removes the subscription without a helper goroutine.
func (c *Container) registerWait(condition string) (<-chan containerWaitResult, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	waiter := &containerWaiter{condition: condition, result: make(chan containerWaitResult, 1)}
	if c.removed || (condition == "not-running" && !c.Running) {
		waiter.result <- c.waitResultLocked()
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

func (c *Container) waitResultLocked() containerWaitResult {
	result := containerWaitResult{StatusCode: c.ExitCode}
	if c.Error != "" {
		result.Error = &containerWaitError{Message: c.Error}
	}
	return result
}

func (c *Container) notifyWaitersLocked(removed bool) {
	result := c.waitResultLocked()
	for waiter := range c.waiters {
		if removed || waiter.condition != "removed" {
			waiter.result <- result
			delete(c.waiters, waiter)
		}
	}
}

func (c *Container) markRemoved() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Running {
		c.applyPatchLocked(StatePatch{Status: stringPtr("exited"), ExitCode: intPtr(137)})
	}
	c.removed = true
	// Removal also releases next-exit waiters for never-started containers.
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
	// Docker SDKs wait for headers before issuing the operation that will make
	// the body ready. Register BEFORE flushing so an immediate stop cannot race.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := http.NewResponseController(w).Flush(); err != nil {
		return
	}
	select {
	case <-r.Context().Done():
		return
	case status := <-result:
		api.engine.ledger.Log(LedgerEntry{Container: c.Name, Kind: c.Kind, Scenario: c.Seed, Channel: "docker", Event: "container.wait", Request: map[string]any{"condition": condition}, Response: status})
		_ = json.NewEncoder(w).Encode(status)
	}
}
