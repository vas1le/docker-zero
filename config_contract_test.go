package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestCreateConfigRoundTripsWithoutSyntheticReplacement(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	api := &DockerAPI{engine: engine}
	payload := `{"Image":"redis:alpine","Hostname":"chosen-host","User":"1000:1000","WorkingDir":"/app","Entrypoint":["/entry"],"Cmd":["worker"],"Tty":true,"ExposedPorts":{"9000/tcp":{}},"Healthcheck":{"Test":["CMD-SHELL","false"],"Interval":9007199254740993,"Timeout":2000000000,"Retries":7},"HostConfig":{"RestartPolicy":{"Name":"on-failure","MaximumRetryCount":7},"ReadonlyRootfs":true,"Memory":1073741824}}`
	response := httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/containers/create?name=config-contract", strings.NewReader(payload)))
	if response.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/containers/config-contract/json", nil))
	var doc, original map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(payload), &original); err != nil {
		t.Fatal(err)
	}
	var config, host map[string]json.RawMessage
	if err := json.Unmarshal(doc["Config"], &config); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(doc["HostConfig"], &host); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"Hostname", "User", "WorkingDir", "Entrypoint", "Cmd", "Tty", "Healthcheck"} {
		var got, want any
		// UseNumber catches truncation of Docker's int64 nanosecond fields.
		decode := func(raw []byte, target *any) {
			d := json.NewDecoder(strings.NewReader(string(raw)))
			d.UseNumber()
			if err := d.Decode(target); err != nil {
				t.Fatal(err)
			}
		}
		decode(config[field], &got)
		decode(original[field], &want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Config.%s = %s, want %s", field, config[field], original[field])
		}
	}
	if !strings.Contains(string(config["ExposedPorts"]), `"9000/tcp"`) {
		t.Error("explicit exposed port lost")
	}
	var policy struct {
		Name              string
		MaximumRetryCount int
	}
	if err := json.Unmarshal(host["RestartPolicy"], &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Name != "on-failure" || policy.MaximumRetryCount != 7 {
		t.Errorf("restart policy replaced: %+v", policy)
	}
	if string(host["ReadonlyRootfs"]) != "true" || string(host["Memory"]) != "1073741824" {
		t.Error("requested host configuration lost")
	}
	if !strings.Contains(string(doc["DockerZero"]), `"MetadataOnlyFields"`) {
		t.Error("unexecuted configuration must be identified as metadata-only")
	}
}

func TestDefaultRestartPolicyIsNo(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("default-policy", "redis:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	policy := containerInspectDocument(c, nil)["HostConfig"].(map[string]any)["RestartPolicy"]
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"Name":"no"`) {
		t.Fatalf("default policy = %s", data)
	}
}

func TestHealthcheckNoneHasNoFabricatedHealth(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	api := &DockerAPI{engine: engine}
	response := httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/containers/create?name=no-health", strings.NewReader(`{"Image":"redis:alpine","Healthcheck":{"Test":["NONE"]}}`)))
	c, err := engine.findContainer("no-health")
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	response = httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/containers/no-health/json", nil))
	var doc struct{ State map[string]any }
	if err := json.Unmarshal(response.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.State["Health"]; ok {
		t.Fatal("disabled healthcheck still reports simulated health")
	}
}

func TestInvalidRestartPolicyDoesNotCreateContainer(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	for _, policy := range []string{`{"Name":"bogus"}`, `{"Name":"on-failure","MaximumRetryCount":-1}`, `{"Name":"no","MaximumRetryCount":2}`} {
		response := httptest.NewRecorder()
		(&DockerAPI{engine: engine}).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/containers/create?name=bad-policy", strings.NewReader(`{"Image":"redis:alpine","HostConfig":{"RestartPolicy":`+policy+`}}`)))
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d", policy, response.Code)
		}
	}
	if len(engine.listContainers(true)) != 0 {
		t.Fatal("invalid configuration left a container behind")
	}
}
