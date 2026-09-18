package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func (api *DockerAPI) handleContainerAction(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeDockerError(w, http.StatusNotFound, "container reference is required")
		return
	}
	ref, _ := url.PathUnescape(parts[0])
	action := "json"
	if len(parts) > 1 {
		action = parts[1]
	}
	container, err := api.engine.findContainer(ref)
	if err != nil {
		writeDockerError(w, http.StatusNotFound, err.Error())
		return
	}

	switch {
	case action == "json" && r.Method == http.MethodGet:
		advance := container.advance("docker.inspect")
		api.engine.syncRuntimeForState(container)
		doc := containerInspectDocument(container, api.engine.containerNetworkEndpoints(container))
		api.engine.logContainerEvent(container, "container.inspect", advance, nil, map[string]any{"status": doc["State"].(map[string]any)["Status"]})
		writeJSON(w, http.StatusOK, doc)
	case action == "start" && r.Method == http.MethodPost:
		before := container.stateSnapshot()
		statusCode := http.StatusNoContent
		if before.Running && !before.Restarting {
			statusCode = http.StatusNotModified
		} else {
			container.start()
			if runtimeErr := api.engine.startContainerRuntime(container); runtimeErr != nil {
				_, after := container.applyPatch(StatePatch{Status: stringPtr("exited"), Running: boolPtr(false), Restarting: boolPtr(false), Health: stringPtr("unhealthy"), ExitCode: intPtr(128), Error: stringPtr(runtimeErr.Error())})
				api.engine.ledger.Log(LedgerEntry{Container: container.Name, Kind: container.Kind, Scenario: container.Seed, Channel: "docker", Event: "container.start.failed", Response: map[string]any{"error": runtimeErr.Error()}, Before: &before, After: &after})
				writeDockerError(w, http.StatusInternalServerError, runtimeErr.Error())
				return
			}
		}
		advance := container.advance("docker.start")
		advance.Before = before
		api.engine.logContainerEvent(container, "container.start", advance, nil, map[string]any{"status": statusCode})
		w.WriteHeader(statusCode)
	case action == "stop" && r.Method == http.MethodPost:
		before := container.stateSnapshot()
		statusCode := http.StatusNoContent
		if !before.Running && !before.Restarting {
			statusCode = http.StatusNotModified
		} else {
			_ = api.engine.stopContainerRuntime(container)
			container.stop(0)
		}
		advance := container.advance("docker.stop")
		advance.Before = before
		api.engine.logContainerEvent(container, "container.stop", advance, nil, map[string]any{"status": statusCode})
		w.WriteHeader(statusCode)
	case action == "pause" && r.Method == http.MethodPost:
		before, after, pauseErr := container.pause()
		if pauseErr != nil {
			writeDockerError(w, http.StatusInternalServerError, pauseErr.Error())
			return
		}
		api.engine.ledger.Log(LedgerEntry{Container: container.Name, Kind: container.Kind, Scenario: container.Seed, Channel: "docker", Event: "container.pause", Before: &before, After: &after})
		w.WriteHeader(http.StatusNoContent)
	case action == "unpause" && r.Method == http.MethodPost:
		before, after, unpauseErr := container.unpause()
		if unpauseErr != nil {
			writeDockerError(w, http.StatusInternalServerError, unpauseErr.Error())
			return
		}
		api.engine.ledger.Log(LedgerEntry{Container: container.Name, Kind: container.Kind, Scenario: container.Seed, Channel: "docker", Event: "container.unpause", Before: &before, After: &after})
		w.WriteHeader(http.StatusNoContent)
	case action == "restart" && r.Method == http.MethodPost:
		before := container.stateSnapshot()
		_ = api.engine.stopContainerRuntime(container)
		container.restart()
		advance := container.advance("docker.restart")
		state := container.stateSnapshot()
		if state.Restarting || state.Status == "restarting" {
			container.start()
		}
		if runtimeErr := api.engine.startContainerRuntime(container); runtimeErr != nil {
			_, after := container.applyPatch(StatePatch{Status: stringPtr("exited"), Running: boolPtr(false), Restarting: boolPtr(false), Health: stringPtr("unhealthy"), ExitCode: intPtr(128), Error: stringPtr(runtimeErr.Error())})
			api.engine.ledger.Log(LedgerEntry{Container: container.Name, Kind: container.Kind, Scenario: container.Seed, Channel: "docker", Event: "container.restart.failed", Response: map[string]any{"error": runtimeErr.Error()}, Before: &before, After: &after})
			writeDockerError(w, http.StatusInternalServerError, runtimeErr.Error())
			return
		}
		advance.Before = before
		advance.After = container.stateSnapshot()
		api.engine.logContainerEvent(container, "container.restart", advance, nil, nil)
		w.WriteHeader(http.StatusNoContent)
	case action == "kill" && r.Method == http.MethodPost:
		before := container.stateSnapshot()
		_ = api.engine.stopContainerRuntime(container)
		container.stop(137)
		advance := container.advance("docker.kill")
		advance.Before = before
		api.engine.logContainerEvent(container, "container.kill", advance, nil, nil)
		w.WriteHeader(http.StatusNoContent)
	case action == "wait" && r.Method == http.MethodPost:
		advance := container.advance("docker.wait")
		state := container.stateSnapshot()
		api.engine.logContainerEvent(container, "container.wait", advance, nil, map[string]any{"StatusCode": state.ExitCode})
		writeJSON(w, http.StatusOK, map[string]any{"StatusCode": state.ExitCode, "Error": nil})
	case action == "logs" && r.Method == http.MethodGet:
		advance := container.advance("docker.logs")
		logs := container.allLogs()
		api.engine.logContainerEvent(container, "container.logs", advance, nil, map[string]any{"bytes": len(logs)})
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		w.WriteHeader(http.StatusOK)
		writeDockerStreamFrame(w, 1, []byte(logs))
	case action == "top" && r.Method == http.MethodGet:
		advance := container.advance("docker.top")
		api.engine.logContainerEvent(container, "container.top", advance, nil, nil)
		writeJSON(w, http.StatusOK, map[string]any{"Titles": []string{"UID", "PID", "PPID", "C", "STIME", "TTY", "TIME", "CMD"}, "Processes": [][]string{}})
	case action == "exec" && r.Method == http.MethodPost:
		container.mu.Lock()
		paused := container.Paused
		container.mu.Unlock()
		if paused {
			writeDockerError(w, http.StatusConflict, "container is paused, unpause the container before exec")
			return
		}
		var request struct {
			Cmd []string `json:"Cmd"`
		}
		if err := decodeJSON(r.Body, &request); err != nil {
			writeDockerError(w, http.StatusBadRequest, err.Error())
			return
		}
		exec := api.engine.createExec(container, request.Cmd)
		writeJSON(w, http.StatusCreated, map[string]any{"Id": exec.ID})
	case action == "archive" && (r.Method == http.MethodPut || r.Method == http.MethodHead):
		advance := container.advance("docker.archive")
		_, _ = io.Copy(io.Discard, r.Body)
		api.engine.logContainerEvent(container, "container.archive", advance, map[string]any{"path": r.URL.Query().Get("path")}, nil)
		w.WriteHeader(http.StatusOK)
	case action == "json" && r.Method == http.MethodDelete:
		fallthrough
	case len(parts) == 1 && r.Method == http.MethodDelete:
		force := parseBoolQuery(r.URL.Query().Get("force"))
		if err := api.engine.removeContainer(ref, force); err != nil {
			writeDockerError(w, http.StatusConflict, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeUnsupportedDockerError(w, http.StatusNotFound, fmt.Sprintf("unsupported container operation: %s %s", r.Method, action))
	}
}
