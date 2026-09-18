package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

type RestartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

type containerHealthcheck struct {
	Test        []string `json:"Test"`
	Interval    int64    `json:"Interval"`
	Timeout     int64    `json:"Timeout"`
	StartPeriod int64    `json:"StartPeriod"`
	Retries     int      `json:"Retries"`
}

// Immutable after container publication. Raw JSON preserves empty arrays and
// numeric precision, including settings whose behavior the simulator cannot run.
type containerConfigRecord struct {
	Config              map[string]json.RawMessage
	HostConfig          map[string]json.RawMessage
	MetadataOnly        []string
	Entrypoint          []string
	RestartPolicy       RestartPolicy
	HealthcheckDisabled bool
}

func readContainerConfig(body io.Reader) (createContainerRequest, *containerConfigRecord, error) {
	var request createContainerRequest
	var raw map[string]json.RawMessage
	if err := decodeJSON(body, &raw); err != nil {
		return request, nil, err
	}
	if raw == nil {
		return request, nil, fmt.Errorf("config must be a JSON object")
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return request, nil, err
	}
	if err := json.Unmarshal(data, &request); err != nil {
		return request, nil, err
	}
	policy := request.HostConfig.RestartPolicy
	if policy.Name == "" {
		policy.Name = "no"
	}
	switch policy.Name {
	case "no", "always", "unless-stopped", "on-failure":
	default:
		return request, nil, fmt.Errorf("invalid restart policy %q", policy.Name)
	}
	if policy.MaximumRetryCount < 0 || (policy.Name != "on-failure" && policy.MaximumRetryCount != 0) {
		return request, nil, fmt.Errorf("MaximumRetryCount must be nonnegative and may only be set with on-failure")
	}
	if request.WorkingDir != "" && !strings.HasPrefix(request.WorkingDir, "/") {
		return request, nil, fmt.Errorf("WorkingDir must be absolute")
	}
	if err := validateContainerHealthcheck(request.Healthcheck); err != nil {
		return request, nil, err
	}
	record := &containerConfigRecord{Config: raw, HostConfig: map[string]json.RawMessage{}, RestartPolicy: policy, Entrypoint: append([]string(nil), request.Entrypoint...), MetadataOnly: []string{}}
	if value, ok := raw["HostConfig"]; ok {
		if err := json.Unmarshal(value, &record.HostConfig); err != nil {
			return request, nil, err
		}
	}
	record.HealthcheckDisabled = request.Healthcheck != nil && len(request.Healthcheck.Test) == 1 && request.Healthcheck.Test[0] == "NONE"
	for key, value := range raw {
		switch key {
		case "Image", "Labels", "ExposedPorts", "AttachStdout", "AttachStderr":
		case "HostConfig":
			for field, setting := range record.HostConfig {
				switch field {
				case "NetworkMode", "PortBindings":
				case "RestartPolicy":
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(setting, &fields); err != nil {
						return request, nil, err
					}
					for name, item := range fields {
						if name != "Name" && name != "MaximumRetryCount" && meaningfulJSON(item) {
							record.MetadataOnly = append(record.MetadataOnly, "HostConfig.RestartPolicy."+name)
						}
					}
				default:
					if meaningfulJSON(setting) && !neutralHostConfig(field, setting) {
						record.MetadataOnly = append(record.MetadataOnly, "HostConfig."+field)
					}
				}
			}
		case "NetworkingConfig":
			if err := record.classifyNetworking(value); err != nil {
				return request, nil, err
			}
		case "Healthcheck":
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(value, &fields); err != nil {
				return request, nil, err
			}
			for field, item := range fields {
				switch field {
				case "Test", "Interval", "Timeout", "StartPeriod", "Retries":
				default:
					if meaningfulJSON(item) {
						record.MetadataOnly = append(record.MetadataOnly, "Healthcheck."+field)
					}
				}
			}
			if !record.HealthcheckDisabled && meaningfulJSON(value) {
				record.MetadataOnly = append(record.MetadataOnly, key)
			}
		default:
			if meaningfulJSON(value) {
				record.MetadataOnly = append(record.MetadataOnly, key)
			}
		}
	}
	sort.Strings(record.MetadataOnly)
	return request, record, nil
}

func validateContainerHealthcheck(health *containerHealthcheck) error {
	if health == nil {
		return nil
	}
	if health.Interval < 0 || health.Timeout < 0 || health.StartPeriod < 0 || health.Retries < 0 {
		return fmt.Errorf("healthcheck timings and retries must be nonnegative")
	}
	if len(health.Test) == 0 {
		return nil
	}
	switch health.Test[0] {
	case "NONE":
		if len(health.Test) == 1 {
			return nil
		}
	case "CMD":
		if len(health.Test) >= 2 {
			return nil
		}
	case "CMD-SHELL":
		if len(health.Test) == 2 {
			return nil
		}
	}
	return fmt.Errorf("invalid Healthcheck.Test; use NONE, CMD plus argv, or CMD-SHELL plus a command")
}

