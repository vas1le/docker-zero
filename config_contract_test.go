package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestCreateConfigRoundTripsAndReportsMetadataOnly(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	api := &DockerAPI{engine: e}
	body := `{"Image":"nginx:alpine","Hostname":"requested-host","User":"1000","WorkingDir":"/app","Entrypoint":["/custom-entry"],"Cmd":["serve"],"Healthcheck":{"Test":["CMD","false"],"Interval":9000000000,"StartPeriod":2000000000,"Retries":7},"ExposedPorts":{"1234/tcp":{}},"HostConfig":{"RestartPolicy":{"Name":"on-failure","MaximumRetryCount":7},"Memory":9007199254740993}}`
	r := httptest.NewRecorder()
	api.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/containers/create?name=config-roundtrip", strings.NewReader(body)))
	if r.Code != http.StatusCreated || r.Header().Get("X-Docker-Zero-Unsupported") != "true" {
		t.Fatalf("create: %d %s", r.Code, r.Body.String())
	}
	var created struct {
		Id       string
		Warnings []string
	}
	if err := json.Unmarshal(r.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if len(created.Warnings) == 0 {
		t.Fatal("metadata-only settings were silently accepted")
	}
	r = httptest.NewRecorder()
	api.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/containers/"+created.Id+"/json", nil))
	var result struct {
		Config     map[string]json.RawMessage
		HostConfig map[string]json.RawMessage
		Path       string
		Args       []string
		DockerZero map[string]any
	}
	if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var original map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &original); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"Hostname", "User", "WorkingDir", "Entrypoint", "Cmd", "Healthcheck"} {
		var got, want any
		if err := json.Unmarshal(result.Config[key], &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(original[key], &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %v != %v", key, got, want)
		}
	}
	if string(result.HostConfig["Memory"]) != "9007199254740993" {
		t.Fatalf("numeric precision lost: %s", result.HostConfig["Memory"])
	}
	var policy RestartPolicy
	if err := json.Unmarshal(result.HostConfig["RestartPolicy"], &policy); err != nil {
		t.Fatal(err)
	}
	if policy != (RestartPolicy{Name: "on-failure", MaximumRetryCount: 7}) {
		t.Fatal(policy)
	}
	if result.Path != "/custom-entry" || !reflect.DeepEqual(result.Args, []string{"serve"}) {
		t.Fatal(result.Path, result.Args)
	}
	var ports map[string]any
	if err := json.Unmarshal(result.Config["ExposedPorts"], &ports); err != nil {
		t.Fatal(err)
	}
	if _, ok := ports["1234/tcp"]; !ok {
		t.Fatal("requested exposed port lost")
	}
	if result.DockerZero["Configuration"] == nil {
		t.Fatal("missing capability report")
	}
}

func TestStrictConfigRejectsBeforeAllocating(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	e.strict = true
	for _, body := range []string{
		`{"Image":"nginx","User":"1000"}`,
		`{"Image":"nginx","Healthcheck":{"Test":["CMD","false"]}}`,
		`{"Image":"nginx","HostConfig":{"Privileged":true}}`,
		`{"Image":"nginx","Volumes":{"/data":{}}}`,
		`{"Image":"nginx","Healthcheck":{"Test":["NONE"],"UnknownOption":true}}`,
		`{"Image":"nginx","HostConfig":{"RestartPolicy":{"Name":"no","UnknownOption":true}}}`,
		`{"Image":"nginx","NetworkingConfig":{"EndpointsConfig":{"bridge":{"IPAMConfig":{"IPv4Address":"10.1.2.3"}}}}}`,
	} {
		r := httptest.NewRecorder()
		(&DockerAPI{engine: e}).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/containers/create?name=strict-config", strings.NewReader(body)))
		if r.Code != http.StatusNotImplemented || r.Header().Get("X-Docker-Zero-Unsupported") != "true" {
			t.Fatalf("strict config accepted: %s -> %d %s", body, r.Code, r.Body.String())
		}
		if len(e.containers) != 0 || len(e.names) != 0 {
			t.Fatal("rejected configuration left container state")
		}
	}
}

