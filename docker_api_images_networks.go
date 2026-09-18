package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strings"
)

func (api *DockerAPI) handleImageList(w http.ResponseWriter) {
	images := make([]any, 0, len(api.engine.cookbooks))
	for _, kind := range sortedCookbookKinds(api.engine.cookbooks) {
		cb := api.engine.cookbooks[kind]
		images = append(images, map[string]any{
			"Containers":  -1,
			"Created":     api.engine.startedAt.Unix(),
			"Id":          imageID(cb.Defaults.Image),
			"Labels":      map[string]string{"docker-zero.kind": cb.Kind},
			"ParentId":    "",
			"RepoDigests": []string{},
			"RepoTags":    []string{cb.Defaults.Image},
			"SharedSize":  -1,
			"Size":        0,
			"VirtualSize": 0,
		})
	}
	writeJSON(w, http.StatusOK, images)
}

func (api *DockerAPI) handleImageCreate(w http.ResponseWriter, r *http.Request) {
	image := r.URL.Query().Get("fromImage")
	if tag := r.URL.Query().Get("tag"); tag != "" && !strings.Contains(image, ":") {
		image += ":" + tag
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	_ = encoder.Encode(map[string]any{"status": "Pulling from docker-zero", "id": image})
	_ = encoder.Encode(map[string]any{"status": "Digest: " + imageID(image)})
	_ = encoder.Encode(map[string]any{"status": "Status: Image is simulated", "id": image})
	api.engine.ledger.Log(LedgerEntry{Channel: "docker", Event: "image.pull", Request: map[string]any{"image": image}})
}

func (api *DockerAPI) handleImageInspect(w http.ResponseWriter, name string) {
	cb, err := api.engine.cookbookFor("", name)
	if err != nil {
		writeDockerError(w, http.StatusNotFound, "No such image: "+name)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Id":              imageID(cb.Defaults.Image),
		"RepoTags":        []string{cb.Defaults.Image},
		"RepoDigests":     []string{},
		"Parent":          "",
		"Comment":         "docker-zero simulated image",
		"Created":         formatDockerTime(api.engine.startedAt),
		"ContainerConfig": map[string]any{},
		"DockerVersion":   engineVersion,
		"Author":          "docker-zero",
		"Config": map[string]any{
			"Hostname": "", "Domainname": "", "User": "", "AttachStdin": false, "AttachStdout": false,
			"AttachStderr": false, "Tty": false, "OpenStdin": false, "StdinOnce": false,
			"Env": []string{}, "Cmd": []string{}, "Image": "", "Volumes": nil, "WorkingDir": "", "Entrypoint": nil,
			"OnBuild": nil, "Labels": map[string]string{"docker-zero.kind": cb.Kind},
		},
		"Architecture": runtime.GOARCH, "Os": "linux", "Size": 0, "VirtualSize": 0,
		"GraphDriver": map[string]any{"Data": map[string]string{}, "Name": "docker-zero"},
		"RootFS":      map[string]any{"Type": "layers", "Layers": []string{}},
		"Metadata":    map[string]any{"LastTagTime": formatDockerTime(api.engine.startedAt)},
	})
}

func (api *DockerAPI) handleNetworkList(w http.ResponseWriter, r *http.Request) {
	filters, err := parseDockerFilters(r.URL.Query().Get("filters"))
	if err != nil {
		writeDockerError(w, http.StatusBadRequest, "invalid filters: "+err.Error())
		return
	}
	api.engine.mu.RLock()
	items := make([]*Network, 0, len(api.engine.networks))
	for _, network := range api.engine.networks {
		if networkMatchesFilters(network, filters) {
			items = append(items, cloneNetwork(network))
		}
	}
	api.engine.mu.RUnlock()
	writeJSON(w, http.StatusOK, items)
}

func (api *DockerAPI) handleNetworkCreate(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name   string            `json:"Name"`
		Driver string            `json:"Driver"`
		Labels map[string]string `json:"Labels"`
	}
	if err := decodeJSON(r.Body, &request); err != nil {
		writeDockerError(w, http.StatusBadRequest, err.Error())
		return
	}
	network, err := api.engine.createNetwork(request.Name, request.Driver, request.Labels)
	if err != nil {
		writeDockerError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"Id": network.ID, "Warning": ""})
}

func (api *DockerAPI) handleNetworkAction(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	ref, _ := url.PathUnescape(parts[0])
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	network, err := api.engine.findNetwork(ref)
	if err != nil {
		writeDockerError(w, http.StatusNotFound, err.Error())
		return
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		api.engine.mu.RLock()
		snapshot := cloneNetwork(network)
		api.engine.mu.RUnlock()
		writeJSON(w, http.StatusOK, snapshot)
	case action == "" && r.Method == http.MethodDelete:
		if err := api.engine.removeNetwork(ref); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, errPredefinedNetwork) {
				status = http.StatusForbidden
			}
			writeDockerError(w, status, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case (action == "connect" || action == "disconnect") && r.Method == http.MethodPost:
		var request struct {
			Container      string `json:"Container"`
			EndpointConfig struct {
				Aliases []string `json:"Aliases"`
			} `json:"EndpointConfig"`
		}
		if err := decodeJSON(r.Body, &request); err != nil {
			writeDockerError(w, http.StatusBadRequest, err.Error())
			return
		}
		container, err := api.engine.findContainer(request.Container)
		if err != nil {
			writeDockerError(w, http.StatusNotFound, err.Error())
			return
		}
		connected, connectedNetwork := api.engine.containerConnectedToNetwork(container, network.ID)
		if action == "connect" && connected {
			writeDockerError(w, http.StatusBadRequest, fmt.Sprintf("endpoint with name %s already exists in network %s", container.Name, connectedNetwork.Name))
			return
		}
		wasRunning := container.isRunning()
		if wasRunning {
			_ = api.engine.stopContainerRuntime(container)
		}
		if action == "connect" {
			err = api.engine.connectContainerNetwork(container, network.ID, request.EndpointConfig.Aliases)
		} else {
			err = api.engine.disconnectContainerNetwork(container, network.ID)
		}
		if err != nil {
			if wasRunning {
				_ = api.engine.startContainerRuntime(container)
			}
			writeDockerError(w, http.StatusBadRequest, err.Error())
			return
		}
		if wasRunning {
			if runtimeErr := api.engine.startContainerRuntime(container); runtimeErr != nil {
				writeDockerError(w, http.StatusInternalServerError, runtimeErr.Error())
				return
			}
		}
		api.engine.ledger.Log(LedgerEntry{Container: container.Name, Kind: container.Kind, Scenario: container.Seed, Channel: "docker", Event: "network." + action, Request: map[string]any{"network": network.Name, "aliases": request.EndpointConfig.Aliases}})
		w.WriteHeader(http.StatusOK)
	default:
		writeUnsupportedDockerError(w, http.StatusNotFound, fmt.Sprintf("unsupported network operation: %s", r.Method))
	}
}
