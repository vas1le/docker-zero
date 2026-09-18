package main

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// ExecFixture describes an exact argv match. It never executes a host process.
type ExecFixture struct {
	Cmd      []string `json:"cmd"`
	Stdout   string   `json:"stdout,omitempty"`
	Stderr   string   `json:"stderr,omitempty"`
	ExitCode int      `json:"exit_code"`
}

type execCreateRequest struct {
	Cmd          []string `json:"Cmd"`
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	Privileged   bool     `json:"Privileged"`
	User         string   `json:"User"`
	Env          []string `json:"Env"`
	WorkingDir   string   `json:"WorkingDir"`
	DetachKeys   string   `json:"DetachKeys"`
}

func validateExecFixtures(path string, fixtures []ExecFixture) error {
	for i, fixture := range fixtures {
		itemPath := fmt.Sprintf("%s/%d", path, i)
		if len(fixture.Cmd) == 0 || fixture.Cmd[0] == "" {
			return validationError(itemPath+"/cmd", "exec fixture requires a non-empty executable")
		}
		for _, arg := range fixture.Cmd {
			if strings.ContainsRune(arg, 0) {
				return validationError(itemPath+"/cmd", "exec arguments cannot contain NUL")
			}
		}
		if fixture.ExitCode < 0 || fixture.ExitCode > 255 {
			return validationError(itemPath+"/exit_code", "exec exit_code must be between 0 and 255")
		}
		for _, previous := range fixtures[:i] {
			if slices.Equal(previous.Cmd, fixture.Cmd) {
				return validationError(itemPath+"/cmd", "duplicate exec command fixture")
			}
		}
	}
	return nil
}

var errExecFixtureMissing = errors.New("no exact exec fixture configured for the requested command; docker-zero does not execute processes")

func (e *Engine) createExec(c *Container, request execCreateRequest) (*ExecInstance, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.execPreconditionLocked(); err != nil {
		return nil, err
	}
	if e.containers[c.ID] != c {
		return nil, fmt.Errorf("container %s was removed", c.Name)
	}
	var fixture *ExecFixture
	for _, candidate := range c.Scenario.Exec {
		if slices.Equal(candidate.Cmd, request.Cmd) {
			fixture = &candidate
			break
		}
	}
	if fixture == nil {
		return nil, errExecFixtureMissing
	}
	id := hashID(fmt.Sprintf("exec:%s:%d", c.ID, e.counter.Add(1)))
	exec := &ExecInstance{
		ID: id, ContainerID: c.ID, Command: slices.Clone(request.Cmd),
		Output: fixture.Stdout, Stderr: fixture.Stderr, FixtureExitCode: fixture.ExitCode,
		AttachStdout: request.AttachStdout, AttachStderr: request.AttachStderr,
	}
	e.execs[id] = exec
	e.ledger.Log(LedgerEntry{Container: c.Name, Kind: c.Kind, Scenario: c.Seed, Channel: "docker", Event: "exec.create", Request: map[string]any{"cmd": request.Cmd}, Response: map[string]any{"id": id}})
	return exec, nil
}

func (api *DockerAPI) handleExecCreate(w http.ResponseWriter, r *http.Request, c *Container) {
	c.mu.Lock()
	stateErr := c.execPreconditionLocked()
	c.mu.Unlock()
	if stateErr != nil {
		writeDockerError(w, http.StatusConflict, stateErr.Error())
		return
	}
	var request execCreateRequest
	if err := decodeJSON(r.Body, &request); err != nil {
		writeDockerError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(request.Cmd) == 0 || request.Cmd[0] == "" {
		writeDockerError(w, http.StatusBadRequest, "no exec command specified")
		return
	}
	if request.Tty || request.AttachStdin || request.Privileged || request.User != "" || len(request.Env) != 0 || request.WorkingDir != "" || request.DetachKeys != "" {
		writeUnsupportedDockerError(w, http.StatusNotImplemented, "exec fixtures do not model TTY, stdin, privilege, user, environment, workdir, or detach keys")
		return
	}
	exec, err := api.engine.createExec(c, request)
	if errors.Is(err, errExecFixtureMissing) {
		writeUnsupportedDockerError(w, http.StatusNotImplemented, err.Error())
		return
	}
	if err != nil {
		writeDockerError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"Id": exec.ID})
}

func (api *DockerAPI) handleExecStart(w http.ResponseWriter, r *http.Request, exec *ExecInstance, c *Container) {
	var request struct {
		Detach bool `json:"Detach"`
		Tty    bool `json:"Tty"`
	}
	if err := decodeJSON(r.Body, &request); err != nil {
		writeDockerError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Tty {
		writeUnsupportedDockerError(w, http.StatusNotImplemented, "exec fixtures do not model TTY sessions")
		return
	}
	if c == nil {
		writeDockerError(w, http.StatusNotFound, "exec container was removed")
		return
	}
	c.mu.Lock()
	if err := c.execPreconditionLocked(); err != nil {
		c.mu.Unlock()
		writeDockerError(w, http.StatusConflict, err.Error())
		return
	}
	exec.mu.Lock()
	if exec.Started {
		exec.mu.Unlock()
		c.mu.Unlock()
		writeDockerError(w, http.StatusConflict, "exec instance has already run")
		return
	}
	exec.Started = true
	exec.ExitCode = intPtr(exec.FixtureExitCode)
	output, stderr := exec.Output, exec.Stderr
	stdoutAttached, stderrAttached := exec.AttachStdout, exec.AttachStderr
	command := slices.Clone(exec.Command)
	exitCode := *exec.ExitCode
	exec.mu.Unlock()
	c.mu.Unlock()
	c.appendLog(output + stderr)
	api.engine.ledger.Log(LedgerEntry{Container: c.Name, Kind: c.Kind, Scenario: c.Seed, Channel: "docker", Event: "exec.start", Request: map[string]any{"id": exec.ID, "cmd": command}, Response: map[string]any{"exit_code": exitCode}})
	if request.Detach {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	w.WriteHeader(http.StatusOK)
	if stdoutAttached {
		writeDockerStreamFrame(w, 1, []byte(output))
	}
	if stderrAttached {
		writeDockerStreamFrame(w, 2, []byte(stderr))
	}
}
