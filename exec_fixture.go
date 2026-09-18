package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// ExecFixture describes a result, not a command to execute on the host. Matching
// is by the complete argument vector; shell text is never evaluated.
type ExecFixture struct {
	Command  []string `json:"cmd"`
	Stdout   string   `json:"stdout,omitempty"`
	Stderr   string   `json:"stderr,omitempty"`
	ExitCode int      `json:"exit_code"`
}

type ExecInstance struct {
	mu sync.Mutex

	ID           string
	ContainerID  string
	Command      []string
	Started      bool
	Running      bool
	ExitCode     int
	Fixture      ExecFixture
	AttachStdout bool
	AttachStderr bool
}

var errExecFixtureMissing = errors.New("no exact exec fixture matches this command; docker-zero does not execute processes")

func validateExecFixtures(path string, fixtures []ExecFixture) error {
	seen := make(map[string]bool)
	for index, fixture := range fixtures {
		itemPath := fmt.Sprintf("%s/%d", path, index)
		if len(fixture.Command) == 0 || fixture.Command[0] == "" {
			return validationError(itemPath+"/cmd", "exec fixture requires a non-empty command")
		}
		for _, argument := range fixture.Command {
			if strings.ContainsRune(argument, '\x00') {
				return validationError(itemPath+"/cmd", "exec command cannot contain NUL")
			}
		}
		key := strings.Join(fixture.Command, "\x00")
		if seen[key] {
			return validationError(itemPath+"/cmd", "duplicate exec fixture command")
		}
		seen[key] = true
		if fixture.ExitCode < 0 || fixture.ExitCode > 255 {
			return validationError(itemPath+"/exit_code", "exec exit_code must be between 0 and 255")
		}
	}
	return nil
}

func (c *Container) execStateError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.removed || !c.Running || c.Restarting || c.Dead {
		return fmt.Errorf("container %s is not running", c.Name)
	}
	if c.Paused {
		return fmt.Errorf("container %s is paused, unpause the container before exec", c.Name)
	}
	return nil
}

func (e *Engine) createExec(c *Container, command []string) (*ExecInstance, error) {
	if err := c.execStateError(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	var matched *ExecFixture
	for _, fixture := range c.Scenario.Exec {
		if slices.Equal(fixture.Command, command) {
			copy := fixture
			copy.Command = slices.Clone(command)
			matched = &copy
			break
		}
	}
	c.mu.Unlock()
	if matched == nil {
		return nil, errExecFixtureMissing
	}
	id := hashID(fmt.Sprintf("exec:%s:%d", c.ID, e.counter.Add(1)))
	exec := &ExecInstance{ID: id, ContainerID: c.ID, Command: slices.Clone(command), Fixture: *matched}
	e.mu.Lock()
	e.execs[id] = exec
	e.mu.Unlock()
	e.ledger.Log(LedgerEntry{Container: c.Name, Kind: c.Kind, Scenario: c.Seed, Channel: "docker", Event: "exec.create", Request: map[string]any{"cmd": command}, Response: map[string]any{"id": id, "fixture": true}})
	return exec, nil
}

// Exec uses a strict decoder so additional execution options cannot silently
// turn a fixture match into an apparently faithful process execution.
func decodeExecConfig(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 16<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			writeUnsupportedDockerError(w, http.StatusNotImplemented, "unsupported exec option: "+err.Error())
		} else {
			writeDockerError(w, http.StatusBadRequest, "invalid exec config: "+err.Error())
		}
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeDockerError(w, http.StatusBadRequest, "unexpected trailing exec config")
		return false
	}
	return true
}

func (api *DockerAPI) handleExecCreate(w http.ResponseWriter, r *http.Request, c *Container) {
	if err := c.execStateError(); err != nil {
		writeDockerError(w, http.StatusConflict, err.Error())
		return
	}
	var request struct {
		Cmd          []string `json:"Cmd"`
		AttachStdout bool     `json:"AttachStdout"`
		AttachStderr bool     `json:"AttachStderr"`
		AttachStdin  bool     `json:"AttachStdin"`
		Tty          bool     `json:"Tty"`
		Privileged   bool     `json:"Privileged"`
		User         string   `json:"User"`
		WorkingDir   string   `json:"WorkingDir"`
		Env          []string `json:"Env"`
		DetachKeys   string   `json:"DetachKeys"`
	}
	if !decodeExecConfig(w, r, &request) {
		return
	}
	if len(request.Cmd) == 0 || request.Cmd[0] == "" {
		writeDockerError(w, http.StatusBadRequest, "exec command is required")
		return
	}
	if request.AttachStdin || request.Tty || request.Privileged || request.User != "" || request.WorkingDir != "" || len(request.Env) != 0 || request.DetachKeys != "" {
		writeUnsupportedDockerError(w, http.StatusNotImplemented, "exec fixtures support only Cmd, AttachStdout and AttachStderr; process, stdin and terminal options are not simulated")
		return
	}
	exec, err := api.engine.createExec(c, request.Cmd)
	if err != nil {
		if errors.Is(err, errExecFixtureMissing) {
			writeUnsupportedDockerError(w, http.StatusNotImplemented, err.Error())
		} else {
			writeDockerError(w, http.StatusConflict, err.Error())
		}
		return
	}
	exec.mu.Lock()
	exec.AttachStdout, exec.AttachStderr = request.AttachStdout, request.AttachStderr
	exec.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"Id": exec.ID})
}

func (api *DockerAPI) startExec(w http.ResponseWriter, r *http.Request, exec *ExecInstance) {
	var request struct {
		Detach bool `json:"Detach"`
		Tty    bool `json:"Tty"`
	}
	if !decodeExecConfig(w, r, &request) {
		return
	}
	if request.Tty {
		writeUnsupportedDockerError(w, http.StatusNotImplemented, "exec fixture TTY is not simulated")
		return
	}
	container, err := api.engine.findContainer(exec.ContainerID)
	if err != nil {
		writeDockerError(w, http.StatusNotFound, err.Error())
		return
	}
	if err := container.execStateError(); err != nil {
		writeDockerError(w, http.StatusConflict, err.Error())
		return
	}
	exec.mu.Lock()
	if exec.Started {
		exec.mu.Unlock()
		writeDockerError(w, http.StatusConflict, "exec instance has already run")
		return
	}
	exec.Started = true
	exec.Running = false // Fixtures complete synchronously; no process is spawned.
	exec.ExitCode = exec.Fixture.ExitCode
	stdout, stderr := exec.Fixture.Stdout, exec.Fixture.Stderr
	attachStdout, attachStderr := exec.AttachStdout, exec.AttachStderr
	exitCode := exec.ExitCode
	command := slices.Clone(exec.Command)
	exec.mu.Unlock()
	api.engine.ledger.Log(LedgerEntry{Container: container.Name, Kind: container.Kind, Scenario: container.Seed, Channel: "docker", Event: "exec.start", Request: map[string]any{"id": exec.ID, "cmd": command}, Response: map[string]any{"exit_code": exitCode, "fixture": true}})
	if request.Detach {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	w.WriteHeader(http.StatusOK)
	if attachStdout {
		writeDockerStreamFrame(w, 1, []byte(stdout))
	}
	if attachStderr {
		writeDockerStreamFrame(w, 2, []byte(stderr))
	}
}
