package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
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
