package main

import (
	"fmt"
	"strconv"
	"strings"
)

// Parse integer version components, never floating point (1.10 is not 1.1).
func parseDockerVersion(value string) (major, minor int, err error) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid API version %q", value)
	}
	values := [2]int{}
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return 0, 0, fmt.Errorf("invalid API version %q", value)
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return 0, 0, fmt.Errorf("invalid API version %q", value)
			}
		}
		values[i], err = strconv.Atoi(part)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid API version %q", value)
		}
	}
	return values[0], values[1], nil
}

func apiVersionPrefix(path string) (string, string, bool) {
	segment, rest, found := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if len(segment) < 2 || segment[0] != 'v' {
		return "", path, false
	}
	// Do not mistake /version or /volumes for a version prefix.
	if (segment[1] < '0' || segment[1] > '9') && !strings.Contains(segment, ".") {
		return "", path, false
	}
	if !found {
		rest = ""
	}
	return segment[1:], "/" + rest, true
}

func validateAPIVersion(path string) error {
	requested, _, present := apiVersionPrefix(path)
	if !present {
		return nil
	}
	major, minor, err := parseDockerVersion(requested)
	if err != nil {
		return err
	}
	minMajor, minMinor, _ := parseDockerVersion(minAPIVersion)
	maxMajor, maxMinor, _ := parseDockerVersion(apiVersion)
	if major < minMajor || (major == minMajor && minor < minMinor) {
		return fmt.Errorf("client version %s is too old. Minimum supported API version is %s", requested, minAPIVersion)
	}
	if major > maxMajor || (major == maxMajor && minor > maxMinor) {
		return fmt.Errorf("client version %s is too new. Maximum supported API version is %s", requested, apiVersion)
	}
	return nil
}
