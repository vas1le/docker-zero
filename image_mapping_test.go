package main

import (
	"net/http/httptest"
	"testing"
)

func TestImageMappingDoesNotDependOnContainerName(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	for _, test := range []struct{ name, image, kind string }{
		{"nginx-named-redis", "redis:alpine", "redis"},
		{"redis-named-nginx", "nginx:alpine", "nginx"},
		{"neutral", "docker.io/library/redis:7-alpine", "redis"},
		{"neutral-two", "library/nginx:latest", "nginx"},
	} {
		c, err := e.createContainer(test.name, test.image, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.Kind != test.kind {
			t.Errorf("%s %s -> %s, want %s", test.name, test.image, c.Kind, test.kind)
		}
	}
}

func TestImageMappingRejectsSubstringAndNamespaceMatches(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	for _, image := range []string{"not-nginx:latest", "company/nginx:alpine", "evil.example/redis:alpine", "redis-backup:latest"} {
		if _, err := e.createContainer("nginx", image, nil, nil, nil); err == nil {
			t.Errorf("unknown image accepted: %s", image)
		}
		response := httptest.NewRecorder()
		(&DockerAPI{engine: e}).ServeHTTP(response, httptest.NewRequest("GET", "/images/"+image+"/json", nil))
		if response.Code != 404 {
			t.Errorf("unknown image inspect: %s -> %d", image, response.Code)
		}
	}
}

func TestExplicitCookbookOverride(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := e.createContainer("nginx-name", "company/custom-service:v1", map[string]string{cookbookOverrideLabel: "redis"}, nil, nil)
	if err != nil || c.Kind != "redis" {
		t.Fatalf("override: %#v %v", c, err)
	}
	if _, err = e.createContainer("unknown", "redis", map[string]string{cookbookOverrideLabel: "missing"}, nil, nil); err == nil {
		t.Fatal("unknown explicit override accepted")
	}
}

func TestImageMappingRespectsExplicitTags(t *testing.T) {
	for _, test := range []struct {
		image, candidate string
		match            bool
	}{
		{"nginx:alpine", "nginx", true},
		{"docker.io/library/nginx@sha256:abcd", "nginx", true},
		{"nginx:latest", "nginx:alpine", false},
		{"company/nginx:latest", "nginx", false},
		{"registry:5000/team/nginx:1", "registry:5000/team/nginx", true},
		{"registry:5001/team/nginx:1", "registry:5000/team/nginx", false},
	} {
		if got := imageMappingMatches(test.image, test.candidate); got != test.match {
			t.Errorf("%s / %s = %v", test.image, test.candidate, got)
		}
	}
}
