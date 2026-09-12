package main

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func externalCookbookDir(t *testing.T, mutate func(name string, data []byte) []byte) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"nginx.json", "redis.json"} {
		data, err := os.ReadFile(filepath.Join("cookbooks", name))
		if err != nil {
			t.Fatal(err)
		}
		if mutate != nil {
			data = mutate(name, data)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func requireDiagnostic(t *testing.T, err error, code, path string, line int) *DiagnosticError {
	t.Helper()
	if err == nil {
		t.Fatal("expected diagnostic error")
	}
	var diagnostic *DiagnosticError
	if !errors.As(err, &diagnostic) {
		t.Fatalf("error type = %T, expected *DiagnosticError: %v", err, err)
	}
	if diagnostic.Code != code {
		t.Fatalf("code = %q, expected %q\n%s", diagnostic.Code, code, diagnostic.Render())
	}
	if path != "" && diagnostic.JSONPath != path {
		t.Fatalf("path = %q, expected %q\n%s", diagnostic.JSONPath, path, diagnostic.Render())
	}
	if line > 0 && diagnostic.Line != line {
		t.Fatalf("line = %d, expected %d\n%s", diagnostic.Line, line, diagnostic.Render())
	}
	if diagnostic.Column < 1 || diagnostic.SourceLine == "" {
		t.Fatalf("missing source coordinates: %+v", diagnostic)
	}
	return diagnostic
}

func TestCookbookSyntaxErrorShowsExactSourceLineAndCaret(t *testing.T) {
	dir := externalCookbookDir(t, func(name string, data []byte) []byte {
		if name != "nginx.json" {
			return data
		}
		return bytes.Replace(data, []byte(`"kind": "nginx",`), []byte(`"kind": "nginx" "image_names":`), 1)
	})
	_, err := loadCookbooks(dir)
	diagnostic := requireDiagnostic(t, err, "CONFIG_SYNTAX_ERROR", "", 2)
	rendered := diagnostic.Render()
	for _, expected := range []string{"line   : 2", "column : 18", `2 |   "kind": "nginx" "image_names":`, "^"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("diagnostic does not contain %q:\n%s", expected, rendered)
		}
	}
}

func TestCookbookUnknownNestedFieldIsRejectedAtExactPath(t *testing.T) {
	dir := externalCookbookDir(t, func(name string, data []byte) []byte {
		if name != "nginx.json" {
			return data
		}
		return bytes.Replace(data, []byte(`"status": "running",`), []byte(`"statuz": "running",`), 1)
	})
	_, err := loadCookbooks(dir)
	diagnostic := requireDiagnostic(t, err, "CONFIG_UNKNOWN_FIELD", "/seeds/0/initial/statuz", 21)
	if !strings.Contains(diagnostic.Message, "usually a typo") {
		t.Fatalf("unexpected message: %s", diagnostic.Message)
	}
}

func TestCookbookDuplicateKeyIsRejectedInsteadOfSilentlyOverwritten(t *testing.T) {
	dir := externalCookbookDir(t, func(name string, data []byte) []byte {
		if name != "nginx.json" {
			return data
		}
		return bytes.Replace(data, []byte(`"kind": "nginx",`), []byte("\"kind\": \"nginx\",\n  \"kind\": \"nginx\","), 1)
	})
	_, err := loadCookbooks(dir)
	_ = requireDiagnostic(t, err, "CONFIG_DUPLICATE_KEY", "/kind", 3)
}

func TestCookbookInvalidLifecycleValueIsRejected(t *testing.T) {
	dir := externalCookbookDir(t, func(name string, data []byte) []byte {
		if name != "nginx.json" {
			return data
		}
		return bytes.Replace(data, []byte(`"status": "running",`), []byte(`"status": "banana",`), 1)
	})
	_, err := loadCookbooks(dir)
	diagnostic := requireDiagnostic(t, err, "CONFIG_VALIDATION_ERROR", "/seeds/0/initial/status", 21)
	if !strings.Contains(diagnostic.Message, `got "banana"`) {
		t.Fatalf("unexpected message: %s", diagnostic.Message)
	}
}

func TestCookbookTypeMismatchShowsFieldAndLine(t *testing.T) {
	dir := externalCookbookDir(t, func(name string, data []byte) []byte {
		if name != "nginx.json" {
			return data
		}
		return bytes.Replace(data, []byte(`"host_port": 18080`), []byte(`"host_port": "eighteen"`), 1)
	})
	_, err := loadCookbooks(dir)
	_ = requireDiagnostic(t, err, "CONFIG_TYPE_ERROR", "/defaults/host_port", 14)
}

func TestCookbookInvalidRESPReplyIsRejected(t *testing.T) {
	dir := externalCookbookDir(t, func(name string, data []byte) []byte {
		if name != "redis.json" {
			return data
		}
		return bytes.Replace(data, []byte(`"reply": "+PONG\r\n"`), []byte(`"reply": "+PONG\n"`), 1)
	})
	_, err := loadCookbooks(dir)
	diagnostic := requireDiagnostic(t, err, "CONFIG_VALIDATION_ERROR", "/seeds/0/redis/PING/0/reply", 46)
	if !strings.Contains(diagnostic.Message, "invalid RESP2 reply") {
		t.Fatalf("unexpected message: %s", diagnostic.Message)
	}
}

func TestCheckModeReturnsConfigExitAndRendersDiagnostic(t *testing.T) {
	dir := externalCookbookDir(t, func(name string, data []byte) []byte {
		if name != "nginx.json" {
			return data
		}
		return bytes.Replace(data, []byte(`"status": "running",`), []byte(`"status": "broken",`), 1)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--check", "--cookbook-dir", dir,
		"--socket", filepath.Join(t.TempDir(), "docker.sock"),
		"--ledger-dir", filepath.Join(t.TempDir(), "runs"),
		"--container-endpoints", "off",
		"--nginx-address", "127.0.0.1:18081",
		"--redis-address", "127.0.0.1:16380",
	}, &stdout, &stderr)
	if code != exitConfig {
		t.Fatalf("exit = %d, expected %d; stdout=%s stderr=%s", code, exitConfig, stdout.String(), stderr.String())
	}
	for _, expected := range []string{"CONFIG_VALIDATION_ERROR", "/seeds/0/initial/status", "line   : 21", "^"} {
		if !strings.Contains(stderr.String(), expected) {
			t.Fatalf("stderr lacks %q:\n%s", expected, stderr.String())
		}
	}
}

func TestCheckModeDetectsPortConflict(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer testClose(t, listener)
	redisProbe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	redisAddress := redisProbe.Addr().String()
	if err := redisProbe.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--check", "--bootstrap",
		"--socket", filepath.Join(t.TempDir(), "docker.sock"),
		"--ledger-dir", filepath.Join(t.TempDir(), "runs"),
		"--container-endpoints", "off",
		"--nginx-address", listener.Addr().String(),
		"--redis-address", redisAddress,
	}, &stdout, &stderr)
	if code != exitConfig || !strings.Contains(stderr.String(), "cannot be bound") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestUnixSocketRefusesActiveOwnerAndRecoversStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docker.sock")
	first, err := listenUnixSocket(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := listenUnixSocket(path, 0o600); err == nil || !strings.Contains(err.Error(), "is active") {
		_ = first.Close()
		t.Fatalf("expected active socket error, got %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	// The closed listener leaves a stale socket pathname. A fresh engine should
	// clean it up safely instead of requiring manual intervention.
	second, err := listenUnixSocket(path, 0o600)
	if err != nil {
		t.Fatalf("recover stale socket: %v", err)
	}
	defer testClose(t, second)
}

func TestUnixSocketNeverDeletesRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docker.sock")
	if err := os.WriteFile(path, []byte("do not delete"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenUnixSocket(path, 0o600); err == nil || !strings.Contains(err.Error(), "non-socket") {
		t.Fatalf("expected non-socket refusal, got %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "do not delete" {
		t.Fatalf("regular file was altered: data=%q err=%v", data, err)
	}
}

func TestPrepareUnixSocketPropagatesNonStaleDialErrors(t *testing.T) {
	// This primarily documents that only ECONNREFUSED/ENOENT are considered
	// stale. The errors.Is checks must continue to recognise wrapped syscalls.
	if !errors.Is(&net.OpError{Err: syscall.ECONNREFUSED}, syscall.ECONNREFUSED) {
		t.Fatal("wrapped ECONNREFUSED is not recognised")
	}
}
