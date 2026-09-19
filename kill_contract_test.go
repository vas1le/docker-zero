package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestKillRejectsStoppedContainersWithoutMutation(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("kill-contract", "redis:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	api := &DockerAPI{engine: engine}
	for _, status := range []string{"created", "exited", "dead"} {
		c.applyPatch(StatePatch{Status: stringPtr(status), ExitCode: intPtr(42)})
		before := c.stateSnapshot()
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/containers/kill-contract/kill", nil))
		if response.Code != http.StatusConflict {
			t.Errorf("%s: got %d, want 409", status, response.Code)
		}
		if after := c.stateSnapshot(); after != before {
			t.Errorf("%s: state changed on rejected kill: %+v", status, after)
		}
	}
}

func TestKillSignalsAreNotSilentlyIgnored(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("kill-signal", "redis:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	api := &DockerAPI{engine: engine}
	for _, signal := range []string{"", "SIGKILL", "KILL", "9", "SIGHUP", "SIGTERM", "0"} {
		c.start()
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/containers/kill-signal/kill?signal="+signal, nil))
		if signal == "SIGHUP" || signal == "SIGTERM" || signal == "0" {
			if response.Code != http.StatusNotImplemented || response.Header().Get("X-Docker-Zero-Unsupported") != "true" || !c.stateSnapshot().Running {
				t.Errorf("unsupported signal %q must not mutate state: %d %s", signal, response.Code, response.Body.String())
			}
		} else if response.Code != http.StatusNoContent || c.stateSnapshot().ExitCode != 137 || c.stateSnapshot().Running {
			t.Errorf("SIGKILL %q: %d %+v", signal, response.Code, c.stateSnapshot())
		}
	}
}

func TestConcurrentKillHasExactlyOneSuccess(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("kill-concurrent", "redis:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	api := &DockerAPI{engine: engine}
	results := make(chan int, 32)
	var workers sync.WaitGroup
	for i := 0; i < cap(results); i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			response := httptest.NewRecorder()
			api.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/containers/kill-concurrent/kill", nil))
			results <- response.Code
		}()
	}
	workers.Wait()
	close(results)
	successes := 0
	for code := range results {
		switch code {
		case http.StatusNoContent:
			successes++
		case http.StatusConflict:
		default:
			t.Errorf("unexpected status %d", code)
		}
	}
	if successes != 1 {
		t.Errorf("successful kills=%d, want 1", successes)
	}
}
