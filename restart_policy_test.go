package main

import "testing"

func TestPollingCannotRecoverNoRestartPolicy(t *testing.T) {
	for _, policy := range []string{"", "no"} {
		e, ledger := testEngine(t, 2)
		c, err := e.createContainer("policy-no", "nginx", nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		c.RestartPolicy = restartPolicy{Name: policy}
		c.start()
		c.advance("http.GET /health")
		c.advance("http.GET /health")
		for i := 0; i < 20; i++ {
			c.advance("docker.inspect")
			c.advance("docker.list")
		}
		state := c.stateSnapshot()
		if state.Status != "exited" || state.RestartCount != 0 || state.ExitCode != 137 {
			t.Errorf("poll-only controller falsely recovered: %#v", state)
		}
		c.start()
		if c.stateSnapshot().Status != "running" {
			t.Error("explicit start did not recover")
		}
		testCloseLedger(t, ledger)
	}
}

func TestManualStopPreventsObservationRecovery(t *testing.T) {
	for _, policy := range []string{"always", "unless-stopped", "on-failure"} {
		e, ledger := testEngine(t, 2)
		c, err := e.createContainer("manual-stop", "nginx", nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		c.RestartPolicy = restartPolicy{Name: policy}
		c.start()
		c.stop(137)
		for i := 0; i < 10; i++ {
			c.advance("docker.inspect")
		}
		if state := c.stateSnapshot(); state.Status != "exited" || state.RestartCount != 0 {
			t.Errorf("manual stop ignored for %s: %#v", policy, state)
		}
		c.restart()
		c.advance("docker.inspect")
		if c.stateSnapshot().Status != "running" {
			t.Errorf("manual restart didn't recover %s", policy)
		}
		testCloseLedger(t, ledger)
	}
}

func TestAutomaticRestartPolicyAndRetryLimit(t *testing.T) {
	for _, test := range []struct {
		policy    string
		code, max int
		allowed   bool
	}{
		{"no", 137, 0, false}, {"always", 0, 0, true}, {"unless-stopped", 137, 0, true}, {"on-failure", 0, 0, false}, {"on-failure", 137, 1, true},
	} {
		e, ledger := testEngine(t, 2)
		c, err := e.createContainer("auto-restart", "nginx", nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		c.RestartPolicy = restartPolicy{Name: test.policy, MaximumRetryCount: test.max}
		c.start()
		c.applyPatch(StatePatch{Status: stringPtr("exited"), ExitCode: intPtr(test.code)})
		first := c.advance("docker.inspect").After
		if (first.Status == "restarting") != test.allowed {
			t.Errorf("%#v -> %#v", test, first)
		}
		if test.allowed {
			c.advance("docker.inspect")
			c.advance("docker.inspect")
			if c.stateSnapshot().RestartCount != 1 {
				t.Fatal("restart not counted exactly once")
			}
			c.applyPatch(StatePatch{Status: stringPtr("exited"), ExitCode: intPtr(137)})
			second := c.advance("docker.list").After
			if test.max == 1 && (second.Status != "exited" || second.RestartCount != 1) {
				t.Fatalf("retry limit ignored: %#v", second)
			}
			if test.max == 0 && second.Status != "restarting" {
				t.Fatalf("unlimited policy didn't retry: %#v", second)
			}
		}
		testCloseLedger(t, ledger)
	}
}
