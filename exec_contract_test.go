package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExecCannotFabricateSuccessfulExecution(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("exec-contract", "redis:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	api := &DockerAPI{engine: engine}
	for _, state := range []string{"created", "running", "paused", "exited", "restarting"} {
		c.applyPatch(StatePatch{Status: stringPtr(state)})
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/containers/exec-contract/exec", strings.NewReader(`{"Cmd":["sh","-c","exit 42"]}`)))
		expected := http.StatusConflict
		if state == "running" {
			expected = http.StatusNotImplemented
		}
		if response.Code != expected {
			t.Errorf("%s: status=%d body=%s, want %d", state, response.Code, response.Body.String(), expected)
		}
		if state == "running" && response.Header().Get("X-Docker-Zero-Unsupported") != "true" {
			t.Error("missing unsupported marker")
		}
	}
}

func TestExecRejectsMissingCommand(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("exec-empty", "redis:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	for _, body := range []string{`{}`, `{"Cmd":[]}`, `{"Cmd":[""]}`, `not-json`} {
		response := httptest.NewRecorder()
		(&DockerAPI{engine: engine}).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/containers/exec-empty/exec", strings.NewReader(body)))
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d, want 400", body, response.Code)
		}
	}
}
