package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
)

type execCreateRequest struct {
	Detach       bool     `json:"Detach"`
	Cmd          []string `json:"Cmd"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	AttachStdin  bool     `json:"AttachStdin"`
	Tty          bool     `json:"Tty"`
	Privileged   bool     `json:"Privileged"`
	User         string   `json:"User"`
	Env          []string `json:"Env"`
	WorkingDir   string   `json:"WorkingDir"`
	DetachKeys   string   `json:"DetachKeys"`
}

func validateExecFixtures(path string, fixtures []ExecFixture) error {
	seen := make(map[string]bool)
	for i, fixture := range fixtures {
		p := fmt.Sprintf("%s/%d", path, i)
		if len(fixture.Cmd) == 0 || fixture.Cmd[0] == "" {
			return validationError(p+"/cmd", "exec fixture needs a nonempty command")
		}
		if fixture.ExitCode == nil || *fixture.ExitCode < 0 || *fixture.ExitCode > 255 {
			return validationError(p+"/exit_code", "exec fixture exit_code is required and must be between 0 and 255")
		}
		key, _ := json.Marshal(fixture.Cmd)
		if seen[string(key)] {
			return validationError(p+"/cmd", "duplicate exec command fixture")
		}
		seen[string(key)] = true
	}
	return nil
}

func decodeExecJSON(body io.Reader, target any) error {
	decoder := json.NewDecoder(io.LimitReader(body, 16<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func writeExecDecodeError(w http.ResponseWriter, err error) {
	if strings.HasPrefix(err.Error(), "json: unknown field ") {
		writeUnsupportedDockerError(w, http.StatusNotImplemented, err.Error())
		return
	}
	writeDockerError(w, http.StatusBadRequest, "invalid exec config: "+err.Error())
}

func (api *DockerAPI) handleExecCreate(w http.ResponseWriter, r *http.Request, c *Container) {
	var request execCreateRequest
	if err := decodeExecJSON(r.Body, &request); err != nil {
		writeExecDecodeError(w, err)
		return
	}
	if len(request.Cmd) == 0 || request.Cmd[0] == "" {
		writeDockerError(w, http.StatusBadRequest, "exec Cmd must not be empty")
		return
	}
	if request.AttachStdin || request.Tty || request.Privileged || request.User != "" || len(request.Env) > 0 || request.WorkingDir != "" || request.DetachKeys != "" {
		writeUnsupportedDockerError(w, http.StatusNotImplemented, "exec fixtures support argv and stdout/stderr attachment only; interactive, environment and execution-context overrides are not simulated")
		return
	}
	exec, err := api.engine.createExec(c, request)
	if err != nil {
		writeUnsupportedDockerError(w, http.StatusNotImplemented, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"Id": exec.ID})
}

func (e *Engine) createExec(c *Container, request execCreateRequest) (*ExecInstance, error) {
	c.mu.Lock()
	var match *ExecFixture
	for _, fixture := range c.Scenario.Exec {
		if slices.Equal(fixture.Cmd, request.Cmd) {
			copy := fixture
			copy.Cmd = slices.Clone(fixture.Cmd)
			if fixture.ExitCode != nil {
				copy.ExitCode = intPtr(*fixture.ExitCode)
			}
			match = &copy
			break
		}
	}
	c.mu.Unlock()
	if match == nil || match.ExitCode == nil {
		return nil, fmt.Errorf("no explicit exec fixture matches argv %q; configure the scenario's exec fixtures", request.Cmd)
	}
	request.Cmd = slices.Clone(request.Cmd)
	id := hashID(fmt.Sprintf("exec:%s:%d", c.ID, e.counter.Add(1)))
	exec := &ExecInstance{ID: id, ContainerID: c.ID, Command: slices.Clone(request.Cmd), Request: request, Fixture: *match, ExitCode: -1}
	e.mu.Lock()
	e.execs[id] = exec
	e.mu.Unlock()
	e.ledger.Log(LedgerEntry{Container: c.Name, Kind: c.Kind, Scenario: c.Seed, Channel: "docker", Event: "exec.create", Request: map[string]any{"cmd": request.Cmd}, Response: map[string]any{"id": id, "simulation": "explicit_fixture"}})
	return exec, nil
}

func (api *DockerAPI) startExecFixture(w http.ResponseWriter, r *http.Request, exec *ExecInstance, c *Container) {
	var request struct {
		Detach bool `json:"Detach"`
		Tty    bool `json:"Tty"`
	}
	if err := decodeExecJSON(r.Body, &request); err != nil {
		writeExecDecodeError(w, err)
		return
	}
	if request.Tty {
		writeUnsupportedDockerError(w, http.StatusNotImplemented, "TTY exec streams are not simulated")
		return
	}
	exec.mu.Lock()
	if exec.Started {
		exec.mu.Unlock()
		writeDockerError(w, http.StatusConflict, "exec instance has already been started")
		return
	}
	if exec.Fixture.ExitCode == nil {
		exec.mu.Unlock()
		writeUnsupportedDockerError(w, http.StatusNotImplemented, "exec instance has no configured result")
		return
	}
	var connection net.Conn
	var upgraded *bufio.ReadWriter
	if !request.Detach && strings.EqualFold(r.Header.Get("Upgrade"), "tcp") {
		var err error
		connection, upgraded, err = http.NewResponseController(w).Hijack()
		if err != nil {
			exec.mu.Unlock()
			writeUnsupportedDockerError(w, http.StatusNotImplemented, "exec stream upgrade unavailable: "+err.Error())
			return
		}
		defer func() { _ = connection.Close() }()
	}
	exec.Started, exec.Running = true, false
	exec.ExitCode = *exec.Fixture.ExitCode
	stdout, stderr, exitCode := exec.Fixture.Stdout, exec.Fixture.Stderr, exec.ExitCode
	attachStdout, attachStderr := exec.Request.AttachStdout, exec.Request.AttachStderr
	command := slices.Clone(exec.Command)
	exec.mu.Unlock()
	api.engine.ledger.Log(LedgerEntry{Container: c.Name, Kind: c.Kind, Scenario: c.Seed, Channel: "docker", Event: "exec.start", Request: map[string]any{"id": exec.ID, "cmd": command}, Response: map[string]any{"exit_code": exitCode, "simulation": "explicit_fixture"}})
	var output io.Writer = w
	if upgraded != nil {
		if recorder, ok := w.(*dockerResponseRecorder); ok {
			recorder.status = http.StatusSwitchingProtocols
			output = &execLedgerWriter{writer: upgraded.Writer, recorder: recorder}
		} else {
			output = upgraded.Writer
		}
		if _, err := upgraded.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"); err != nil {
			return
		}
	} else {
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		w.WriteHeader(http.StatusOK)
	}
	if !request.Detach {
		if attachStdout {
			writeDockerStreamFrame(output, 1, []byte(stdout))
		}
		if attachStderr {
			writeDockerStreamFrame(output, 2, []byte(stderr))
		}
	}
	if upgraded != nil {
		_ = upgraded.Flush()
	}
}

// Preserve HTTP-ledger byte accounting when Docker's SDK upgrades to a raw
// connection rather than consuming an ordinary HTTP response body.
type execLedgerWriter struct {
	writer   io.Writer
	recorder *dockerResponseRecorder
}

func (w *execLedgerWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.recorder.bytes += n
	return n, err
}
