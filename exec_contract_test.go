package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func TestExecUnconfiguredCommandIsExplicitlyUnsupported(t *testing.T) {
	api, container := contractAPI(t, 0)
	container.start()
	response := contractRequest(api, http.MethodPost, "/containers/contract/exec", `{"Cmd":["sh","-c","exit 42"]}`)
	if response.Code != http.StatusNotImplemented || response.Header().Get("X-Docker-Zero-Unsupported") != "true" {
		t.Fatalf("unconfigured exec must not pretend to succeed: %d %s", response.Code, response.Body.String())
	}
	if len(api.engine.execs) != 0 {
		t.Fatal("unsupported exec allocated an instance")
	}
}

func TestExecRejectsInactiveContainer(t *testing.T) {
	for _, state := range []string{"created", "exited", "paused", "restarting"} {
		t.Run(state, func(t *testing.T) {
			api, container := contractAPI(t, 0)
			container.applyPatch(StatePatch{Status: stringPtr(state)})
			response := contractRequest(api, http.MethodPost, "/containers/contract/exec", `{"Cmd":["true"]}`)
			if response.Code != http.StatusConflict {
				t.Fatalf("exec in %s: status %d, want 409", state, response.Code)
			}
		})
	}
}

func fixtureExec(t *testing.T, api *DockerAPI, container *Container) string {
	t.Helper()
	container.Scenario.Exec = []ExecFixture{{Command: []string{"sh", "-c", "exit 42"}, Stdout: "out\n", Stderr: "failed\n", ExitCode: 42}}
	response := contractRequest(api, http.MethodPost, "/containers/contract/exec", `{"Cmd":["sh","-c","exit 42"],"AttachStdout":true,"AttachStderr":true}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("create fixture exec: %d %s", response.Code, response.Body.String())
	}
	var result struct{ Id string }
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.Id
}

func TestExecFixtureReportsFailureAndSeparateStreams(t *testing.T) {
	api, container := contractAPI(t, 0)
	container.start()
	id := fixtureExec(t, api, container)
	response := contractRequest(api, http.MethodPost, "/exec/"+id+"/start", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("start: %d %s", response.Code, response.Body.String())
	}
	var expected bytes.Buffer
	writeDockerStreamFrame(&expected, 1, []byte("out\n"))
	writeDockerStreamFrame(&expected, 2, []byte("failed\n"))
	if !bytes.Equal(expected.Bytes(), response.Body.Bytes()) {
		t.Fatalf("stream frames = %q, want %q", response.Body.Bytes(), expected.Bytes())
	}
	response = contractRequest(api, http.MethodGet, "/exec/"+id+"/json", "")
	var inspected struct {
		ExitCode int
		Running  bool
	}
	if err := json.Unmarshal(response.Body.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	if inspected.ExitCode != 42 || inspected.Running {
		t.Fatalf("fixture failure was hidden: %+v", inspected)
	}
	response = contractRequest(api, http.MethodPost, "/exec/"+id+"/start", `{}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("already-run exec must fail: %d", response.Code)
	}
}

func TestExecFixtureRequiresExactArguments(t *testing.T) {
	api, container := contractAPI(t, 0)
	container.start()
	container.Scenario.Exec = []ExecFixture{{Command: []string{"echo", "a b"}}}
	for _, body := range []string{`{"Cmd":["echo","a","b"]}`, `{"Cmd":["echo"]}`, `{"Cmd":["echo","a b","extra"]}`} {
		response := contractRequest(api, http.MethodPost, "/containers/contract/exec", body)
		if response.Code != http.StatusNotImplemented {
			t.Fatalf("non-exact command accepted: %s -> %d", body, response.Code)
		}
	}
}

func TestExecStartValidatesStateAndOptionsWithoutRunning(t *testing.T) {
	for _, state := range []string{"exited", "paused", "restarting", "removed"} {
		t.Run(state, func(t *testing.T) {
			api, container := contractAPI(t, 0)
			container.start()
			id := fixtureExec(t, api, container)
			want := http.StatusConflict
			if state == "removed" {
				if err := api.engine.removeContainer(container.ID, true); err != nil {
					t.Fatal(err)
				}
				want = http.StatusNotFound
			} else {
				container.applyPatch(StatePatch{Status: stringPtr(state)})
			}
			response := contractRequest(api, http.MethodPost, "/exec/"+id+"/start", `{}`)
			if response.Code != want {
				t.Fatalf("exec start in %s: %d, want %d", state, response.Code, want)
			}
			exec, err := api.engine.findExec(id)
			if err != nil {
				t.Fatal(err)
			}
			if exec.Started {
				t.Fatal("rejected start ran fixture")
			}
		})
	}
	api, container := contractAPI(t, 0)
	container.start()
	id := fixtureExec(t, api, container)
	for _, body := range []string{`{"Tty":true}`, `{"NotSupported":true}`} {
		response := contractRequest(api, http.MethodPost, "/exec/"+id+"/start", body)
		if response.Code != http.StatusNotImplemented || response.Header().Get("X-Docker-Zero-Unsupported") != "true" {
			t.Fatalf("unsupported option accepted: %s -> %d", body, response.Code)
		}
	}
	response := contractRequest(api, http.MethodPost, "/exec/"+id+"/start", `{"Detach":true}`)
	if response.Code != http.StatusOK || response.Body.Len() != 0 {
		t.Fatalf("detached fixture: %d %q", response.Code, response.Body.Bytes())
	}
}

func TestExecFixtureValidation(t *testing.T) {
	for _, fixtures := range [][]ExecFixture{
		{{Command: nil}},
		{{Command: []string{""}}},
		{{Command: []string{"true"}, ExitCode: -1}},
		{{Command: []string{"true"}, ExitCode: 256}},
		{{Command: []string{"echo", "\x00"}}},
		{{Command: []string{"true"}}, {Command: []string{"true"}}},
	} {
		if err := validateExecFixtures("/seeds/0/exec", fixtures); err == nil {
			t.Fatalf("invalid fixtures accepted: %#v", fixtures)
		}
	}
	cookbooks, err := loadCookbooks("")
	if err != nil {
		t.Fatal(err)
	}
	cb := cookbooks["nginx"]
	scenario := cb.Seeds["0"]
	scenario.Exec = []ExecFixture{{Command: []string{"true"}, ExitCode: 0}}
	cb.Seeds["0"] = scenario
	data, err := json.Marshal(cb)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeCookbook("fixture.json", data)
	if err != nil || len(decoded.Seeds["0"].Exec) != 1 {
		t.Fatalf("cookbook fixture round-trip: %v", err)
	}
}
