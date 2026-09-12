package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// dockerFilters is the normalized representation of Docker Engine's filters
// query parameter. Moby clients encode it as map[string]map[string]bool; a few
// clients/tools use arrays, so docker-zero accepts both forms.
type dockerFilters map[string][]string

func parseDockerFilters(raw string) (dockerFilters, error) {
	out := dockerFilters{}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return out, nil
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		return nil, err
	}
	for key, payload := range generic {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			return nil, fmt.Errorf("empty filter name")
		}
		var object map[string]bool
		if err := json.Unmarshal(payload, &object); err == nil && object != nil {
			for value, enabled := range object {
				if enabled {
					out[key] = append(out[key], value)
				}
			}
			continue
		}
		var list []string
		if err := json.Unmarshal(payload, &list); err == nil {
			out[key] = append(out[key], list...)
			continue
		}
		var one string
		if err := json.Unmarshal(payload, &one); err == nil {
			out[key] = append(out[key], one)
			continue
		}
		return nil, fmt.Errorf("unsupported value for filter %q", key)
	}
	return out, nil
}

func matchLabels(labels map[string]string, expressions []string) bool {
	for _, expr := range expressions {
		if key, value, ok := strings.Cut(expr, "!="); ok {
			if actual, exists := labels[key]; exists && actual == value {
				return false
			}
			continue
		}
		if key, value, ok := strings.Cut(expr, "="); ok {
			if actual, exists := labels[key]; !exists || actual != value {
				return false
			}
			continue
		}
		if _, exists := labels[expr]; !exists {
			return false
		}
	}
	return true
}

func matchesAnySubstring(value string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if strings.Contains(value, p) {
			return true
		}
	}
	return false
}

func matchesAnyExact(value string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if value == p {
			return true
		}
	}
	return false
}

func containerMatchesFilters(c *Container, f dockerFilters) bool {
	if values := f["label"]; !matchLabels(c.Labels, values) {
		return false
	}
	if values := f["name"]; len(values) > 0 && !matchesAnySubstring(c.Name, values) {
		return false
	}
	if values := f["id"]; len(values) > 0 {
		matched := false
		for _, v := range values {
			if strings.HasPrefix(c.ID, v) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if values := f["status"]; len(values) > 0 && !matchesAnyExact(c.stateSnapshot().Status, values) {
		return false
	}
	// Compose may send ancestor filters for user-facing commands. Matching the
	// image name is sufficient for docker-zero's simulated image model.
	if values := f["ancestor"]; len(values) > 0 {
		matched := false
		for _, v := range values {
			if c.Image == v || c.ImageID == v || strings.HasPrefix(c.ImageID, v) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func networkMatchesFilters(n *Network, f dockerFilters) bool {
	if values := f["label"]; !matchLabels(n.Labels, values) {
		return false
	}
	if values := f["name"]; len(values) > 0 && !matchesAnySubstring(n.Name, values) {
		return false
	}
	if values := f["id"]; len(values) > 0 {
		matched := false
		for _, v := range values {
			if strings.HasPrefix(n.ID, v) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if values := f["driver"]; len(values) > 0 && !matchesAnyExact(n.Driver, values) {
		return false
	}
	if values := f["scope"]; len(values) > 0 && !matchesAnyExact(n.Scope, values) {
		return false
	}
	if values := f["type"]; len(values) > 0 {
		// docker-zero has only custom local networks plus the built-in bridge.
		typ := "custom"
		if n.Name == "bridge" {
			typ = "builtin"
		}
		if !matchesAnyExact(typ, values) {
			return false
		}
	}
	return true
}

func volumeMatchesFilters(v *Volume, f dockerFilters) bool {
	if values := f["label"]; !matchLabels(v.Labels, values) {
		return false
	}
	if values := f["name"]; len(values) > 0 && !matchesAnySubstring(v.Name, values) {
		return false
	}
	if values := f["driver"]; len(values) > 0 && !matchesAnyExact(v.Driver, values) {
		return false
	}
	return true
}
