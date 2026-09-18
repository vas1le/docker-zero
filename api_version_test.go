package main

import (
	"net/http"
	"testing"
)

func TestAPIVersionBoundsAndParsing(t *testing.T) {
	api, _ := contractAPI(t, 0)
	for _, path := range []string{"/version", "/_ping", "/v1.24/version", "/v1.43/version", "/v1.30/_ping"} {
		response := contractRequest(api, "GET", path, "")
		if response.Code != http.StatusOK {
			t.Errorf("supported %s: %d", path, response.Code)
		}
	}
	for _, prefix := range []string{"v1.23", "v1.44", "v99.99", "v0.99", "v2.0", "v1.43e0", "v1.430", "v1.043", "v1.43.0", "v1", "v1.-2", "v999999999999999999999999.43"} {
		response := contractRequest(api, "POST", "/"+prefix+"/containers/create?name=forbidden", `{"Image":"nginx:alpine"}`)
		if response.Code != 400 {
			t.Errorf("invalid %s accepted/routed: %d %s", prefix, response.Code, response.Body.String())
		}
		if response.Header().Get("Api-Version") != "1.43" {
			t.Error("negotiation header missing")
		}
	}
	if _, err := api.engine.findContainer("forbidden"); err == nil {
		t.Fatal("invalid API version reached container creation")
	}
}
