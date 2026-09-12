package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

func (api *DockerAPI) handleVolumeList(w http.ResponseWriter, r *http.Request) {
	filters, err := parseDockerFilters(r.URL.Query().Get("filters"))
	if err != nil {
		writeDockerError(w, http.StatusBadRequest, "invalid filters: "+err.Error())
		return
	}
	api.engine.mu.RLock()
	items := make([]*Volume, 0, len(api.engine.volumes))
	for _, volume := range api.engine.volumes {
		if volumeMatchesFilters(volume, filters) {
			items = append(items, volume)
		}
	}
	api.engine.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"Volumes": items, "Warnings": []string{}})
}

func (api *DockerAPI) handleVolumeCreate(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name       string            `json:"Name"`
		Driver     string            `json:"Driver"`
		DriverOpts map[string]string `json:"DriverOpts"`
		Labels     map[string]string `json:"Labels"`
	}
	if err := decodeJSON(r.Body, &request); err != nil {
		writeDockerError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, api.engine.createVolume(request.Name, request.Driver, request.Labels, request.DriverOpts))
}

func (api *DockerAPI) handleVolumeAction(w http.ResponseWriter, r *http.Request, ref string) {
	ref, _ = url.PathUnescape(ref)
	api.engine.mu.RLock()
	volume, ok := api.engine.volumes[ref]
	api.engine.mu.RUnlock()
	if !ok {
		writeDockerError(w, http.StatusNotFound, "No such volume: "+ref)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, volume)
	case http.MethodDelete:
		api.engine.mu.Lock()
		delete(api.engine.volumes, ref)
		api.engine.mu.Unlock()
		api.engine.ledger.Log(LedgerEntry{Channel: "docker", Event: "volume.remove", Request: map[string]any{"name": ref}})
		w.WriteHeader(http.StatusNoContent)
	default:
		writeUnsupportedDockerError(w, http.StatusNotFound, fmt.Sprintf("unsupported volume operation: %s", r.Method))
	}
}

func (api *DockerAPI) handleExecAction(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	id := parts[0]
	action := "json"
	if len(parts) > 1 {
		action = parts[1]
	}
	exec, err := api.engine.findExec(id)
	if err != nil {
		writeDockerError(w, http.StatusNotFound, err.Error())
		return
	}
	container, _ := api.engine.findContainer(exec.ContainerID)
	switch {
	case action == "start" && r.Method == http.MethodPost:
		exec.mu.Lock()
		exec.Running = false
		output := exec.Output
		command := append([]string(nil), exec.Command...)
		exitCode := exec.ExitCode
		exec.mu.Unlock()
		if container != nil {
			container.appendLog(output)
			api.engine.ledger.Log(LedgerEntry{Container: container.Name, Kind: container.Kind, Scenario: container.Seed, Channel: "docker", Event: "exec.start", Request: map[string]any{"id": id, "cmd": command}, Response: map[string]any{"exit_code": exitCode}})
		}
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		w.WriteHeader(http.StatusOK)
		writeDockerStreamFrame(w, 1, []byte(output))
	case action == "json" && r.Method == http.MethodGet:
		exec.mu.Lock()
		doc := map[string]any{
			"CanRemove": false, "DetachKeys": "", "ID": exec.ID, "Running": exec.Running,
			"ExitCode": exec.ExitCode, "ProcessConfig": map[string]any{"entrypoint": "", "arguments": append([]string(nil), exec.Command...), "privileged": false, "tty": false, "user": ""},
			"OpenStdin": false, "OpenStderr": true, "OpenStdout": true, "ContainerID": exec.ContainerID, "Pid": 0,
		}
		exec.mu.Unlock()
		writeJSON(w, http.StatusOK, doc)
	default:
		writeUnsupportedDockerError(w, http.StatusNotFound, fmt.Sprintf("unsupported exec operation: %s %s", r.Method, action))
	}
}
