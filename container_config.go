package main

import (
	"encoding/json"
	"fmt"
	"sort"
)

type RestartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

type HealthcheckConfig struct {
	Test        []string `json:"Test"`
	Interval    int64    `json:"Interval,omitempty"`
	Timeout     int64    `json:"Timeout,omitempty"`
	Retries     int      `json:"Retries,omitempty"`
	StartPeriod int64    `json:"StartPeriod,omitempty"`
}

// Retain raw JSON so fields not executed by the simulator can still be checked
// by a controller test. In particular, int64 durations must not pass via float64.
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
	if raw, ok := r.rawConfig["HostConfig"]; ok {
		if err := json.Unmarshal(raw, &r.rawHostConfig); err != nil {
			return err
		}
	}
	delete(r.rawConfig, "HostConfig")
	delete(r.rawConfig, "NetworkingConfig")
	return nil
}

func (r *createContainerRequest) validateMetadata() error {
	policy := &r.HostConfig.RestartPolicy
	if policy.Name == "" {
		policy.Name = "no"
	}
	switch policy.Name {
	case "no", "always", "unless-stopped", "on-failure":
	default:
		return fmt.Errorf("invalid restart policy %q", policy.Name)
	}
	if policy.MaximumRetryCount < 0 || (policy.Name != "on-failure" && policy.MaximumRetryCount != 0) {
		return fmt.Errorf("MaximumRetryCount must be nonnegative and is only valid with on-failure")
	}
	if health := r.Healthcheck; health != nil && len(health.Test) > 0 {
		switch health.Test[0] {
		case "NONE":
			if len(health.Test) != 1 {
				return fmt.Errorf("NONE healthcheck must not contain a command")
			}
		case "CMD", "CMD-SHELL":
			if len(health.Test) < 2 {
				return fmt.Errorf("healthcheck command is required")
			}
		default:
			return fmt.Errorf("invalid healthcheck test type %q", health.Test[0])
		}
	}
	return nil
}

func (c *Container) storeRequestedConfig(r *createContainerRequest) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requestedConfig = cloneRawJSONMap(r.rawConfig)
	c.requestedHostConfig = cloneRawJSONMap(r.rawHostConfig)
	c.RestartPolicy = r.HostConfig.RestartPolicy
	c.healthDisabled = r.Healthcheck != nil && len(r.Healthcheck.Test) > 0 && r.Healthcheck.Test[0] == "NONE"
	// These fields are modeled elsewhere. All others are retained, not enforced.
	for field := range r.rawConfig {
		switch field {
		case "Image", "Labels", "ExposedPorts":
			continue
		}
		c.metadataOnlyFields = append(c.metadataOnlyFields, "Config."+field)
	}
	for field := range r.rawHostConfig {
		switch field {
		case "NetworkMode", "PortBindings", "RestartPolicy":
			continue
		}
		c.metadataOnlyFields = append(c.metadataOnlyFields, "HostConfig."+field)
	}
	sort.Strings(c.metadataOnlyFields)
	warnings := make([]string, 0, len(c.metadataOnlyFields))
	for _, field := range c.metadataOnlyFields {
		warnings = append(warnings, "docker-zero: "+field+" is stored as metadata; it is not executed or enforced")
	}
	return warnings
}

func cloneRawJSONMap(input map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(input))
	for key, value := range input {
		out[key] = append(json.RawMessage(nil), value...)
	}
	return out
}
