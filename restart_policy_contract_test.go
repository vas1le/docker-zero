package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestObserversCannotRecoverWithoutRestartPolicy(t *testing.T) {
	for _, kind := range []string{"nginx", "redis"} {
		t.Run(kind, func(t *testing.T) {
			engine, ledger := testEngine(t, 2)
			defer testCloseLedger(t, ledger)
			c, err := engine.createContainer("policy-no", kind+":alpine", nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			c.start()
			event := "http.GET /health"
			if kind == "redis" {
				event = "redis.PING"
			}
			c.advance(event)
			c.advance(event)
			if c.stateSnapshot().Status != "exited" {
				t.Fatal("crash precondition failed")
			}
			api := &DockerAPI{engine: engine}
			for i := 0; i < 6; i++ {
				for _, path := range []string{"/containers/policy-no/json", "/containers/json?all=1"} {
					api.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
				}
			}
			state := c.stateSnapshot()
			if state.Status != "exited" || state.RestartCount != 0 {
				t.Fatalf("polling recovered container: %+v", state)
			}
			response := httptest.NewRecorder()
			api.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/containers/policy-no/restart", nil))
			if response.Code != http.StatusNoContent || !c.isRunning() {
				t.Fatalf("explicit recovery failed: %d %+v", response.Code, c.stateSnapshot())
			}
		})
	}
}

func TestRestartPoliciesRespectManualStopAndRetryBudget(t *testing.T) {
	for _, policy := range []string{"always", "unless-stopped", "on-failure"} {
		t.Run(policy, func(t *testing.T) {
			engine, ledger := testEngine(t, 2)
			defer testCloseLedger(t, ledger)
			c, err := engine.createContainer("policy-test", "nginx:alpine", nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			c.RestartPolicy = RestartPolicy{Name: policy, MaximumRetryCount: 1}
			c.start()
			c.stop(137)
			for i := 0; i < 4; i++ {
				c.advance("docker.inspect")
			}
			if c.isRunning() {
				t.Fatal("manual stop was undone by polling")
			}
			c.start()
			c.applyPatch(StatePatch{Status: stringPtr("exited"), ExitCode: intPtr(137)})
			c.advance("docker.inspect")
			c.advance("docker.inspect")
			c.advance("docker.inspect")
			if !c.isRunning() {
				t.Fatal("permitted automatic restart did not happen")
			}
			c.applyPatch(StatePatch{Status: stringPtr("exited"), ExitCode: intPtr(137)})
			c.advance("docker.inspect")
			if policy == "on-failure" && c.stateSnapshot().Status != "exited" {
				t.Fatal("on-failure retry budget exceeded")
			}
		})
	}
}

func TestOnFailureDoesNotRestartSuccessfulExit(t *testing.T) {
	engine, ledger := testEngine(t, 2)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("successful-exit", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.RestartPolicy = RestartPolicy{Name: "on-failure"}
	c.start()
	c.applyPatch(StatePatch{Status: stringPtr("exited"), ExitCode: intPtr(0)})
	for i := 0; i < 4; i++ {
		c.advance("docker.inspect")
	}
	if c.stateSnapshot().Status != "exited" {
		t.Fatal("successful exit was restarted")
	}
}
