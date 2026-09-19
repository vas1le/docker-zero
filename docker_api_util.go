package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
)

func writeDockerStreamFrame(w io.Writer, stream byte, payload []byte) {
	if len(payload) == 0 {
		return
	}
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	_, _ = w.Write(header)
	_, _ = w.Write(payload)
}

// Docker API versions are integer pairs, not decimal numbers: 1.100 > 1.43.
func parseAPIVersion(value string) (major, minor int, err error) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid API version %q", value)
	}
	for _, part := range parts {
		if part == "" || strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return 0, 0, fmt.Errorf("invalid API version %q", value)
		}
	}
	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid API version %q", value)
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid API version %q", value)
	}
	return major, minor, nil
}

func stripAPIVersion(path string) string {
	parts := strings.SplitN(path, "/", 3)
	if len(parts) == 3 && strings.HasPrefix(parts[1], "v") {
		if _, _, err := parseAPIVersion(strings.TrimPrefix(parts[1], "v")); err == nil {
			return "/" + parts[2]
		}
	}
	return path
}

func validateAPIVersionPath(path string) error {
	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 3 || len(parts[1]) < 2 || parts[1][0] != 'v' || parts[1][1] < '0' || parts[1][1] > '9' {
		return nil // Unversioned endpoint; do not confuse /volumes with a version.
	}
	value := parts[1][1:]
	major, minor, err := parseAPIVersion(value)
	if err != nil {
		return err
	}
	minMajor, minMinor, _ := parseAPIVersion(minAPIVersion)
	maxMajor, maxMinor, _ := parseAPIVersion(apiVersion)
	if major < minMajor || (major == minMajor && minor < minMinor) {
		return fmt.Errorf("client version %s is too old. Minimum supported API version is %s", value, minAPIVersion)
	}
	if major > maxMajor || (major == maxMajor && minor > maxMinor) {
		return fmt.Errorf("client version %s is too new. Maximum supported API version is %s", value, apiVersion)
	}
	return nil
}

func decodeJSON(body io.Reader, target any) error {
	decoder := json.NewDecoder(io.LimitReader(body, 16<<20))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeDockerError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

func writeUnsupportedDockerError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("X-Docker-Zero-Unsupported", "true")
	writeDockerError(w, status, message)
}

func dockerMachineArchitecture() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return runtime.GOARCH
	}
}

func parseBoolQuery(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "1" || value == "true" || value == "yes"
}

func firstOrEmpty(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return items[0]
}

func restOrEmpty(items []string) []string {
	if len(items) <= 1 {
		return []string{}
	}
	return items[1:]
}

// compile-time guard for APIs that return scanners/readers in future versions.
var _ = bufio.ErrInvalidUnreadByte
