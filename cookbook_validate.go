package main

import (
	"fmt"
	"net"
	"strings"
)

func (c *Cookbook) validate() error {
	kind := strings.ToLower(strings.TrimSpace(c.Kind))
	if kind == "" {
		return validationError("/kind", "kind is required")
	}
	if kind != "nginx" && kind != "redis" {
		return validationError("/kind", "kind must be nginx or redis, got %q", c.Kind)
	}
	c.Kind = kind

	if strings.TrimSpace(c.Defaults.Name) == "" {
		return validationError("/defaults/name", "defaults.name is required")
	}
	if strings.ContainsAny(c.Defaults.Name, "/\\\x00\r\n\t ") {
		return validationError("/defaults/name", "defaults.name %q contains a forbidden character", c.Defaults.Name)
	}
	if strings.TrimSpace(c.Defaults.Image) == "" {
		return validationError("/defaults/image", "defaults.image is required")
	}
	expectedServiceType := map[string]string{"nginx": "http", "redis": "redis"}[kind]
	if c.Defaults.ServiceType != expectedServiceType {
		return validationError("/defaults/service_type", "defaults.service_type for %s must be %q", kind, expectedServiceType)
	}
	ip := net.ParseIP(c.Defaults.IPAddress)
	if ip == nil {
		return validationError("/defaults/ip_address", "defaults.ip_address %q is not a valid IP address", c.Defaults.IPAddress)
	}
	if !ip.IsLoopback() {
		return validationError("/defaults/ip_address", "defaults.ip_address %q is not loopback; docker-zero refuses to expose fake services on a non-loopback container address", c.Defaults.IPAddress)
	}
	if c.Defaults.ContainerPort < 1 || c.Defaults.ContainerPort > 65535 {
		return validationError("/defaults/container_port", "defaults.container_port must be between 1 and 65535")
	}
	if strings.TrimSpace(c.Defaults.HostIP) == "" || net.ParseIP(c.Defaults.HostIP) == nil {
		return validationError("/defaults/host_ip", "defaults.host_ip %q is not a valid IP address", c.Defaults.HostIP)
	}
	if c.Defaults.HostPort < 1 || c.Defaults.HostPort > 65535 {
		return validationError("/defaults/host_port", "defaults.host_port must be between 1 and 65535")
	}

	if len(c.ImageNames) == 0 {
		return validationError("/image_names", "image_names must contain at least one image matcher")
	}
	seenImages := make(map[string]struct{})
	for index, image := range c.ImageNames {
		image = strings.TrimSpace(image)
		if image == "" {
			return validationError(fmt.Sprintf("/image_names/%d", index), "image matcher cannot be empty")
		}
		key := strings.ToLower(image)
		if _, exists := seenImages[key]; exists {
			return validationError(fmt.Sprintf("/image_names/%d", index), "duplicate image matcher %q", image)
		}
		seenImages[key] = struct{}{}
	}

	if len(c.Seeds) != 3 {
		return validationError("/seeds", "seeds must contain exactly 0, 1, and 2")
	}
	for seed := range c.Seeds {
		if seed != "0" && seed != "1" && seed != "2" {
			return validationError("/seeds/"+escapeJSONPointer(seed), "unsupported seed %q; this release supports exactly 0, 1, and 2", seed)
		}
	}
	for _, seed := range []string{"0", "1", "2"} {
		if _, ok := c.Seeds[seed]; !ok {
			return validationError("/seeds", "seed %s is required", seed)
		}
	}

	for _, seed := range []string{"0", "1", "2"} {
		scenario := c.Seeds[seed]
		basePath := "/seeds/" + seed
		if strings.TrimSpace(scenario.Description) == "" {
			return validationError(basePath+"/description", "scenario description is required")
		}
		if err := validateStatePatch(basePath+"/initial", scenario.Initial, false); err != nil {
			return err
		}
		if seed == "0" {
			if err := validateHealthyBaseline(basePath+"/initial", scenario.Initial); err != nil {
				return err
			}
		}
		if kind == "nginx" {
			if len(scenario.HTTP) == 0 {
				return validationError(basePath+"/http", "nginx scenario must define HTTP responses")
			}
			if _, ok := scenario.HTTP["GET /health"]; !ok {
				return validationError(basePath+"/http", "nginx scenario must define GET /health")
			}
			if len(scenario.Redis) != 0 {
				return validationError(basePath+"/redis", "nginx cookbook cannot contain Redis responses")
			}
		} else {
			if len(scenario.Redis) == 0 {
				return validationError(basePath+"/redis", "redis scenario must define Redis responses")
			}
			if _, ok := scenario.Redis["PING"]; !ok {
				return validationError(basePath+"/redis", "redis scenario must define PING")
			}
			if len(scenario.HTTP) != 0 {
				return validationError(basePath+"/http", "redis cookbook cannot contain HTTP responses")
			}
		}

		if err := validateExecFixtures(basePath+"/exec", scenario.Exec); err != nil {
			return err
		}
		for index, transition := range scenario.Transitions {
			path := fmt.Sprintf("%s/transitions/%d", basePath, index)
			if strings.TrimSpace(transition.Event) == "" {
				return validationError(path+"/event", "transition event is required")
			}
			if transition.Count < 0 {
				return validationError(path+"/count", "transition count cannot be negative")
			}
			if err := validateStateValue(path+"/when_status", "status", transition.WhenStatus, true); err != nil {
				return err
			}
			if err := validateStateValue(path+"/when_health", "health", transition.WhenHealth, true); err != nil {
				return err
			}
			if isZeroStatePatch(transition.Set) {
				return validationError(path+"/set", "transition set patch cannot be empty")
			}
			if err := validateStatePatch(path+"/set", transition.Set, true); err != nil {
				return err
			}
			if err := validateTransitionEvent(path+"/event", transition.Event, scenario); err != nil {
				return err
			}
		}

		for route, responses := range scenario.HTTP {
			routePath := basePath + "/http/" + escapeJSONPointer(route)
			if err := validateHTTPRoute(routePath, route); err != nil {
				return err
			}
			if len(responses) == 0 {
				return validationError(routePath, "route %q has no responses", route)
			}
			if err := validateResponseOrderHTTP(routePath, responses); err != nil {
				return err
			}
			for index, response := range responses {
				path := fmt.Sprintf("%s/%d", routePath, index)
				if err := validateStateValue(path+"/when_status", "status", response.WhenStatus, true); err != nil {
					return err
				}
				if err := validateStateValue(path+"/when_health", "health", response.WhenHealth, true); err != nil {
					return err
				}
				if response.Status < 100 || response.Status > 599 {
					return validationError(path+"/status", "HTTP status must be between 100 and 599")
				}
				if response.DelayMS < 0 {
					return validationError(path+"/delay_ms", "delay_ms cannot be negative")
				}
				for header, value := range response.Headers {
					if !validHTTPHeaderName(header) {
						return validationError(path+"/headers/"+escapeJSONPointer(header), "invalid HTTP header name %q", header)
					}
					if strings.ContainsAny(value, "\r\n") {
						return validationError(path+"/headers/"+escapeJSONPointer(header), "HTTP header value contains a newline")
					}
				}
				if err := validateStatePatch(path+"/set", response.StatePatch, true); err != nil {
					return err
				}
			}
		}

		for command, responses := range scenario.Redis {
			commandPath := basePath + "/redis/" + escapeJSONPointer(command)
			if command != "*" && command != strings.ToUpper(command) {
				return validationError(commandPath, "Redis command keys must be uppercase")
			}
			if command != "*" && !validRedisCommandName(command) {
				return validationError(commandPath, "invalid Redis command name %q", command)
			}
			if len(responses) == 0 {
				return validationError(commandPath, "command %q has no responses", command)
			}
			if err := validateResponseOrderRedis(commandPath, responses); err != nil {
				return err
			}
			for index, response := range responses {
				path := fmt.Sprintf("%s/%d", commandPath, index)
				if err := validateStateValue(path+"/when_status", "status", response.WhenStatus, true); err != nil {
					return err
				}
				if err := validateStateValue(path+"/when_health", "health", response.WhenHealth, true); err != nil {
					return err
				}
				if response.DelayMS < 0 {
					return validationError(path+"/delay_ms", "delay_ms cannot be negative")
				}
				if response.Reply == "" && !response.Close {
					return validationError(path+"/reply", "Redis response requires reply or close=true")
				}
				if response.Reply != "" {
					if err := validateRESPReply(response.Reply); err != nil {
						return validationError(path+"/reply", "invalid RESP2 reply: %v", err)
					}
				}
				if err := validateStatePatch(path+"/set", response.StatePatch, true); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
