package main

import (
	"fmt"
	"strconv"
	"strings"
)

type dockerAPIVersion struct{ major, minor int }

func parseDockerAPIVersion(value string) (dockerAPIVersion, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return dockerAPIVersion{}, fmt.Errorf("invalid API version %q", value)
	}
	var numbers [2]int
	for i, part := range parts {
		if part == "" {
			return dockerAPIVersion{}, fmt.Errorf("invalid API version %q", value)
		}
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return dockerAPIVersion{}, fmt.Errorf("invalid API version %q", value)
			}
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return dockerAPIVersion{}, fmt.Errorf("invalid API version %q", value)
		}
		numbers[i] = n
	}
	return dockerAPIVersion{numbers[0], numbers[1]}, nil
}

func (a dockerAPIVersion) before(b dockerAPIVersion) bool {
	return a.major < b.major || (a.major == b.major && a.minor < b.minor)
}

func apiVersionPrefix(path string) string {
	first, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if len(first) < 2 || first[0] != 'v' {
		return ""
	}
	// Don't mistake unversioned /version or /volumes for a version prefix.
	if (first[1] >= '0' && first[1] <= '9') || first[1] == '.' || first[1] == '+' || first[1] == '-' {
		return first[1:]
	}
	return ""
}

func validateAPIVersionPath(path string) error {
	value := apiVersionPrefix(path)
	if value == "" {
		return nil
	}
	requested, err := parseDockerAPIVersion(value)
	if err != nil {
		return err
	}
	minimum, _ := parseDockerAPIVersion(minAPIVersion)
	maximum, _ := parseDockerAPIVersion(apiVersion)
	if requested.before(minimum) {
		return fmt.Errorf("client version %s is too old. Minimum supported API version is %s", value, minAPIVersion)
	}
	if maximum.before(requested) {
		return fmt.Errorf("client version %s is too new. Maximum supported API version is %s", value, apiVersion)
	}
	return nil
}
