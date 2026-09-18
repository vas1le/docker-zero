package main

import "testing"

func TestNoRestartPolicyCannotRecoverThroughObservation(t *testing.T) {
	for _, policy := range []string{"", "no"} {
		t.Run(policy, func(t *testing.T) {
			_, c := contractAPI(t, 2)
			c.RestartPolicy = RestartPolicy{Name: policy}
			c.start()
			c.advance("http.GET /health")
			c.advance("http.GET /health")
			for i := 0; i < 10; i++ {
				c.advance("docker.inspect")
				c.advance("docker.list")
			}
			if s := c.stateSnapshot(); s.Status != "exited" || s.ExitCode != 137 || s.RestartCount != 0 {
				t.Fatalf("polling recovered without controller: %+v", s)
			}
			c.restart()
			c.advance("docker.inspect")
			if !c.isRunning() {
				t.Fatal("explicit restart must still work with policy=no")
			}
		})
	}
}

func TestRestartPolicyEligibilityAndRetryLimit(t *testing.T) {
	for _, tc := range []struct {
		policy  string
		exit    int
		allowed bool
	}{
		{"always", 0, true}, {"unless-stopped", 0, true}, {"on-failure", 0, false}, {"on-failure", 42, true},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			_, c := contractAPI(t, 2)
			c.RestartPolicy = RestartPolicy{Name: tc.policy}
			c.start()
			c.applyPatch(StatePatch{Status: stringPtr("exited"), ExitCode: intPtr(tc.exit)})
			c.advance("docker.inspect")
			if got := c.stateSnapshot().Restarting; got != tc.allowed {
				t.Fatalf("restart allowed=%t want=%t", got, tc.allowed)
			}
		})
	}
	_, c := contractAPI(t, 2)
	c.RestartPolicy = RestartPolicy{Name: "on-failure", MaximumRetryCount: 2}
	c.start()
	for attempt := 1; attempt <= 3; attempt++ {
		c.applyPatch(StatePatch{Status: stringPtr("exited"), ExitCode: intPtr(42)})
		c.advance("docker.inspect")
		if got := c.stateSnapshot().Restarting; got != (attempt <= 2) {
			t.Fatalf("attempt %d restarting=%t", attempt, got)
		}
		if attempt <= 2 {
			c.advance("docker.inspect")
		}
	}
	if got := c.stateSnapshot().RestartCount; got != 2 {
		t.Fatalf("restart count=%d want2", got)
	}
	c.start() // explicit intervention resets the retry budget, not total restarts.
	c.applyPatch(StatePatch{Status: stringPtr("exited"), ExitCode: intPtr(42)})
	c.advance("docker.inspect")
	if s := c.stateSnapshot(); !s.Restarting || s.RestartCount != 3 {
		t.Fatalf("explicit start retry reset: %+v", s)
	}
}

func TestManualStopSuppressesAutomaticRestart(t *testing.T) {
	for _, policy := range []string{"always", "unless-stopped", "on-failure"} {
		t.Run(policy, func(t *testing.T) {
			api, c := contractAPI(t, 2)
			c.RestartPolicy = RestartPolicy{Name: policy}
			c.start()
			contractRequest(api, "POST", "/containers/contract/stop", "")
			for i := 0; i < 5; i++ {
				c.advance("docker.inspect")
			}
			if c.stateSnapshot().Status != "exited" {
				t.Fatal("manual stop was undone by observation")
			}
			c.start()
			c.applyPatch(StatePatch{Status: stringPtr("exited"), ExitCode: intPtr(42)})
			c.advance("docker.inspect")
			if !c.stateSnapshot().Restarting {
				t.Fatal("explicit start did not re-enable policy")
			}
		})
	}
}

func TestStopAlreadyExitedPreservesExitAndSuppressesRecovery(t *testing.T) {
	api, c := contractAPI(t, 2)
	c.RestartPolicy = RestartPolicy{Name: "always"}
	c.start()
	c.applyPatch(StatePatch{Status: stringPtr("exited"), ExitCode: intPtr(137)})
	response := contractRequest(api, "POST", "/containers/contract/stop", "")
	if response.Code != 304 {
		t.Fatalf("stop status=%d", response.Code)
	}
	c.advance("docker.inspect")
	if s := c.stateSnapshot(); s.Status != "exited" || s.ExitCode != 137 {
		t.Fatalf("stop changed exited state: %+v", s)
	}
}
