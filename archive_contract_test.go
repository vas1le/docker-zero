package main

import (
	"net/http"
	"testing"
)

func TestArchiveOperationsAreExplicitlyUnsupported(t *testing.T) {
	api, container := contractAPI(t, 0)
	for _, method := range []string{http.MethodPut, http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			response := contractRequest(api, method, "/containers/contract/archive?path=/missing", "not a tar archive")
			if response.Code != http.StatusNotImplemented || response.Header().Get("X-Docker-Zero-Unsupported") != "true" {
				t.Fatalf("archive must not claim success: %d %s", response.Code, response.Body.String())
			}
		})
	}
	if container.eventCount("docker.archive") != 0 {
		t.Fatal("unsupported archive changed scenario state")
	}
	response := contractRequest(api, http.MethodPut, "/containers/missing/archive?path=/tmp", "invalid")
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing container: %d", response.Code)
	}
}
