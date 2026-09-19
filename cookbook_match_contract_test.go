package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCookbookSelectionUsesImageIdentityNotContainerName(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	for _, tc := range []struct{ name, image, kind string }{
		{"nginx-named-redis", "redis:alpine", "redis"},
		{"redis-named-nginx", "nginx:alpine", "nginx"},
		{"web", "docker.io/library/nginx:alpine", "nginx"},
		{"cache", "library/redis:7-alpine", "redis"},
		{"cache", "redis@sha256:" + strings.Repeat("a", 64), "redis"},
		{"nginx", "company/not-nginx:latest", ""},
		{"nginx", "redis-commander:latest", ""},
		{"redis", "example.org/library/redis:alpine", ""},
	} {
		cb, err := engine.cookbookFor(tc.name, tc.image)
		if tc.kind == "" {
			if err == nil {
				t.Errorf("%s silently matched %s", tc.image, cb.Kind)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.image, err)
			continue
		}
		if cb.Kind != tc.kind {
			t.Errorf("%s (%s) matched %s, want %s", tc.name, tc.image, cb.Kind, tc.kind)
		}
	}
}

func TestImageInspectRejectsSubstringMatches(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	response := httptest.NewRecorder()
	(&DockerAPI{engine: engine}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/images/company/not-nginx:latest/json", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", response.Code)
	}
}

func TestExplicitImageMatchersAreSpecificAndUnambiguous(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	special := *engine.cookbooks["nginx"]
	special.Kind = "special"
	special.ImageNames = []string{"redis:Special", "localhost:5000/team/custom:1"}
	engine.cookbooks["special"] = &special
	for _, image := range []string{"redis:Special", "localhost:5000/team/custom:1"} {
		cb, err := engine.cookbookFor("ignored", image)
		if err != nil || cb.Kind != "special" {
			t.Fatalf("%s: cb=%v error=%v", image, cb, err)
		}
	}
	cb, err := engine.cookbookFor("ignored", "redis:special")
	if err != nil || cb.Kind != "redis" {
		t.Fatal("tag matching must be case-sensitive")
	}
	duplicate := special
	duplicate.Kind = "duplicate"
	engine.cookbooks["duplicate"] = &duplicate
	if _, err := engine.cookbookFor("ignored", "redis:Special"); err == nil {
		t.Fatal("ambiguous exact mapping accepted")
	}
}
