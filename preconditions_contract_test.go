package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExecCreateRejectsInactiveContainerStates(t *testing.T) {
	for _, status := range []string{"created", "exited", "dead", "restarting", "paused"} {
		t.Run(status, func(t *testing.T) {
			e, ledger := testEngine(t, 0)
			defer testCloseLedger(t, ledger)
			c, err := e.createContainer("exec-precondition", "nginx", nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			c.applyPatch(StatePatch{Status: stringPtr(status)})
			rec := httptest.NewRecorder()
			(&DockerAPI{engine: e}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/containers/"+c.ID+"/exec", strings.NewReader(`{"Cmd":["true"]}`)))
			if rec.Code != http.StatusConflict {
				t.Fatalf("%s accepted exec: %d %s", status, rec.Code, rec.Body.String())
			}
			if len(e.execs) != 0 {
				t.Fatal("rejected request allocated an exec")
			}
		})
	}
}

func TestExecStartRechecksContainerState(t *testing.T) {
	for _, status := range []string{"exited", "paused", "restarting", "removed"} {
		t.Run(status, func(t *testing.T) {
			e, ledger := testEngine(t, 0)
			defer testCloseLedger(t, ledger)
			c, err := e.createContainer("exec-start-precondition", "nginx", nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			c.start()
			// Isolate the start precondition from command-fixture matching.
			e.execs["fixture"] = &ExecInstance{ID: "fixture", ContainerID: c.ID}
			want := http.StatusConflict
			if status == "removed" {
				if err := e.removeContainer(c.ID, true); err != nil {
					t.Fatal(err)
				}
				want = http.StatusNotFound
			} else {
				c.applyPatch(StatePatch{Status: stringPtr(status)})
			}
			rec := httptest.NewRecorder()
			(&DockerAPI{engine: e}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/exec/fixture/start", strings.NewReader(`{}`)))
			if rec.Code != want {
				t.Fatalf("%s returned %d: %s", status, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestKillStoppedContainerIsConflictWithoutMutation(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := e.createContainer("kill-precondition", "nginx", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	api := &DockerAPI{engine: e}
	request := func(query string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		api.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/containers/"+c.ID+"/kill"+query, nil))
		return r
	}
	if r := request("?signal=SIGTERM"); r.Code != http.StatusNotImplemented || r.Header().Get("X-Docker-Zero-Unsupported") != "true" {
		t.Fatalf("unmodeled signal silently succeeded: %d", r.Code)
	}
	if !c.isRunning() {
		t.Fatal("unmodeled signal killed container")
	}
	if r := request(""); r.Code != http.StatusNoContent {
		t.Fatalf("kill: %d %s", r.Code, r.Body.String())
	}
	if c.stateSnapshot().ExitCode != 137 {
		t.Fatal("incorrect kill exit code")
	}
	before := c.stateSnapshot()
	if r := request(""); r.Code != http.StatusConflict {
		t.Fatalf("repeated kill: %d", r.Code)
	}
	if after := c.stateSnapshot(); after != before {
		t.Fatalf("rejected kill mutated state: %+v -> %+v", before, after)
	}
}
