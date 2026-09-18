package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

type RestartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

type HealthcheckConfig struct {
	Test          []string `json:"Test"`
	Interval      int64    `json:"Interval,omitempty"`
	Timeout       int64    `json:"Timeout,omitempty"`
	StartPeriod   int64    `json:"StartPeriod,omitempty"`
	StartInterval int64    `json:"StartInterval,omitempty"`
	Retries       int      `json:"Retries,omitempty"`
}

type containerCreateMetadata struct {
	config         map[string]json.RawMessage
	hostConfig     map[string]json.RawMessage
	metadataOnly   []string
	healthOverride bool
}

// Keep raw values as well as typed operational fields. Unknown meaningful
// configuration is retained for inspection, but explicitly classified as
// unmodeled, rather than silently discarded. RawMessage preserves large numbers
// and the distinction between null, an empty slice, and an omitted field.
func (request *createContainerRequest) UnmarshalJSON(data []byte) error {
	type plain createContainerRequest
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*request = createContainerRequest(decoded)
	if err := json.Unmarshal(data, &request.rawConfig); err != nil {
		return err
	}
	if request.rawConfig == nil {
		return fmt.Errorf("container config must be an object")
	}
	if raw := request.rawConfig["HostConfig"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &request.rawHostConfig); err != nil {
			return err
		}
	}
	return nil
}

func (request *createContainerRequest) validateMetadata() error {
	policy := &request.HostConfig.RestartPolicy
	if policy.Name == "" {
		policy.Name = "no"
	}
	switch policy.Name {
	case "no", "always", "unless-stopped", "on-failure":
	default:
		return fmt.Errorf("invalid restart policy %q", policy.Name)
	}
	if policy.MaximumRetryCount < 0 || (policy.Name != "on-failure" && policy.MaximumRetryCount != 0) {
		return fmt.Errorf("MaximumRetryCount must be non-negative and only applies to on-failure")
	}
	if h := request.Healthcheck; h != nil {
		if h.Interval < 0 || h.Timeout < 0 || h.StartPeriod < 0 || h.StartInterval < 0 || h.Retries < 0 {
			return fmt.Errorf("healthcheck durations and retries must be non-negative")
		}
		if len(h.Test) > 0 {
			switch h.Test[0] {
			case "NONE":
				if len(h.Test) != 1 {
					return fmt.Errorf("NONE healthcheck cannot contain a command")
				}
			case "CMD", "CMD-SHELL":
				if len(h.Test) < 2 {
					return fmt.Errorf("healthcheck command is required")
				}
			default:
				return fmt.Errorf("unknown healthcheck test type %q", h.Test[0])
			}
		}
	}
	return nil
}

func rawMeaningful(raw json.RawMessage) bool {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return true
	}
	return meaningfulConfigValue(value)
}

func meaningfulConfigValue(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case string:
		return value != ""
	case float64:
		return value != 0
	case []any:
		return len(value) != 0
	case map[string]any:
		for _, nested := range value {
			if meaningfulConfigValue(nested) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func (c *Container) captureCreateMetadata(request *createContainerRequest) {
	metadata := containerCreateMetadata{config: make(map[string]json.RawMessage), hostConfig: make(map[string]json.RawMessage)}
	for field, raw := range request.rawConfig {
		switch field {
		case "HostConfig", "NetworkingConfig", "Image", "Labels", "ExposedPorts":
			continue
		case "Hostname":
			if request.Hostname == "" {
				continue // Empty hostname uses the generated Docker default.
			}
		case "Cmd", "Env", "AttachStdout", "AttachStderr":
			// Commands/environment feed service fixtures. Attachment preferences
			// are retained; an actual unsupported attach request fails separately.
		case "Healthcheck":
			if request.Healthcheck == nil {
				continue // null inherits the image fixture's health metadata.
			}
			metadata.healthOverride = true
			if !slices.Equal(request.Healthcheck.Test, []string{"NONE"}) {
				metadata.metadataOnly = append(metadata.metadataOnly, "Config.Healthcheck")
			}
		default:
			if rawMeaningful(raw) {
				metadata.metadataOnly = append(metadata.metadataOnly, "Config."+field)
			}
		}
		metadata.config[field] = slices.Clone(raw)
	}
	for field, raw := range request.rawHostConfig {
		switch field {
		case "NetworkMode", "PortBindings", "RestartPolicy":
			continue
		case "ConsoleSize":
			var size []int
			if err := json.Unmarshal(raw, &size); err != nil || len(size) != 2 || size[0] != 0 || size[1] != 0 {
				if rawMeaningful(raw) {
					metadata.metadataOnly = append(metadata.metadataOnly, "HostConfig.ConsoleSize")
				}
			}
		default:
			if rawMeaningful(raw) {
				metadata.metadataOnly = append(metadata.metadataOnly, "HostConfig."+field)
			}
		}
		metadata.hostConfig[field] = slices.Clone(raw)
	}
	c.RestartPolicy = request.HostConfig.RestartPolicy
	slices.Sort(metadata.metadataOnly)
	c.createMetadata = metadata
	c.ExposedPorts = make(map[string]any, len(request.ExposedPorts))
	for port := range request.ExposedPorts {
		c.ExposedPorts[port] = map[string]any{}
	}
}

func (c *Container) metadataWarnings(w http.ResponseWriter) []string {
	c.mu.Lock()
	fields := slices.Clone(c.createMetadata.metadataOnly)
	c.mu.Unlock()
	if len(fields) == 0 {
		return []string{}
	}
	// The existing matrix runner reads this marker from the HTTP ledger even
	// on a 201 response, so metadata-only requests cannot produce a green test.
	w.Header().Set("X-Docker-Zero-Unsupported", "true")
	w.Header().Set("X-Docker-Zero-Metadata-Only", strings.Join(fields, ", "))
	return []string{"docker-zero stores but does not execute/model: " + strings.Join(fields, ", ")}
}
