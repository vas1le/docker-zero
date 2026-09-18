package main

import (
	"fmt"
	"strconv"
	"strings"
)

type dockerAPIVersion struct{ major, minor int }

func parseDockerAPIVersion(text string) (dockerAPIVersion, bool) {
	parts := strings.Split(text, ".")
	if len(parts) != 2 {
		return dockerAPIVersion{}, false
	}
	for _, part := range parts {
		if part == "" {
			return dockerAPIVersion{}, false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return dockerAPIVersion{}, false
			}
		}
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return dockerAPIVersion{}, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return dockerAPIVersion{}, false
	}
	return dockerAPIVersion{major, minor}, true
}

func requestedAPIVersion(path string) (string, dockerAPIVersion, bool) {
	first, _, ok := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if !ok || !strings.HasPrefix(first, "v") {
		return "", dockerAPIVersion{}, false
	}
	text := strings.TrimPrefix(first, "v")
	version, ok := parseDockerAPIVersion(text)
	return text, version, ok
}

func (v dockerAPIVersion) less(other dockerAPIVersion) bool {
	return v.major < other.major || (v.major == other.major && v.minor < other.minor)
}

func validateAPIVersion(path string) error {
	text, version, present := requestedAPIVersion(path)
	if !present {
		return nil
	}
	minimum, _ := parseDockerAPIVersion(minAPIVersion)
	maximum, _ := parseDockerAPIVersion(apiVersion)
	if version.less(minimum) {
		return fmt.Errorf("client version %s is too old. Minimum supported API version is %s", text, minAPIVersion)
	}
	if maximum.less(version) {
		return fmt.Errorf("client version %s is too new. Maximum supported API version is %s", text, apiVersion)
	}
	return nil
}

func stripAPIVersion(path string) string {
	if _, _, ok := requestedAPIVersion(path); ok {
		_, rest, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
		return "/" + rest
	}
	return path
}
