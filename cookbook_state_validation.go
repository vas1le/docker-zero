package main

import (
	"net/http"
	"sort"
	"strings"
	"unicode"
)

func validateHealthyBaseline(path string, patch StatePatch) error {
	if patch.Status == nil || *patch.Status != "running" {
		return validationError(path+"/status", "seed 0 must start with status=running")
	}
	if patch.Running == nil || !*patch.Running {
		return validationError(path+"/running", "seed 0 must start with running=true")
	}
	if patch.Restarting == nil || *patch.Restarting {
		return validationError(path+"/restarting", "seed 0 must start with restarting=false")
	}
	if patch.Health == nil || *patch.Health != "healthy" {
		return validationError(path+"/health", "seed 0 must start with health=healthy")
	}
	if patch.ExitCode == nil || *patch.ExitCode != 0 {
		return validationError(path+"/exit_code", "seed 0 must start with exit_code=0")
	}
	if patch.RestartCount == nil || *patch.RestartCount != 0 {
		return validationError(path+"/restart_count", "seed 0 must start with restart_count=0")
	}
	return nil
}

func validateStatePatch(path string, patch StatePatch, allowEmpty bool) error {
	if !allowEmpty && isZeroStatePatch(patch) {
		return validationError(path, "state patch cannot be empty")
	}
	if patch.Status != nil {
		if err := validateStateValue(path+"/status", "status", *patch.Status, false); err != nil {
			return err
		}
	}
	if patch.Health != nil {
		if err := validateStateValue(path+"/health", "health", *patch.Health, true); err != nil {
			return err
		}
	}
	if patch.ExitCode != nil && (*patch.ExitCode < 0 || *patch.ExitCode > 255) {
		return validationError(path+"/exit_code", "exit_code must be between 0 and 255")
	}
	if patch.RestartCount != nil && *patch.RestartCount < 0 {
		return validationError(path+"/restart_count", "restart_count cannot be negative")
	}
	if patch.RestartCountDelta < 0 {
		return validationError(path+"/restart_count_delta", "restart_count_delta cannot be negative")
	}
	if patch.Error != nil && strings.ContainsRune(*patch.Error, '\x00') {
		return validationError(path+"/error", "error cannot contain a NUL byte")
	}
	if patch.Status == nil && (patch.Running != nil || patch.Restarting != nil || patch.Dead != nil) {
		return validationError(path, "running, restarting, and dead flags require status in the same patch")
	}

	if patch.Status != nil {
		switch *patch.Status {
		case "running":
			if patch.Running != nil && !*patch.Running {
				return validationError(path+"/running", "status=running conflicts with running=false")
			}
			if patch.Restarting != nil && *patch.Restarting {
				return validationError(path+"/restarting", "status=running conflicts with restarting=true")
			}
			if patch.Dead != nil && *patch.Dead {
				return validationError(path+"/dead", "status=running conflicts with dead=true")
			}
		case "restarting":
			if patch.Running != nil && !*patch.Running {
				return validationError(path+"/running", "status=restarting conflicts with running=false")
			}
			if patch.Restarting != nil && !*patch.Restarting {
				return validationError(path+"/restarting", "status=restarting conflicts with restarting=false")
			}
			if patch.Dead != nil && *patch.Dead {
				return validationError(path+"/dead", "status=restarting conflicts with dead=true")
			}
		case "created", "exited":
			if patch.Running != nil && *patch.Running {
				return validationError(path+"/running", "status=%s conflicts with running=true", *patch.Status)
			}
			if patch.Restarting != nil && *patch.Restarting {
				return validationError(path+"/restarting", "status=%s conflicts with restarting=true", *patch.Status)
			}
			if patch.Dead != nil && *patch.Dead {
				return validationError(path+"/dead", "status=%s conflicts with dead=true", *patch.Status)
			}
		case "dead":
			if patch.Running != nil && *patch.Running {
				return validationError(path+"/running", "status=dead conflicts with running=true")
			}
			if patch.Restarting != nil && *patch.Restarting {
				return validationError(path+"/restarting", "status=dead conflicts with restarting=true")
			}
			if patch.Dead != nil && !*patch.Dead {
				return validationError(path+"/dead", "status=dead conflicts with dead=false")
			}
		}
	}
	if patch.Dead != nil && *patch.Dead && (patch.Status == nil || *patch.Status != "dead") {
		return validationError(path+"/dead", "dead=true requires status=dead in the same patch")
	}
	return nil
}

func validateStateValue(path, kind, value string, allowEmpty bool) error {
	if value == "" && allowEmpty {
		return nil
	}
	var allowed map[string]bool
	if kind == "status" {
		allowed = map[string]bool{"created": true, "running": true, "restarting": true, "exited": true, "dead": true}
	} else {
		allowed = map[string]bool{"starting": true, "healthy": true, "unhealthy": true}
	}
	if !allowed[value] {
		values := make([]string, 0, len(allowed))
		for candidate := range allowed {
			values = append(values, candidate)
		}
		sort.Strings(values)
		return validationError(path, "%s must be one of %s, got %q", kind, strings.Join(values, ", "), value)
	}
	return nil
}

func validateTransitionEvent(path, event string, scenario Scenario) error {
	if strings.HasPrefix(event, "docker.") {
		allowed := map[string]bool{
			"docker.observe": true, "docker.inspect": true, "docker.list": true,
			"docker.start": true, "docker.stop": true, "docker.restart": true,
			"docker.kill": true, "docker.wait": true, "docker.logs": true,
			"docker.top": true, "docker.archive": true,
		}
		if !allowed[event] {
			return validationError(path, "unsupported Docker transition event %q", event)
		}
		return nil
	}
	if strings.HasPrefix(event, "http.") {
		route := strings.TrimPrefix(event, "http.")
		if err := validateHTTPRoute(path, route); err != nil {
			return err
		}
		if _, ok := scenario.HTTP[route]; !ok {
			if _, wildcard := scenario.HTTP["*"]; !wildcard {
				return validationError(path, "transition event %q has no matching HTTP route", event)
			}
		}
		return nil
	}
	if strings.HasPrefix(event, "redis.") {
		command := strings.TrimPrefix(event, "redis.")
		if command == "" || command != strings.ToUpper(command) || !validRedisCommandName(command) {
			return validationError(path, "invalid Redis transition event %q", event)
		}
		if _, ok := scenario.Redis[command]; !ok {
			if _, wildcard := scenario.Redis["*"]; !wildcard {
				return validationError(path, "transition event %q has no matching Redis response", event)
			}
		}
		return nil
	}
	return validationError(path, "transition event must start with docker., http., or redis.")
}

func validateHTTPRoute(path, route string) error {
	if route == "*" {
		return nil
	}
	parts := strings.SplitN(route, " ", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return validationError(path, "HTTP route %q must use the form METHOD /path", route)
	}
	method := parts[0]
	allowedMethods := map[string]bool{
		http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true,
		http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true,
	}
	if !allowedMethods[method] {
		return validationError(path, "unsupported or non-uppercase HTTP method %q", method)
	}
	if !strings.HasPrefix(parts[1], "/") || strings.ContainsAny(parts[1], "\r\n\t ") {
		return validationError(path, "HTTP route path %q must begin with / and contain no whitespace", parts[1])
	}
	if strings.Contains(parts[1], "?") {
		return validationError(path, "HTTP route path must not include a query string; matching uses URL.Path")
	}
	return nil
}

func validHTTPHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("!#$%&'*+-.^_`|~", r) {
			continue
		}
		return false
	}
	return true
}

func validRedisCommandName(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}
