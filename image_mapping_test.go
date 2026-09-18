package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImageMappingIgnoresIncidentalContainerName(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	for _, image := range []string{"redis", "redis:alpine", "library/redis:7", "docker.io/library/redis:7", "index.docker.io/library/redis@sha256:abc"} {
		cb, err := e.cookbookFor("nginx-named-redis", image)
		if err != nil || cb.Kind != "redis" {
			t.Fatalf("%q mapped incorrectly: %+v %v", image, cb, err)
		}
	}
	for _, image := range []string{"notredis:alpine", "acme/redis:alpine", "registry.test/redis", "custom-nginx", "unknown:1"} {
		if cb, err := e.cookbookFor("nginx-zero", image); err == nil {
			t.Fatalf("%q unexpectedly mapped to %s", image, cb.Kind)
		}
	}
}

func TestExplicitImageRegistrations(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	e.cookbooks["nginx"].ImageNames = append(e.cookbooks["nginx"].ImageNames, "localhost:5000/acme/web", "acme/pinned:Prod")
	for _, image := range []string{"localhost:5000/acme/web:v2", "localhost:5000/acme/web@sha256:abc", "acme/pinned:Prod"} {
		cb, err := e.cookbookFor("redis", image)
		if err != nil || cb.Kind != "nginx" {
			t.Fatalf("registered %q: %+v %v", image, cb, err)
		}
	}
	for _, image := range []string{"localhost:5001/acme/web:v2", "acme/pinned:prod", "acme/pinned:other"} {
		if _, err := e.cookbookFor("nginx", image); err == nil {
			t.Fatalf("matched unregistered reference %q", image)
		}
	}
	e.cookbooks["redis"].ImageNames = append(e.cookbooks["redis"].ImageNames, "docker.io/library/nginx")
	if _, err := e.cookbookFor("nginx", "nginx:latest"); err == nil {
		t.Fatal("ambiguous mappings silently chose a model")
	}
}

func TestUnmappedImagesFailClosedAcrossAPI(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	for _, test := range []struct{ method, path, body string }{
		{http.MethodPost, "/containers/create?name=nginx", `{"Image":"acme/redis"}`},
		{http.MethodGet, "/images/acme%2Fredis/json", ""},
		{http.MethodPost, "/images/create?fromImage=acme%2Fredis&tag=latest", ""},
	} {
		r := httptest.NewRecorder()
		(&DockerAPI{engine: e}).ServeHTTP(r, httptest.NewRequest(test.method, test.path, strings.NewReader(test.body)))
		if r.Code != http.StatusNotImplemented || r.Header().Get("X-Docker-Zero-Unsupported") != "true" {
			t.Fatalf("%s: %d %s", test.path, r.Code, r.Body.String())
		}
	}
	if len(e.containers) != 0 {
		t.Fatal("unmapped image allocated a container")
	}
}
