package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIVersionBoundsAndMalformedPrefixes(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	api := &DockerAPI{engine: e}
	for _, prefix := range []string{"v1.23", "v1.5", "v1.44", "v1.100", "v2.0", "v99.99", "v1.43e0", "v1.43.0", "v+1.43", "v1.-1", "v1.99999999999999999999999999"} {
		for _, route := range []string{"/version", "/_ping", "/containers/create"} {
			method := http.MethodGet
			if route == "/containers/create" {
				method = http.MethodPost
			}
			r := httptest.NewRecorder()
			api.ServeHTTP(r, httptest.NewRequest(method, "/"+prefix+route, strings.NewReader(`{"Image":"nginx"}`)))
			if r.Code != http.StatusBadRequest {
				t.Fatalf("%s%s: %d %s", prefix, route, r.Code, r.Body.String())
			}
			if r.Header().Get("Api-Version") != apiVersion {
				t.Fatal("missing negotiation header")
			}
		}
	}
	if len(e.containers) != 0 {
		t.Fatal("version rejection still created containers")
	}
	for _, path := range []string{"/version", "/_ping", "/volumes", "/v1.24/version", "/v1.30/_ping", "/v1.43/version"} {
		r := httptest.NewRecorder()
		api.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		if r.Code != http.StatusOK {
			t.Fatalf("valid %s: %d %s", path, r.Code, r.Body.String())
		}
	}
}

func TestVersionComparisonIsNumericTupleNotFloat(t *testing.T) {
	v, _ := parseDockerAPIVersion("1.5")
	min, _ := parseDockerAPIVersion("1.24")
	high, _ := parseDockerAPIVersion("1.100")
	if !v.before(min) || !min.before(high) {
		t.Fatal("minor versions compared as fractions")
	}
}
