package main

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExecFixtureFailureAndSeparateStreams(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := e.createContainer("exec-fixture", "nginx", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Scenario.Exec = []ExecFixture{{Cmd: []string{"sh", "-c", "exit 42"}, Stdout: "starting\n", Stderr: "failed\n", ExitCode: intPtr(42)}}
	c.start()
	api := &DockerAPI{engine: e}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		api.ServeHTTP(r, httptest.NewRequest(method, path, strings.NewReader(body)))
		return r
	}
	r := call(http.MethodPost, "/containers/"+c.ID+"/exec", `{"Cmd":["sh","-c","exit 42"],"AttachStdout":true,"AttachStderr":true,"Detach":false}`)
	if r.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.Code, r.Body.String())
	}
	var created struct{ Id string }
	if err := json.Unmarshal(r.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	inspect := func() map[string]any {
		r := call(http.MethodGet, "/exec/"+created.Id+"/json", "")
		var doc map[string]any
		if err := json.Unmarshal(r.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	if inspect()["ExitCode"] != nil {
		t.Fatal("unstarted exec has a successful exit status")
	}
	r = call(http.MethodPost, "/exec/"+created.Id+"/start", `{"Detach":false,"Tty":false}`)
	if r.Code != http.StatusOK {
		t.Fatalf("start: %d %s", r.Code, r.Body.String())
	}
	payload := r.Body.Bytes()
	for i, expected := range []string{"starting\n", "failed\n"} {
		if len(payload) < 8 {
			t.Fatal("missing stream header")
		}
		size := int(binary.BigEndian.Uint32(payload[4:8]))
		if payload[0] != byte(i+1) || size > len(payload)-8 || string(payload[8:8+size]) != expected {
			t.Fatalf("incorrect stream %d: %q", i+1, payload)
		}
		payload = payload[8+size:]
	}
	if len(payload) != 0 {
		t.Fatal("unexpected extra output")
	}
	if doc := inspect(); doc["ExitCode"] != float64(42) || doc["Running"] != false {
		t.Fatalf("failure hidden: %+v", doc)
	}
	if r := call(http.MethodPost, "/exec/"+created.Id+"/start", `{}`); r.Code != http.StatusConflict {
		t.Fatalf("exec ran twice: %d", r.Code)
	}
}

func TestExecUnconfiguredAndUnmodeledRequestsFailClosed(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := e.createContainer("exec-unsupported", "nginx", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Scenario.Exec = []ExecFixture{{Cmd: []string{"true"}, ExitCode: intPtr(0)}}
	c.start()
	for _, body := range []string{
		`{"Cmd":["sh","-c","exit 42"]}`,
		`{"Cmd":["true","extra"]}`,
		`{"Cmd":["true"],"Tty":true}`,
		`{"Cmd":["true"],"Env":["RESULT=fail"]}`,
		`{"Cmd":["true"],"WorkingDir":"/missing"}`,
		`{"Cmd":["true"],"UnknownOption":true}`,
	} {
		r := httptest.NewRecorder()
		(&DockerAPI{engine: e}).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/containers/"+c.ID+"/exec", strings.NewReader(body)))
		if r.Code != http.StatusNotImplemented || r.Header().Get("X-Docker-Zero-Unsupported") != "true" {
			t.Fatalf("%s: %d %s", body, r.Code, r.Body.String())
		}
		if len(e.execs) != 0 {
			t.Fatal("unsupported request allocated an exec")
		}
	}
	for _, body := range []string{`{`, `null`, `{"Cmd":[]}`, `{"Cmd":"true"}`, `{"Cmd":["true"]}{}`} {
		r := httptest.NewRecorder()
		(&DockerAPI{engine: e}).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/containers/"+c.ID+"/exec", strings.NewReader(body)))
		if r.Code != http.StatusBadRequest {
			t.Fatalf("bad request accepted: %s -> %d", body, r.Code)
		}
	}
}

func TestExecFixtureValidation(t *testing.T) {
	for _, fixtures := range [][]ExecFixture{
		{{Cmd: []string{"true"}}},
		{{ExitCode: intPtr(0)}},
		{{Cmd: []string{"true"}, ExitCode: intPtr(-1)}},
		{{Cmd: []string{"true"}, ExitCode: intPtr(256)}},
		{{Cmd: []string{"true"}, ExitCode: intPtr(0)}, {Cmd: []string{"true"}, ExitCode: intPtr(1)}},
	} {
		if validateExecFixtures("/exec", fixtures) == nil {
			t.Fatalf("invalid fixtures accepted: %+v", fixtures)
		}
	}
	cookbooks, err := loadCookbooks("")
	if err != nil {
		t.Fatal(err)
	}
	cb := cookbooks["nginx"]
	scenario := cb.Seeds["0"]
	scenario.Exec = []ExecFixture{{Cmd: []string{"sh", "-c", "exit 42"}, ExitCode: intPtr(42)}}
	cb.Seeds["0"] = scenario
	data, err := json.Marshal(cb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeCookbook("fixture.json", data); err != nil {
		t.Fatal(err)
	}
}

func TestExecDetachedFixture(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := e.createContainer("exec-detached", "nginx", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	c.Scenario.Exec = []ExecFixture{{Cmd: []string{"job"}, Stdout: "hidden", ExitCode: intPtr(7)}}
	exec, err := e.createExec(c, execCreateRequest{Cmd: []string{"job"}, AttachStdout: true})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	(&DockerAPI{engine: e}).ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/exec/"+exec.ID+"/start", strings.NewReader(`{"Detach":true}`)))
	if r.Code != http.StatusOK || r.Body.Len() != 0 || exec.ExitCode != 7 {
		t.Fatalf("detached result: %d %s %d", r.Code, r.Body.String(), exec.ExitCode)
	}
}

func TestExecFixtureSupportsDockerUpgradeHandshake(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := e.createContainer("exec-upgrade", "nginx", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	c.Scenario.Exec = []ExecFixture{{Cmd: []string{"job"}, Stdout: "out", Stderr: "err", ExitCode: intPtr(42)}}
	exec, err := e.createExec(c, execCreateRequest{Cmd: []string{"job"}, AttachStdout: true, AttachStderr: true})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(&DockerAPI{engine: e})
	defer server.Close()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/exec/"+exec.ID+"/start", strings.NewReader(`{"Detach":false,"Tty":false}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	client := server.Client()
	client.Timeout = 3 * time.Second
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer testClose(t, res.Body)
	payload, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusSwitchingProtocols || res.Header.Get("Upgrade") != "tcp" {
		t.Fatalf("SDK handshake: %d", res.StatusCode)
	}
	if len(payload) != 22 || payload[0] != 1 || string(payload[8:11]) != "out" || payload[11] != 2 || string(payload[19:]) != "err" {
		t.Fatalf("upgrade lost framed output: %v", payload)
	}
	if exec.ExitCode != 42 {
		t.Fatal("upgrade lost exit status")
	}
}

func TestExecUpgradeFailureDoesNotMarkFixtureCompleted(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := e.createContainer("exec-no-hijacker", "nginx", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	c.Scenario.Exec = []ExecFixture{{Cmd: []string{"job"}, ExitCode: intPtr(0)}}
	exec, err := e.createExec(c, execCreateRequest{Cmd: []string{"job"}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/exec/"+exec.ID+"/start", strings.NewReader(`{}`))
	req.Header.Set("Upgrade", "tcp")
	r := httptest.NewRecorder()
	(&DockerAPI{engine: e}).ServeHTTP(r, req)
	if r.Code != http.StatusNotImplemented || exec.Started {
		t.Fatalf("unsupported upgrade falsely completed: %d %+v", r.Code, exec)
	}
}
