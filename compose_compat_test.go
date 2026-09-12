package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func composeFilter(value string) string {
	data, _ := json.Marshal(map[string]map[string]bool{"label": {value: true}})
	return url.QueryEscape(string(data))
}

func TestComposeProjectFilteringAndNetworkingConfig(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer ledger.Close()
	api := &DockerAPI{engine: engine}

	for _, body := range []string{
		`{"Name":"project_default","Driver":"bridge","Labels":{"com.docker.compose.project":"project"}}`,
		`{"Name":"other_default","Driver":"bridge","Labels":{"com.docker.compose.project":"other"}}`,
	} {
		r := httptest.NewRequest(http.MethodPost, "/v1.43/networks/create", strings.NewReader(body))
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		if w.Code != http.StatusCreated {
			t.Fatalf("network create status=%d body=%s", w.Code, w.Body.String())
		}
	}

	create := `{
		"Image":"nginx:alpine",
		"Labels":{"com.docker.compose.project":"project","com.docker.compose.service":"nginx"},
		"HostConfig":{"NetworkMode":"project_default"},
		"NetworkingConfig":{"EndpointsConfig":{"project_default":{"Aliases":["nginx","project-nginx-1"]}}}
	}`
	r := httptest.NewRequest(http.MethodPost, "/v1.43/containers/create?name=project-nginx-1", strings.NewReader(create))
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("container create status=%d body=%s", w.Code, w.Body.String())
	}

	r = httptest.NewRequest(http.MethodGet, "/v1.43/containers/json?all=1&filters="+composeFilter("com.docker.compose.project=project"), nil)
	w = httptest.NewRecorder()
	api.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("container list status=%d body=%s", w.Code, w.Body.String())
	}
	var containers []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &containers); err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 {
		t.Fatalf("filtered containers=%d body=%s", len(containers), w.Body.String())
	}
	networks := containers[0]["NetworkSettings"].(map[string]any)["Networks"].(map[string]any)
	if _, ok := networks["project_default"]; !ok {
		t.Fatalf("project network missing from container list: %#v", networks)
	}
	if _, ok := networks["bridge"]; ok {
		t.Fatalf("container unexpectedly remains on bridge: %#v", networks)
	}

	r = httptest.NewRequest(http.MethodGet, "/v1.43/containers/project-nginx-1/json", nil)
	w = httptest.NewRecorder()
	api.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("inspect status=%d body=%s", w.Code, w.Body.String())
	}
	var inspect map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &inspect); err != nil {
		t.Fatal(err)
	}
	inspectNetworks := inspect["NetworkSettings"].(map[string]any)["Networks"].(map[string]any)
	if _, ok := inspectNetworks["project_default"]; !ok {
		t.Fatalf("project network missing from inspect: %#v", inspectNetworks)
	}

	r = httptest.NewRequest(http.MethodGet, "/v1.43/networks?filters="+composeFilter("com.docker.compose.project=project"), nil)
	w = httptest.NewRecorder()
	api.ServeHTTP(w, r)
	var networkList []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &networkList); err != nil {
		t.Fatal(err)
	}
	if len(networkList) != 1 || networkList[0]["Name"] != "project_default" {
		t.Fatalf("filtered networks=%s", w.Body.String())
	}
}

func TestComposeVolumeProjectFilter(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer ledger.Close()
	api := &DockerAPI{engine: engine}
	for _, body := range []string{
		`{"Name":"project_data","Labels":{"com.docker.compose.project":"project"}}`,
		`{"Name":"other_data","Labels":{"com.docker.compose.project":"other"}}`,
	} {
		r := httptest.NewRequest(http.MethodPost, "/v1.43/volumes/create", strings.NewReader(body))
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		if w.Code != http.StatusCreated {
			t.Fatalf("volume create status=%d body=%s", w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/v1.43/volumes?filters="+composeFilter("com.docker.compose.project=project"), nil)
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)
	var response struct {
		Volumes []Volume `json:"Volumes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Volumes) != 1 || response.Volumes[0].Name != "project_data" {
		t.Fatalf("filtered volumes=%s", w.Body.String())
	}
}
