package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIVersionRangeIsEnforced(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	api := &DockerAPI{engine: engine}
	for _, version := range []string{"1.24", "1.43", "1.30", "1.23", "1.9", "1.44", "1.100", "99.99", "0.99", "1.43e0", "1.43.0", "999999999999999999999999999999.1"} {
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v"+version+"/version", nil))
		want := http.StatusBadRequest
		if version == "1.24" || version == "1.43" || version == "1.30" {
			want = http.StatusOK
		}
		if response.Code != want {
			t.Errorf("%s: status=%d want=%d body=%s", version, response.Code, want, response.Body.String())
		}
	}
	for _, path := range []string{"/version", "/_ping", "/v1.24/_ping", "/v1.43/_ping"} {
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || response.Header().Get("Api-Version") != apiVersion {
			t.Errorf("%s: %d %+v", path, response.Code, response.Header())
		}
	}
}

func TestRejectedAPIVersionCannotMutateResources(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	api := &DockerAPI{engine: engine}
	response := httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v99.99/containers/create?name=version-rejected", strings.NewReader(`{"Image":"redis:alpine"}`)))
	if response.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", response.Code)
	}
	if _, err := engine.findContainer("version-rejected"); err == nil {
		t.Fatal("invalid API version created a container")
	}
}

func TestStripAPIVersionDoesNotTreatVersionsAsFloats(t *testing.T) {
	for _, path := range []string{"/v1.43e0/version", "/v1.4.3/version", "/v1.43", "/version"} {
		if got := stripAPIVersion(path); got != path {
			t.Errorf("malformed or unversioned path %q became %q", path, got)
		}
	}
	if got := stripAPIVersion("/v1.43/containers/json"); got != "/containers/json" {
		t.Fatal(got)
	}
}
