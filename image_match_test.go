package main

import "testing"

func TestCookbookSelectionUsesImageNotContainerName(t *testing.T) {
	api, _ := contractAPI(t, 0)
	for _, tc := range []struct{ name, image, kind string }{
		{"nginx-named-redis", "redis:alpine", "redis"},
		{"redis-named-nginx", "nginx:alpine", "nginx"},
		{"app", "docker.io/library/redis:7-alpine", "redis"},
		{"app", "redis@sha256:abcd", "redis"},
	} {
		cb, err := api.engine.cookbookFor(tc.name, tc.image)
		if err != nil || cb.Kind != tc.kind {
			t.Errorf("%s %s: cookbook=%v error=%v", tc.name, tc.image, cb, err)
		}
	}
	for _, image := range []string{"notredis:latest", "myorg/redis:latest", "registry.example:5000/redis:latest", "nginx-unrelated:latest"} {
		if _, err := api.engine.cookbookFor("nginx-redis", image); err == nil {
			t.Errorf("unregistered image %s must not match", image)
		}
	}
}

func TestCookbookImageSpecificityAndAmbiguity(t *testing.T) {
	api, _ := contractAPI(t, 0)
	api.engine.cookbooks = map[string]*Cookbook{
		"generic":  {Kind: "generic", ImageNames: []string{"org/service"}},
		"specific": {Kind: "specific", ImageNames: []string{"org/service:v2"}},
	}
	for image, kind := range map[string]string{"org/service:v2": "specific", "org/service:v3": "generic", "org/service:v2@sha256:123": "generic"} {
		cb, err := api.engine.cookbookFor("anything", image)
		if err != nil || cb.Kind != kind {
			t.Errorf("%s: %v %v", image, cb, err)
		}
	}
	api.engine.cookbooks["duplicate"] = &Cookbook{Kind: "duplicate", ImageNames: []string{"docker.io/org/service:v2"}}
	if _, err := api.engine.cookbookFor("specific", "org/service:v2"); err == nil {
		t.Fatal("ambiguous equal-specificity mapping must fail")
	}
}