func TestDisabledHealthcheckIsNotReplacedWithFixtureHealth(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	e.strict = true
	r := httptest.NewRecorder()
	(&DockerAPI{engine: e}).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/containers/create?name=no-health", strings.NewReader(`{"Image":"nginx","Healthcheck":{"Test":["NONE"]}}`)))
	if r.Code != http.StatusCreated {
		t.Fatalf("disabled healthcheck: %d %s", r.Code, r.Body.String())
	}
	c, err := e.findContainer("no-health")
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	c.applyPatch(StatePatch{Health: stringPtr("healthy")})
	if c.stateSnapshot().Health != "" {
		t.Fatal("NONE healthcheck still reports simulated health")
	}
	if _, ok := containerInspectDocument(c, nil)["State"].(map[string]any)["Health"]; ok {
		t.Fatal("disabled healthcheck appeared in inspect")
	}
}

func TestInvalidConfigurationDoesNotReserveName(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	for _, body := range []string{`null`, `{"Image":"nginx","User":123}`, `{"Image":"nginx","WorkingDir":"relative"}`, `{"Image":"nginx","Healthcheck":{"Test":["BAD"]}}`, `{"Image":"nginx","Healthcheck":{"Test":["NONE","extra"]}}`, `{"Image":"nginx","Healthcheck":{"Interval":-1}}`, `{"Image":"nginx","HostConfig":{"RestartPolicy":{"Name":"invalid"}}}`, `{"Image":"nginx","HostConfig":{"RestartPolicy":{"Name":"always","MaximumRetryCount":2}}}`} {
		r := httptest.NewRecorder()
		(&DockerAPI{engine: e}).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/containers/create?name=invalid-config", strings.NewReader(body)))
		if r.Code != http.StatusBadRequest || len(e.names) != 0 {
			t.Fatalf("invalid configuration: %d %s", r.Code, r.Body.String())
		}
	}
}

func TestEmptyEntrypointAndDefaultRestartPolicyRoundTrip(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	api := &DockerAPI{engine: e}
	r := httptest.NewRecorder()
	api.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/containers/create?name=empty-config", strings.NewReader(`{"Image":"nginx","Entrypoint":[]}`)))
	if r.Code != http.StatusCreated {
		t.Fatal(r.Body.String())
	}
	c, err := e.findContainer("empty-config")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(containerInspectDocument(c, nil))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Config     struct{ Entrypoint []string }
		HostConfig struct{ RestartPolicy RestartPolicy }
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Config.Entrypoint == nil || doc.HostConfig.RestartPolicy.Name != "no" {
		t.Fatal(string(data))
	}
}

func TestStrictFlagReachesOptions(t *testing.T) {
	opts, err := parseOptions([]string{"--strict"}, &strings.Builder{})
	if err != nil || !opts.strict {
		t.Fatalf("strict option: %+v %v", opts, err)
	}
}

func TestStrictConfigAcceptsNeutralComposeDefaults(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	e.strict = true
	for i, body := range []string{
		`{"Image":"nginx","AttachStdout":true,"AttachStderr":true,"HostConfig":{"ConsoleSize":[0,0],"LogConfig":{"Type":"","Config":{}}}}`,
		`{"Image":"redis","Cmd":["redis-server","--replicaof","master","6379"]}`,
	} {
		r := httptest.NewRecorder()
		(&DockerAPI{engine: e}).ServeHTTP(r, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/containers/create?name=neutral-%d", i), strings.NewReader(body)))
		if r.Code != http.StatusCreated || r.Header().Get("X-Docker-Zero-Unsupported") != "" {
			t.Fatalf("modeled or neutral config: %d %s", r.Code, r.Body.String())
		}
	}
	for _, body := range []string{
		`{"Image":"nginx","HostConfig":{"ConsoleSize":[80,24]}}`,
		`{"Image":"nginx","HostConfig":{"LogConfig":{"Type":"syslog"}}}`,
		`{"Image":"nginx","Cmd":["redis-server","--replicaof","master","6379"]}`,
		`{"Image":"redis","Cmd":["redis-server","--replicaof","master","6379","--unknown"]}`,
	} {
		r := httptest.NewRecorder()
		(&DockerAPI{engine: e}).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/containers/create?name=nonneutral", strings.NewReader(body)))
		if r.Code != http.StatusNotImplemented {
			t.Fatalf("unmodeled non-default config accepted: %s -> %d", body, r.Code)
		}
	}
}
