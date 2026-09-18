package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExecUnconfiguredCommandIsUnsupported(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("exec-unconfigured", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	response := httptest.NewRecorder()
	(&DockerAPI{engine: engine}).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/containers/"+c.ID+"/exec", strings.NewReader(`{"Cmd":["sh","-c","exit 42"]}`)))
	if response.Code != 501 || response.Header().Get("X-Docker-Zero-Unsupported") != "true" {
		t.Fatalf("unconfigured exec falsely accepted: %d %s", response.Code, response.Body.String())
	}
	engine.mu.RLock()
	count := len(engine.execs)
	engine.mu.RUnlock()
	if count != 0 {
		t.Fatalf("unsupported exec left %d records", count)
	}
}

func TestExecFixtureReturnsConfiguredFailureAndStreams(t *testing.T) {
	for _, detach := range []bool{false, true} {
		t.Run(fmt.Sprint(detach), func(t *testing.T) {
			engine, ledger := testEngine(t, 0)
			defer testCloseLedger(t, ledger)
			c, err := engine.createContainer("exec-fixture", "nginx:alpine", nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			c.start()
			c.Scenario.Exec = []ExecFixture{{Cmd: []string{"sh", "-c", "exit 42"}, Stdout: "out\n", Stderr: "failed\n", ExitCode: 42}}
			api := &DockerAPI{engine: engine}
			request := func(method, path, body string) *httptest.ResponseRecorder {
				response := httptest.NewRecorder()
				api.ServeHTTP(response, httptest.NewRequest(method, path, strings.NewReader(body)))
				return response
			}
			response := request("POST", "/containers/"+c.ID+"/exec", `{"Cmd":["sh","-c","exit 42"],"AttachStdout":true,"AttachStderr":true}`)
			if response.Code != 201 {
				t.Fatalf("create = %d %s", response.Code, response.Body.String())
			}
			var created struct{ Id string }
			if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			inspect := request("GET", "/exec/"+created.Id+"/json", "")
			var status struct {
				ExitCode *int
				Running  bool
			}
			if err := json.Unmarshal(inspect.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			if status.ExitCode != nil {
				t.Fatal("exec reported an exit code before starting")
			}
			response = request("POST", "/exec/"+created.Id+"/start", fmt.Sprintf(`{"Detach":%t}`, detach))
			if response.Code != 200 {
				t.Fatalf("start = %d %s", response.Code, response.Body.String())
			}
			if detach {
				if response.Body.Len() != 0 {
					t.Fatal("detached exec returned stream data")
				}
			} else {
				body := response.Body.Bytes()
				for i, expected := range []string{"out\n", "failed\n"} {
					if len(body) < 8 {
						t.Fatal("missing stream header")
					}
					length := int(binary.BigEndian.Uint32(body[4:8]))
					if len(body) < 8+length || body[0] != byte(i+1) || string(body[8:8+length]) != expected {
						t.Fatalf("bad frame: %q", body)
					}
					body = body[8+length:]
				}
				if len(body) != 0 {
					t.Fatalf("unexpected trailing stream data: %q", body)
				}
			}
			inspect = request("GET", "/exec/"+created.Id+"/json", "")
			if err := json.Unmarshal(inspect.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			if status.ExitCode == nil || *status.ExitCode != 42 || status.Running {
				t.Fatalf("completed exec = %s", inspect.Body.String())
			}
			response = request("POST", "/exec/"+created.Id+"/start", `{}`)
			if response.Code != 409 {
				t.Fatalf("second start = %d", response.Code)
			}
		})
	}
}

func TestExecFixtureMatchingUsesArgvNotJoinedText(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("exec-argv", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	c.Scenario.Exec = []ExecFixture{{Cmd: []string{"echo", "a b"}, ExitCode: 0}}
	if _, err := engine.createExec(c, execCreateRequest{Cmd: []string{"echo", "a", "b"}}); err == nil {
		t.Fatal("argument boundaries were ignored")
	}
	command := []string{"echo", "a b"}
	exec, err := engine.createExec(c, execCreateRequest{Cmd: command})
	if err != nil {
		t.Fatal("exact command did not match")
	}
	command[1] = "mutated"
	if exec.Command[1] != "a b" {
		t.Fatal("exec retains caller-owned command storage")
	}
}

func TestExecRejectsUnmodeledOptions(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("exec-options", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	c.Scenario.Exec = []ExecFixture{{Cmd: []string{"true"}, ExitCode: 0}}
	for _, body := range []string{`{"Cmd":["true"],"Tty":true}`, `{"Cmd":["true"],"Privileged":true}`, `{"Cmd":["true"],"Env":["X=1"]}`, `{"Cmd":["true"],"AttachStdin":true}`} {
		response := httptest.NewRecorder()
		(&DockerAPI{engine: engine}).ServeHTTP(response, httptest.NewRequest("POST", "/containers/"+c.ID+"/exec", strings.NewReader(body)))
		if response.Code != 501 || response.Header().Get("X-Docker-Zero-Unsupported") != "true" {
			t.Fatalf("options accepted: %s -> %d", body, response.Code)
		}
	}
}

func TestExecFixtureValidation(t *testing.T) {
	for _, fixtures := range [][]ExecFixture{
		{{Cmd: nil}},
		{{Cmd: []string{""}}},
		{{Cmd: []string{"bad\x00arg"}}},
		{{Cmd: []string{"true"}, ExitCode: -1}},
		{{Cmd: []string{"true"}, ExitCode: 256}},
		{{Cmd: []string{"true"}}, {Cmd: []string{"true"}}},
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
	scenario.Exec = []ExecFixture{{Cmd: []string{"migrate"}, Stderr: "failure", ExitCode: 42}}
	cb.Seeds["0"] = scenario
	data, err := json.Marshal(cb)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := decodeCookbook("fixture.json", data)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Seeds["0"].Exec[0].ExitCode != 42 {
		t.Fatal("fixture did not round-trip through cookbook decoder")
	}
}
