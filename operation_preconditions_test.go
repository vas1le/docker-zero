package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExecRequiresRunningContainerAtCreateAndStart(t *testing.T) {
	for _, state := range []string{"created", "exited", "paused", "restarting", "dead"} {
		t.Run(state, func(t *testing.T) {
			e, ledger := testEngine(t, 0)
			defer testCloseLedger(t, ledger)
			c, err := e.createContainer("exec-state", "nginx", nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			c.Scenario.Exec = []ExecFixture{{Cmd: []string{"true"}}}
			api := &DockerAPI{engine: e}
			c.start()
			create := httptest.NewRecorder()
			api.ServeHTTP(create, httptest.NewRequest("POST", "/containers/"+c.ID+"/exec", strings.NewReader(`{"Cmd":["true"]}`)))
			if create.Code != 201 {
				t.Fatal(create.Body.String())
			}
			var id struct{ Id string }
			if err := json.Unmarshal(create.Body.Bytes(), &id); err != nil {
				t.Fatal(err)
			}
			c.applyPatch(StatePatch{Status: stringPtr(state)})
			for _, path := range []string{"/containers/" + c.ID + "/exec", "/exec/" + id.Id + "/start"} {
				response := httptest.NewRecorder()
				api.ServeHTTP(response, httptest.NewRequest("POST", path, strings.NewReader(`{"Cmd":["true"]}`)))
				if response.Code != 409 {
					t.Errorf("%s while %s: %d %s", path, state, response.Code, response.Body.String())
				}
			}
		})
	}
}

func TestKillStoppedContainerDoesNotMutateState(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := e.createContainer("kill-state", "nginx", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	c.stop(42)
	before := c.stateSnapshot()
	response := httptest.NewRecorder()
	(&DockerAPI{engine: e}).ServeHTTP(response, httptest.NewRequest("POST", "/containers/"+c.ID+"/kill", nil))
	if response.Code != 409 || c.stateSnapshot() != before || c.eventCount("docker.kill") != 0 {
		t.Errorf("kill stopped: %d before=%#v after=%#v", response.Code, before, c.stateSnapshot())
	}
}

func TestRemoveContainerInvalidatesExecRecords(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := e.createContainer("exec-remove", "nginx", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	c.Scenario.Exec = []ExecFixture{{Cmd: []string{"true"}}}
	exec, err := e.createExec(c, execCreateRequest{Cmd: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.removeContainer(c.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"json", "start"} {
		method := "GET"
		if action == "start" {
			method = "POST"
		}
		response := httptest.NewRecorder()
		(&DockerAPI{engine: e}).ServeHTTP(response, httptest.NewRequest(method, "/exec/"+exec.ID+"/"+action, strings.NewReader(`{}`)))
		if response.Code != 404 {
			t.Errorf("orphan exec %s: %d", action, response.Code)
		}
	}
}

func TestKillRunningAndUnsupportedSignal(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := e.createContainer("kill-signals", "nginx", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	api := &DockerAPI{engine: e}
	response := httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest("POST", "/containers/"+c.ID+"/kill?signal=HUP", nil))
	if response.Code != 501 || !c.isRunning() {
		t.Fatal("unmodeled signal falsely executed")
	}
	response = httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest("POST", "/containers/"+c.ID+"/kill?signal=SIGKILL", nil))
	if response.Code != 204 || c.stateSnapshot().ExitCode != 137 || c.isRunning() {
		t.Fatalf("SIGKILL failed: %d %#v", response.Code, c.stateSnapshot())
	}
}
