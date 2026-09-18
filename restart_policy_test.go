package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPollingCannotRecoverContainerWithNoRestartPolicy(t *testing.T) {
	e, ledger := testEngine(t, 2)
	defer testCloseLedger(t, ledger)
	defer testCloseRuntimes(t, e)
	e.endpointMode = "off"
	api := &DockerAPI{engine: e}
	r := httptest.NewRecorder()
	api.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/containers/create?name=polling-controller", strings.NewReader(`{"Image":"nginx","HostConfig":{"RestartPolicy":{"Name":"no"}}}`)))
	if r.Code != http.StatusCreated {
		t.Fatal(r.Body.String())
	}
	c, err := e.findContainer("polling-controller")
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	c.advance("http.GET /health")
	c.advance("http.GET /health")
	if c.stateSnapshot().Status != "exited" {
		t.Fatal("scenario did not crash")
	}
	// A broken controller that only polls must not pass a recovery test.
	for i := 0; i < 20; i++ {
		for _, path := range []string{"/containers/" + c.ID + "/json", "/containers/json?all=1"} {
			r := httptest.NewRecorder()
			api.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
			if r.Code != http.StatusOK {
				t.Fatal(r.Code)
			}
		}
		if state := c.stateSnapshot(); state.Status != "exited" || state.RestartCount != 0 {
			t.Fatalf("polling manufactured recovery: %+v", state)
		}
	}
	r = httptest.NewRecorder()
	api.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/containers/"+c.ID+"/restart", nil))
	if r.Code != http.StatusNoContent || !c.isRunning() {
		t.Fatalf("explicit recovery failed: %d %+v", r.Code, c.stateSnapshot())
	}
}

func TestAutomaticRestartPolicyEligibilityAndRetryBudget(t *testing.T) {
	for _, test := range []struct {
		name    string
		policy  RestartPolicy
		exit    int
		allowed int
	}{
		{"default", RestartPolicy{}, 137, 0},
		{"no", RestartPolicy{Name: "no"}, 137, 0},
		{"always", RestartPolicy{Name: "always"}, 0, 3},
		{"unless-stopped", RestartPolicy{Name: "unless-stopped"}, 137, 3},
		{"successful-on-failure", RestartPolicy{Name: "on-failure"}, 0, 0},
		{"bounded-on-failure", RestartPolicy{Name: "on-failure", MaximumRetryCount: 2}, 42, 2},
		{"unbounded-on-failure", RestartPolicy{Name: "on-failure"}, 42, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			e, ledger := testEngine(t, 2)
			defer testCloseLedger(t, ledger)
			c, err := e.createContainer("policy-test", "nginx", nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			c.RestartPolicy = test.policy
			c.start()
			for attempt := 0; attempt < 3; attempt++ {
				c.applyPatch(StatePatch{Status: stringPtr("exited"), ExitCode: intPtr(test.exit)})
				advance := c.advance("docker.inspect")
				if attempt < test.allowed {
					if advance.After.Status != "restarting" || advance.After.RestartCount != attempt+1 || !strings.Contains(advance.Transition, ":restart_policy") {
						t.Fatalf("attempt %d: %+v", attempt, advance)
					}
					c.advance("docker.inspect")
					if !c.isRunning() {
						t.Fatal("automatic restart failed to finish")
					}
				} else if advance.After.Status != "exited" {
					t.Fatalf("ineligible automatic restart: %+v", advance)
				}
			}
			if c.automaticRestarts != test.allowed {
				t.Fatalf("retry budget = %d", c.automaticRestarts)
			}
		})
	}
}

func TestManualStopCancelsPendingAutomaticRestart(t *testing.T) {
	e, ledger := testEngine(t, 2)
	defer testCloseLedger(t, ledger)
	defer testCloseRuntimes(t, e)
	e.endpointMode = "off"
	c, err := e.createContainer("manual-stop", "nginx", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.RestartPolicy = RestartPolicy{Name: "always"}
	c.start()
	c.applyPatch(StatePatch{Status: stringPtr("exited"), ExitCode: intPtr(137)})
	r := httptest.NewRecorder()
	(&DockerAPI{engine: e}).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/containers/"+c.ID+"/stop", nil))
	if r.Code != http.StatusNotModified {
		t.Fatal(r.Code)
	}
	for i := 0; i < 5; i++ {
		if c.advance("docker.inspect").After.Status != "exited" {
			t.Fatal("stopped container resurrected")
		}
	}
	c.start()
	c.applyPatch(StatePatch{Status: stringPtr("exited"), ExitCode: intPtr(137)})
	if c.advance("docker.inspect").After.Status != "restarting" {
		t.Fatal("explicit start did not re-enable policy")
	}
	r = httptest.NewRecorder()
	(&DockerAPI{engine: e}).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/containers/"+c.ID+"/kill", nil))
	if r.Code != http.StatusNoContent {
		t.Fatalf("cannot cancel restarting container: %d", r.Code)
	}
	if c.advance("docker.inspect").After.Status != "exited" {
		t.Fatal("kill did not suppress restart")
	}
}

func TestScenarioCannotStartCreatedOrDeadContainers(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := e.createContainer("no-resurrection", "nginx", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.RestartPolicy = RestartPolicy{Name: "always"}
	c.Scenario.Transitions = []Transition{{Event: "docker.observe", Set: StatePatch{Status: stringPtr("running")}}}
	if c.advance("docker.inspect").After.Status != "created" {
		t.Fatal("observation started an unstarted container")
	}
	c.start()
	c.applyPatch(StatePatch{Status: stringPtr("dead")})
	if c.advance("docker.inspect").After.Status != "dead" {
		t.Fatal("observation revived a dead container")
	}
}
