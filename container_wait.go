package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type containerWaitResult struct {
	StatusCode int `json:"StatusCode"`
	Error      *struct {
		Message string `json:"Message"`
	} `json:"Error"`
}

type containerWaiter struct {
	condition string
	result    chan containerWaitResult
}

// registerWait installs a subscription before returning. Results are buffered so
// an exit followed immediately by a restart cannot overwrite the observed code.
// Cancellation removes the subscription without creating a waiter goroutine.
func (c *Container) registerWait(condition string) (<-chan containerWaitResult, func(), error) {
	if condition == "" {
		condition = "not-running"
	}
	switch condition {
	case "not-running", "next-exit", "removed":
	default:
		return nil, nil, fmt.Errorf("invalid wait condition: %q", condition)
	}
	waiter := &containerWaiter{condition: condition, result: make(chan containerWaitResult, 1)}
	c.mu.Lock()
	if c.removed || (condition == "not-running" && !c.Running) {
		waiter.result <- containerWaitResult{StatusCode: c.ExitCode}
	} else {
		if c.waiters == nil {
			c.waiters = make(map[*containerWaiter]struct{})
		}
		c.waiters[waiter] = struct{}{}
	}
	c.mu.Unlock()
	cancel := func() {
		c.mu.Lock()
		delete(c.waiters, waiter)
		c.mu.Unlock()
	}
	return waiter.result, cancel, nil
}

func (c *Container) notifyWaitersLocked(exited bool) {
	for waiter := range c.waiters {
		ready := c.removed || (waiter.condition == "not-running" && !c.Running) || (waiter.condition == "next-exit" && exited)
		if ready {
			waiter.result <- containerWaitResult{StatusCode: c.ExitCode}
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
	c.notifyWaitersLocked(false)
}

func (api *DockerAPI) handleContainerWait(w http.ResponseWriter, r *http.Request, c *Container) {
	condition := r.URL.Query().Get("condition")
	result, cancel, err := c.registerWait(condition)
	if err != nil {
		writeDockerError(w, http.StatusBadRequest, err.Error())
		return
	}
	defer cancel()
	advance := c.advance("docker.wait")
	// Docker clients may register next-exit before starting a container. Flush the
	// headers after registration so they can issue start while the body is pending.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = http.NewResponseController(w).Flush()
	select {
	case status := <-result:
		advance.After = c.stateSnapshot()
		api.engine.logContainerEvent(c, "container.wait", advance, map[string]any{"condition": condition}, status)
		_ = json.NewEncoder(w).Encode(status)
	case <-r.Context().Done():
		return
	}
}
