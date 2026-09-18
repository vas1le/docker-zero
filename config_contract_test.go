package main

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestContainerConfigurationRoundTrips(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	api := &DockerAPI{engine: e}
	body := `{"Image":"nginx:alpine","Hostname":"requested-host","User":"1000","WorkingDir":"/app","Entrypoint":["/entry"],"Cmd":["run"],"Healthcheck":{"Test":["CMD","false"],"Interval":3000000000,"Retries":7},"ExposedPorts":{"8081/tcp":{}},"HostConfig":{"RestartPolicy":{"Name":"on-failure","MaximumRetryCount":7},"Memory":123456789}}`
	response := httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest("POST", "/containers/create?name=config", strings.NewReader(body)))
	if response.Code != 201 {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	var created struct {
		Id       string
		Warnings []string
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest("GET", "/containers/"+created.Id+"/json", nil))
	var doc map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	config := doc["Config"].(map[string]any)
	for key, want := range map[string]any{"Hostname": "requested-host", "User": "1000", "WorkingDir": "/app", "Entrypoint": []any{"/entry"}, "Healthcheck": map[string]any{"Test": []any{"CMD", "false"}, "Interval": float64(3000000000), "Retries": float64(7)}} {
		if !reflect.DeepEqual(config[key], want) {
			t.Errorf("%s = %#v; want %#v", key, config[key], want)
		}
	}
	host := doc["HostConfig"].(map[string]any)
	if !reflect.DeepEqual(host["RestartPolicy"], map[string]any{"Name": "on-failure", "MaximumRetryCount": float64(7)}) {
		t.Errorf("restart policy replaced: %#v", host["RestartPolicy"])
	}
	if host["Memory"] != float64(123456789) {
		t.Errorf("memory discarded: %v", host["Memory"])
	}
	if _, ok := config["ExposedPorts"].(map[string]any)["8081/tcp"]; !ok {
		t.Error("requested exposed port lost")
	}
	if doc["Path"] != "/entry" || !reflect.DeepEqual(doc["Args"], []any{"run"}) {
		t.Errorf("entrypoint/args lost: %v %v", doc["Path"], doc["Args"])
	}
	if len(created.Warnings) == 0 {
		t.Error("metadata-only settings were not disclosed")
	}
}

func TestStrictConfigurationRejectsBeforeCreatingResources(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	e.strictConfig = true
	api := &DockerAPI{engine: e}
	for _, body := range []string{`{"Image":"nginx","User":"1000"}`, `{"Image":"nginx","HostConfig":{"Privileged":true}}`, `{"Image":"nginx","Healthcheck":{"Test":["CMD","false"]}}`, `{"Image":"nginx","NetworkingConfig":{"EndpointsConfig":{"bridge":{"IPAMConfig":{"IPv4Address":"10.0.0.9"}}}}}`} {
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest("POST", "/containers/create", strings.NewReader(body)))
		if response.Code != 501 || response.Header().Get("X-Docker-Zero-Unsupported") != "true" {
			t.Fatalf("strict accepted %s: %d %s", body, response.Code, response.Body.String())
		}
	}
	if len(e.listContainers(true)) != 0 {
		t.Fatal("strict rejection left containers")
	}
	response := httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest("GET", "/__docker_zero/capabilities", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"strict_container_config":true`) {
		t.Fatalf("missing capability report: %s", response.Body.String())
	}
}

func TestHealthcheckNoneAndDefaultRestartPolicy(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	api := &DockerAPI{engine: e}
	response := httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest("POST", "/containers/create?name=disabled-health", strings.NewReader(`{"Image":"nginx","Healthcheck":{"Test":["NONE"]}}`)))
	if response.Code != 201 {
		t.Fatal(response.Body.String())
	}
	c, err := e.findContainer("disabled-health")
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	response = httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest("GET", "/containers/disabled-health/json", nil))
	var doc struct {
		State      map[string]any
		HostConfig struct{ RestartPolicy restartPolicy }
	}
	if err := json.Unmarshal(response.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.State["Health"]; ok {
		t.Fatal("disabled healthcheck still reported health")
	}
	if doc.HostConfig.RestartPolicy.Name != "no" {
		t.Fatal("default restart policy isn't no")
	}
}

func TestInvalidConfigurationRejected(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	for _, extra := range []string{`"HostConfig":{"RestartPolicy":{"Name":"typo"}}`, `"HostConfig":{"RestartPolicy":{"Name":"always","MaximumRetryCount":1}}`, `"HostConfig":{"RestartPolicy":{"Name":"on-failure","MaximumRetryCount":-1}}`, `"Healthcheck":{"Test":["nonsense"]}`, `"Healthcheck":{"Test":["NONE","false"]}`, `"Healthcheck":{"Timeout":-1}`, `"ExposedPorts":{"broken/tcp":{}}`} {
		response := httptest.NewRecorder()
		(&DockerAPI{engine: e}).ServeHTTP(response, httptest.NewRequest("POST", "/containers/create", strings.NewReader(`{"Image":"nginx",`+extra+`}`)))
		if response.Code != 400 {
			t.Fatalf("invalid config accepted: %s -> %d", extra, response.Code)
		}
	}
	if len(e.listContainers(true)) != 0 {
		t.Fatal("invalid config leaked containers")
	}
}
