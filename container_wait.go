package main

import "net/http"

type containerWaitResult struct {
	StatusCode int                 `json:"StatusCode"`
	Error      *containerWaitError `json:"Error"`
}

type containerWaitError struct {
	Message string `json:"Message"`
}

type containerWaiter struct {
	condition string
	result    chan containerWaitResult
}

// Registration and exit notifications share c.mu. Buffered per-request results
// preserve the exit status even if a stop is immediately followed by a start.
func (c *Container) subscribeWait(condition string) (*containerWaiter, func()) {
	c.mu.Lock()
	waiter := &containerWaiter{condition: condition, result: make(chan containerWaitResult, 1)}
	if c.removed || (condition == "not-running" && !c.Running) {
		waiter.result <- c.waitResultLocked()
	} else {
		if c.waiters == nil {
			c.waiters = make(map[*containerWaiter]struct{})
		}
		c.waiters[waiter] = struct{}{}
	}
	c.mu.Unlock()
	return waiter, func() {
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
	for waiter := range c.waiters {
		if !removed && waiter.condition == "removed" {
			continue
		}
		waiter.result <- c.waitResultLocked()
		delete(c.waiters, waiter)
	}
}

func (c *Container) markRemoved() {
	c.mu.Lock()
	defer c.mu.Unlock()
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
	before := c.stateSnapshot()
	waiter, cancel := c.subscribeWait(condition)
	defer cancel()
	// Docker SDKs wait for these headers before issuing the operation that
	// satisfies the condition. Register first to avoid a lost exit notification.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	select {
	case result := <-waiter.result:
		api.engine.logContainerEvent(c, "container.wait", eventAdvance{Before: before, After: c.stateSnapshot()}, map[string]any{"condition": condition}, result)
		writeJSON(w, http.StatusOK, result)
	case <-r.Context().Done():
		// Do not fabricate an exit result on client cancellation.
		api.engine.logContainerEvent(c, "container.wait.cancelled", eventAdvance{Before: before, After: c.stateSnapshot()}, map[string]any{"condition": condition}, nil)
	}
}
