package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type restartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

type healthcheckConfig struct {
	Test          []string `json:"Test"`
	Interval      int64    `json:"Interval"`
	Timeout       int64    `json:"Timeout"`
	StartPeriod   int64    `json:"StartPeriod"`
	StartInterval int64    `json:"StartInterval"`
	Retries       int      `json:"Retries"`
}

// Keep the original JSON as well as the supported typed fields. Unknown settings
// are inspectable metadata, not silently discarded promises of Docker behavior.
func (r *createContainerRequest) UnmarshalJSON(data []byte) error {
	type plain createContainerRequest
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = createContainerRequest(decoded)
	if err := json.Unmarshal(data, &r.rawConfig); err != nil {
		return err
	}
	if host := r.rawConfig["HostConfig"]; host != nil {
		if err := json.Unmarshal(host, &r.rawHostConfig); err != nil {
			return err
		}
	}
	return nil
}

func (r *createContainerRequest) validate() error {
	policy := &r.HostConfig.RestartPolicy
	if policy.Name == "" {
		policy.Name = "no"
	}
	switch policy.Name {
	case "no", "always", "unless-stopped", "on-failure":
	default:
		return fmt.Errorf("unknown restart policy %q", policy.Name)
	}
	if policy.MaximumRetryCount < 0 || (policy.MaximumRetryCount != 0 && policy.Name != "on-failure") {
		return fmt.Errorf("MaximumRetryCount must be nonnegative and only applies to on-failure")
	}
	if h := r.Healthcheck; h != nil {
		if h.Interval < 0 || h.Timeout < 0 || h.StartPeriod < 0 || h.StartInterval < 0 || h.Retries < 0 {
			return fmt.Errorf("negative healthcheck duration or retries")
		}
		if len(h.Test) > 0 {
			switch h.Test[0] {
			case "NONE":
				if len(h.Test) != 1 {
					return fmt.Errorf("NONE healthcheck cannot have arguments")
				}
			case "CMD", "CMD-SHELL":
				if len(h.Test) < 2 {
					return fmt.Errorf("healthcheck command is empty")
				}
			default:
				return fmt.Errorf("invalid healthcheck test %q", h.Test[0])
			}
		}
	}
	for key := range r.ExposedPorts {
		parts := strings.Split(key, "/")
		port, err := strconv.Atoi(parts[0])
		if err != nil || port < 1 || port > 65535 || len(parts) > 2 {
			return fmt.Errorf("invalid exposed port %q", key)
		}
		if len(parts) == 2 && parts[1] != "tcp" && parts[1] != "udp" && parts[1] != "sctp" {
			return fmt.Errorf("invalid exposed port protocol %q", key)
		}
	}
	return nil
}

func meaningfulJSON(raw json.RawMessage) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return true
	}
	return meaningfulValue(value)
}

func meaningfulValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case json.Number:
		n, err := v.Float64()
		return err != nil || n != 0
	case []any:
		for _, item := range v {
			if meaningfulValue(item) {
				return true
			}
		}
		return false
	case map[string]any:
		for _, item := range v {
			if meaningfulValue(item) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func (r *createContainerRequest) metadataOnlyFields() []string {
	fields := []string{}
	for key, raw := range r.rawConfig {
		switch key {
		case "Image", "Labels", "ExposedPorts", "HostConfig", "NetworkingConfig":
			continue
		case "Healthcheck":
			if r.Healthcheck != nil && len(r.Healthcheck.Test) == 1 && r.Healthcheck.Test[0] == "NONE" {
				continue
			}
		}
		if meaningfulJSON(raw) {
			fields = append(fields, "Config."+key)
		}
	}
	for key, raw := range r.rawHostConfig {
		switch key {
		case "NetworkMode", "PortBindings", "RestartPolicy":
			continue
		}
		if meaningfulJSON(raw) {
			fields = append(fields, "HostConfig."+key)
		}
	}
	// Network aliases are modeled; explicit IPAM/link/driver settings are not.
	if raw := r.rawConfig["NetworkingConfig"]; raw != nil {
		var network map[string]json.RawMessage
		if json.Unmarshal(raw, &network) == nil {
			for key, value := range network {
				if key != "EndpointsConfig" {
					if meaningfulJSON(value) {
						fields = append(fields, "NetworkingConfig."+key)
					}
					continue
				}
				var endpoints map[string]map[string]json.RawMessage
				if json.Unmarshal(value, &endpoints) != nil {
					fields = append(fields, "NetworkingConfig.EndpointsConfig")
					continue
				}
				for name, endpoint := range endpoints {
					for option, input := range endpoint {
						if option != "Aliases" && meaningfulJSON(input) {
							fields = append(fields, "NetworkingConfig.EndpointsConfig."+name+"."+option)
						}
					}
				}
			}
		}
	}
	sort.Strings(fields)
	return fields
}

func (c *Container) storeRequestedConfig(r *createContainerRequest, fields []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.RequestedConfig = make(map[string]json.RawMessage, len(r.rawConfig))
	for key, value := range r.rawConfig {
		if key != "HostConfig" && key != "NetworkingConfig" {
			c.RequestedConfig[key] = append(json.RawMessage(nil), value...)
		}
	}
	c.RequestedHostConfig = make(map[string]json.RawMessage, len(r.rawHostConfig))
	for key, value := range r.rawHostConfig {
		c.RequestedHostConfig[key] = append(json.RawMessage(nil), value...)
	}
	c.RequestedNetworking = append(json.RawMessage(nil), r.rawConfig["NetworkingConfig"]...)
	c.MetadataOnlyFields = append([]string{}, fields...)
	c.RestartPolicy = r.HostConfig.RestartPolicy
	c.HealthcheckDisabled = r.Healthcheck != nil && len(r.Healthcheck.Test) == 1 && r.Healthcheck.Test[0] == "NONE"
}

func (e *Engine) capabilities() map[string]any {
	return map[string]any{
		"strict_container_config": e.strictConfig,
		"api_version":             apiVersion, "min_api_version": minAPIVersion,
		"wait_conditions":  []string{"not-running", "next-exit", "removed"},
		"exec":             "exact-argv cookbook fixtures only; no process execution, TTY or stdin",
		"archive":          "unsupported; all operations fail explicitly",
		"health":           "cookbook-driven, not command execution; NONE disables reported health",
		"container_config": "requested settings preserved in inspect; metadata-only fields appear in create warnings and DockerZero.MetadataOnlyFields",
		"isolation":        "no real containers, namespaces, filesystem mounts or resource enforcement",
	}
}