func meaningfulJSON(raw json.RawMessage) bool {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return true
	}
	meaningful := func(v any) bool {
		switch item := v.(type) {
		case nil:
			return false
		case bool:
			return item
		case float64:
			return item != 0
		case string:
			return item != ""
		case []any:
			return len(item) > 0
		case map[string]any:
			return len(item) > 0
		default:
			return true
		}
	}
	return meaningful(value)
}

func (c *containerConfigRecord) classifyNetworking(raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for field, value := range fields {
		if field != "EndpointsConfig" {
			if meaningfulJSON(value) {
				c.MetadataOnly = append(c.MetadataOnly, "NetworkingConfig."+field)
			}
			continue
		}
		var endpoints map[string]map[string]json.RawMessage
		if err := json.Unmarshal(value, &endpoints); err != nil {
			return err
		}
		for network, settings := range endpoints {
			for key, item := range settings {
				if key != "Aliases" && meaningfulJSON(item) {
					c.MetadataOnly = append(c.MetadataOnly, "NetworkingConfig.EndpointsConfig."+network+"."+key)
				}
			}
		}
	}
	return nil
}

func (c *containerConfigRecord) warnings() []string {
	warnings := make([]string, 0, len(c.MetadataOnly))
	for _, field := range c.MetadataOnly {
		warnings = append(warnings, field+": metadata stored, behavior not simulated; health and process behavior come from the scenario")
	}
	return warnings
}

func (c *containerConfigRecord) overlayInspect(doc map[string]any) {
	config := doc["Config"].(map[string]any)
	host := doc["HostConfig"].(map[string]any)
	for key, value := range c.Config {
		switch key {
		case "HostConfig", "NetworkingConfig":
			continue
		case "ExposedPorts":
			var ports map[string]json.RawMessage
			if json.Unmarshal(value, &ports) == nil {
				for port, setting := range ports {
					config["ExposedPorts"].(map[string]any)[port] = json.RawMessage(bytes.Clone(setting))
					networkPorts := doc["NetworkSettings"].(map[string]any)["Ports"].(map[string]any)
					if _, ok := networkPorts[port]; !ok {
						networkPorts[port] = nil
					}
				}
			}
		default:
			config[key] = json.RawMessage(bytes.Clone(value))
		}
	}
	for key, value := range c.HostConfig {
		if key != "NetworkMode" && key != "PortBindings" && key != "RestartPolicy" {
			host[key] = json.RawMessage(bytes.Clone(value))
		}
	}
	// The normalized restart policy and allocated ports remain runtime facts.
	host["RestartPolicy"] = c.RestartPolicy
	if len(c.Entrypoint) > 0 {
		var cmd []string
		if raw, ok := c.Config["Cmd"]; ok {
			_ = json.Unmarshal(raw, &cmd)
		}
		argv := append(append([]string(nil), c.Entrypoint...), cmd...)
		doc["Path"], doc["Args"] = firstOrEmpty(argv), restOrEmpty(argv)
	}
	capabilities := map[string]any{"metadata_only": append([]string{}, c.MetadataOnly...), "health_source": "scenario_fixture", "process_execution": false}
	if c.HealthcheckDisabled {
		capabilities["health_source"] = "disabled"
	}
	doc["DockerZero"].(map[string]any)["Configuration"] = capabilities
	if raw, ok := c.Config["NetworkingConfig"]; ok {
		doc["DockerZero"].(map[string]any)["RequestedNetworkingConfig"] = json.RawMessage(bytes.Clone(raw))
	}
}

// Compose serializes these default aggregates even without a logging or TTY
// override. Non-default values still require explicit unsupported reporting.
func neutralHostConfig(field string, raw json.RawMessage) bool {
	switch field {
	case "ConsoleSize":
		var dimensions []int
		return json.Unmarshal(raw, &dimensions) == nil && len(dimensions) == 2 && dimensions[0] == 0 && dimensions[1] == 0
	case "LogConfig":
		var settings map[string]json.RawMessage
		if json.Unmarshal(raw, &settings) != nil {
			return false
		}
		for key, value := range settings {
			switch key {
			case "Type":
				var name string
				if json.Unmarshal(value, &name) != nil || name != "" {
					return false
				}
			case "Config":
				var options map[string]any
				if json.Unmarshal(value, &options) != nil || len(options) != 0 {
					return false
				}
			default:
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (e *Engine) classifyModeledCommand(request createContainerRequest, config *containerConfigRecord) {
	cmd := request.Cmd
	if len(cmd) != 4 || cmd[0] != "redis-server" || (cmd[1] != "--replicaof" && cmd[1] != "--slaveof") {
		return
	}
	cb, err := e.cookbookFor("", request.Image)
	if err != nil || cb.Kind != "redis" {
		return
	}
	if _, _, ok := redisReplicaTargetFromCommand(cmd); !ok {
		return
	}
	fields := config.MetadataOnly[:0]
	for _, field := range config.MetadataOnly {
		if field != "Cmd" {
			fields = append(fields, field)
		}
	}
	config.MetadataOnly = fields
}
