package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestArchiveOperationsFailClosed(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("archive-test", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodPut, http.MethodHead, http.MethodGet} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(method, "/containers/"+c.ID+"/archive?path=/missing", strings.NewReader("not a tar archive"))
		(&DockerAPI{engine: engine}).ServeHTTP(response, request)
		if response.Code != http.StatusNotImplemented || response.Header().Get("X-Docker-Zero-Unsupported") != "true" {
			t.Errorf("%s archive = %d %s", method, response.Code, response.Body.String())
		}
	}
	if c.eventCount("docker.archive") != 0 {
		t.Fatal("unsupported archive advanced the scenario")
	}
}
