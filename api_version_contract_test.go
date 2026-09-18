package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIVersionBounds(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	for _, test := range []struct {
		path   string
		status int
	}{
		{"/version", 200}, {"/v1.24/version", 200}, {"/v1.43/version", 200},
		{"/v1.23/version", 400}, {"/v1.9/version", 400}, {"/v1.44/version", 400}, {"/v99.99/version", 400}, {"/v2.0/_ping", 400},
	} {
		response := httptest.NewRecorder()
		(&DockerAPI{engine: e}).ServeHTTP(response, httptest.NewRequest("GET", test.path, nil))
		if response.Code != test.status {
			t.Errorf("%s -> %d, want %d", test.path, response.Code, test.status)
		}
	}
	response := httptest.NewRecorder()
	(&DockerAPI{engine: e}).ServeHTTP(response, httptest.NewRequest("POST", "/v99.99/containers/create", strings.NewReader(`{"Image":"nginx"}`)))
	if response.Code != 400 || len(e.listContainers(true)) != 0 {
		t.Fatal("unsupported API version mutated state")
	}
}

func TestMalformedAPIVersionNeverRoutesAsSupported(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	for _, prefix := range []string{"v1.43e0", "v1.43.0", "v1e0.43", "v1.+43", "v1.43x", "v9999999999999999999999999.43"} {
		response := httptest.NewRecorder()
		(&DockerAPI{engine: e}).ServeHTTP(response, httptest.NewRequest("GET", "/"+prefix+"/version", nil))
		if response.Code == 200 {
			t.Errorf("malformed version accepted: %s", prefix)
		}
	}
}
