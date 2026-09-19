package main

import (
	"fmt"
	"net/http"
	"net/url"
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
	writeUnsupportedDockerError(w, http.StatusNotImplemented, "exec is not simulated: no container processes are executed")
}
