package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestCreateConfigRoundTripsInsteadOfInventingDefaults(t *testing.T) {
	api, _ := contractAPI(t, 0)
	body := `{"Image":"nginx:alpine","Hostname":"test-host","User":"1000","WorkingDir":"/app","Entrypoint":["custom-entry"],"Healthcheck":{"Test":["CMD","false"],"Interval":1000000000,"Timeout":2000000000,"Retries":2},"HostConfig":{"RestartPolicy":{"Name":"on-failure","MaximumRetryCount":7}}}`
	response := contractRequest(api, http.MethodPost, "/containers/create?name=config", body)
	if response.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	var wanted map[string]any
	if err := json.Unmarshal([]byte(body), &wanted); err != nil {
		t.Fatal(err)
	}
	response = contractRequest(api, http.MethodGet, "/containers/config/json", "")
	var inspected map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	config := inspected["Config"].(map[string]any)
	for _, field := range []string{"Hostname", "User", "WorkingDir", "Entrypoint", "Healthcheck"} {
		if !reflect.DeepEqual(config[field], wanted[field]) {
			t.Errorf("Config.%s = %#v, want %#v", field, config[field], wanted[field])
		}
	}
	if got := inspected["HostConfig"].(map[string]any)["RestartPolicy"]; !reflect.DeepEqual(got, wanted["HostConfig"].(map[string]any)["RestartPolicy"]) {
		t.Errorf("RestartPolicy = %#v, want %#v", got, wanted["HostConfig"])
	}
}

func TestCreateMetadataOnlyCannotReportAnUnqualifiedSuccess(t *testing.T) {
	api, _ := contractAPI(t, 0)
	body := `{"Image":"nginx:alpine","User":"1000","Healthcheck":{"Test":["CMD","false"]},"HostConfig":{"Memory":9007199254740993,"Privileged":true}}`
	created := contractRequest(api, "POST", "/containers/create?name=metadata", body)
	if created.Code != 201 || created.Header().Get("X-Docker-Zero-Unsupported") != "true" {
		t.Fatalf("metadata create must be marked incomplete: %d %s %v", created.Code, created.Body.String(), created.Header())
	}
	if !strings.Contains(created.Body.String(), "does not execute/model") {
		t.Fatal("create omitted metadata-only warning")
	}
	contractRequest(api, "POST", "/containers/metadata/start", "")
	inspected := contractRequest(api, "GET", "/containers/metadata/json", "")
	if inspected.Header().Get("X-Docker-Zero-Unsupported") != "true" {
		t.Fatal("inspect lost incomplete marker")
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(inspected.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(doc["State"], &state); err != nil {
		t.Fatal(err)
	}
	if _, exists := state["Health"]; exists {
		t.Fatal("custom health command was never run: must not invent healthy status")
	}
	var host map[string]json.RawMessage
	if err := json.Unmarshal(doc["HostConfig"], &host); err != nil {
		t.Fatal(err)
	}
	if string(host["Memory"]) != "9007199254740993" {
		t.Fatalf("integer lost precision: %s", host["Memory"])
	}
}

func TestCreateHealthcheckDisableAndInheritance(t *testing.T) {
	for _, tc := range []struct {
		name, health string
		hasHealth    bool
	}{
		{"disabled", `{"Test":["NONE"]}`, false}, {"inherited", `null`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, _ := contractAPI(t, 0)
			response := contractRequest(api, "POST", "/containers/create?name=health", `{"Image":"nginx:alpine","Healthcheck":`+tc.health+`}`)
			if response.Code != 201 || response.Header().Get("X-Docker-Zero-Unsupported") != "" {
				t.Fatalf("create: %d %v", response.Code, response.Header())
			}
			contractRequest(api, "POST", "/containers/health/start", "")
			response = contractRequest(api, "GET", "/containers/health/json", "")
			var doc map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			_, hasHealth := doc["State"].(map[string]any)["Health"]
			if hasHealth != tc.hasHealth {
				t.Fatalf("State.Health present=%t want=%t", hasHealth, tc.hasHealth)
			}
		})
	}
}

func TestCreateDefaultsAndEmptyMetadata(t *testing.T) {
	api, _ := contractAPI(t, 0)
	response := contractRequest(api, "POST", "/containers/create?name=defaults", `{"Image":"nginx:alpine","Entrypoint":[],"User":"","ExposedPorts":{"8080/tcp":{}},"HostConfig":{"Privileged":false,"Memory":0}}`)
	if response.Code != 201 || response.Header().Get("X-Docker-Zero-Unsupported") != "" {
		t.Fatalf("create: %d %v", response.Code, response.Header())
	}
	response = contractRequest(api, "GET", "/containers/defaults/json", "")
	var doc map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	config := doc["Config"].(map[string]any)
	if _, ok := config["Entrypoint"].([]any); !ok {
		t.Fatalf("empty entrypoint became null: %v", config["Entrypoint"])
	}
	if _, ok := config["ExposedPorts"].(map[string]any)["8080/tcp"]; !ok {
		t.Fatal("lost exposed port")
	}
	policy := doc["HostConfig"].(map[string]any)["RestartPolicy"].(map[string]any)
	if policy["Name"] != "no" {
		t.Fatalf("invented restart policy: %v", policy)
	}
}

func TestCreateInvalidMetadataDoesNotAllocateContainer(t *testing.T) {
	for _, extra := range []string{
		`"HostConfig":{"RestartPolicy":{"Name":"sometimes"}}`,
		`"HostConfig":{"RestartPolicy":{"Name":"no","MaximumRetryCount":7}}`,
		`"HostConfig":{"RestartPolicy":{"Name":"on-failure","MaximumRetryCount":-1}}`,
		`"Healthcheck":{"Test":["NONE","extra"]}`, `"Healthcheck":{"Test":["CMD"]}`,
		`"Healthcheck":{"Test":["UNKNOWN","false"]}`, `"Healthcheck":{"Interval":-1}`,
	} {
		t.Run(extra, func(t *testing.T) {
			api, _ := contractAPI(t, 0)
			response := contractRequest(api, "POST", "/containers/create?name=invalid", `{"Image":"nginx:alpine",`+extra+`}`)
			if response.Code != 400 {
				t.Fatalf("create: %d %s", response.Code, response.Body.String())
			}
			if _, err := api.engine.findContainer("invalid"); err == nil {
				t.Fatal("invalid request allocated container")
			}
		})
	}
}

func TestComposeDefaultAttachmentAndConsoleSizeAreNotFalsePositives(t *testing.T) {
	api, _ := contractAPI(t, 0)
	response := contractRequest(api, "POST", "/containers/create?name=compose", `{"Image":"nginx:alpine","AttachStdout":true,"AttachStderr":true,"HostConfig":{"ConsoleSize":[0,0]}}`)
	if response.Code != 201 || response.Header().Get("X-Docker-Zero-Unsupported") != "" {
		t.Fatalf("ordinary Compose defaults incorrectly classified: %d %s", response.Code, response.Body.String())
	}
	response = contractRequest(api, "POST", "/containers/compose/attach", "")
	if response.Header().Get("X-Docker-Zero-Unsupported") != "true" {
		t.Fatal("actual unsupported attachment must still fail explicitly")
	}
	response = contractRequest(api, "POST", "/containers/create?name=console", `{"Image":"nginx:alpine","HostConfig":{"ConsoleSize":[24,80]}}`)
	if response.Code != 201 || response.Header().Get("X-Docker-Zero-Unsupported") != "true" {
		t.Fatal("non-default unmodeled console size must be flagged")
	}
}
