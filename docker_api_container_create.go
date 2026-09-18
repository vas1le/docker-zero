package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type createContainerRequest struct {
	rawConfig     map[string]json.RawMessage
	rawHostConfig map[string]json.RawMessage
	User          string             `json:"User"`
	WorkingDir    string             `json:"WorkingDir"`
	Entrypoint    []string           `json:"Entrypoint"`
	Healthcheck   *HealthcheckConfig `json:"Healthcheck"`
	Tty           bool               `json:"Tty"`

	Hostname     string            `json:"Hostname"`
	Image        string            `json:"Image"`
	Cmd          []string          `json:"Cmd"`
	Env          []string          `json:"Env"`
	Labels       map[string]string `json:"Labels"`
	ExposedPorts map[string]any    `json:"ExposedPorts"`
	HostConfig   struct {
		RestartPolicy RestartPolicy `json:"RestartPolicy"`
		NetworkMode   string        `json:"NetworkMode"`
		PortBindings  map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"PortBindings"`
	} `json:"HostConfig"`
	NetworkingConfig struct {
		EndpointsConfig map[string]struct {
			Aliases []string `json:"Aliases"`
		} `json:"EndpointsConfig"`
	} `json:"NetworkingConfig"`
}

func configureContainerPortBindings(container *Container, input map[string][]struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}) error {
	bindings := make(map[string][]PortBinding, len(input))
	for key, items := range input {
		parts := strings.SplitN(key, "/", 2)
		port, err := strconv.Atoi(parts[0])
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("invalid container port binding %q", key)
		}
		protocol := "tcp"
		if len(parts) == 2 && parts[1] != "" {
			protocol = strings.ToLower(parts[1])
		}
		for _, item := range items {
			hostIP := strings.TrimSpace(item.HostIP)
			if hostIP == "" {
				hostIP = "0.0.0.0"
			}
			requested := 0
			if strings.TrimSpace(item.HostPort) != "" {
				value, err := strconv.Atoi(item.HostPort)
				if err != nil || value < 0 || value > 65535 {
					return fmt.Errorf("invalid host port %q for %s", item.HostPort, key)
				}
				requested = value
			}
			bindings[key] = append(bindings[key], PortBinding{HostIP: hostIP, RequestedHostPort: requested, HostPort: requested, Protocol: protocol, ContainerPort: port})
		}
	}
	container.setPortBindings(bindings)
	return nil
}

func (api *DockerAPI) handleContainerCreate(w http.ResponseWriter, r *http.Request) {
	var request createContainerRequest
	if err := decodeJSON(r.Body, &request); err != nil {
		writeDockerError(w, http.StatusBadRequest, "invalid container config: "+err.Error())
		return
	}
	if err := request.validateMetadata(); err != nil {
		writeDockerError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := r.URL.Query().Get("name")
	container, err := api.engine.createContainer(name, request.Image, request.Labels, request.Env, request.Cmd)
	warnings := []string{}
	if err == nil {
		warnings = container.storeRequestedConfig(&request)
		if bindingErr := configureContainerPortBindings(container, request.HostConfig.PortBindings); bindingErr != nil {
			_ = api.engine.removeContainer(container.ID, true)
			err = bindingErr
		}
	}
	if err == nil {
		aliases := make(map[string][]string, len(request.NetworkingConfig.EndpointsConfig))
		for networkName, endpoint := range request.NetworkingConfig.EndpointsConfig {
			aliases[networkName] = append([]string(nil), endpoint.Aliases...)
		}
		if networkErr := api.engine.configureContainerNetworks(container, request.HostConfig.NetworkMode, aliases); networkErr != nil {
			_ = api.engine.removeContainer(container.ID, true)
			err = networkErr
		}
	}
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(strings.ToLower(err.Error()), "conflict") {
			status = http.StatusConflict
		}
		writeDockerError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"Id": container.ID, "Warnings": warnings})
}

func (api *DockerAPI) handleContainerList(w http.ResponseWriter, r *http.Request) {
	all := parseBoolQuery(r.URL.Query().Get("all"))
	filters, err := parseDockerFilters(r.URL.Query().Get("filters"))
	if err != nil {
		writeDockerError(w, http.StatusBadRequest, "invalid filters: "+err.Error())
		return
	}
	containers := api.engine.listContainers(true)
	response := make([]any, 0, len(containers))
	for _, container := range containers {
		advance := container.advance("docker.list")
		api.engine.syncRuntimeForState(container)
		api.engine.logContainerEvent(container, "container.list", advance, nil, nil)
		if !all && !container.isDockerPSVisible() {
			continue
		}
		if !containerMatchesFilters(container, filters) {
			continue
		}
		response = append(response, containerListDocument(container, api.engine.containerNetworkEndpoints(container)))
	}
	writeJSON(w, http.StatusOK, response)
}
