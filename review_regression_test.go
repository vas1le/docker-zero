package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCleanStopPreservesLastHealthState(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer ledger.Close()
	container, err := engine.createContainer("health-stop", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	container.start()
	before := container.stateSnapshot()
	if before.Health != "healthy" {
		t.Fatalf("precondition health = %q, expected healthy", before.Health)
	}
	container.stop(0)
	after := container.stateSnapshot()
	if after.Status != "exited" || after.ExitCode != 0 {
		t.Fatalf("stop state = %#v, expected exited/0", after)
	}
	if after.Health != before.Health {
		t.Fatalf("clean stop changed health from %q to %q", before.Health, after.Health)
	}
	container.mu.Lock()
	streak := container.Health.FailingStreak
	container.mu.Unlock()
	if streak != 0 {
		t.Fatalf("clean stop failing streak = %d, expected 0", streak)
	}
}

func TestConfigureContainerNetworksIsAtomicOnAllocationFailure(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer ledger.Close()
	container, err := engine.createContainer("atomic-network", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.configureContainerNetworks(container, "bridge", nil); err != nil {
		t.Fatal(err)
	}
	before := container.snapshot()
	bridge, err := engine.findNetwork("bridge")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bridge.Containers[container.ID]; !ok {
		t.Fatal("precondition: container is not attached to bridge")
	}

	target, err := engine.createNetwork("full", "bridge", nil)
	if err != nil {
		t.Fatal(err)
	}
	for host := 2; host <= 254; host++ {
		id := fmt.Sprintf("dummy-%03d", host)
		target.Containers[id] = map[string]any{"IPv4Address": fmt.Sprintf("127.20.%d.%d/24", target.Index, host)}
	}

	if err := engine.configureContainerNetworks(container, target.Name, nil); err == nil {
		t.Fatal("expected exhausted-network allocation to fail")
	}
	after := container.snapshot()
	if after.IPAddress != before.IPAddress || after.NetworkMode != before.NetworkMode {
		t.Fatalf("failed reconfiguration mutated container network: before=%#v after=%#v", before, after)
	}
	if _, ok := bridge.Containers[container.ID]; !ok {
		t.Fatal("failed reconfiguration detached container from previous network")
	}
	if _, ok := target.Containers[container.ID]; ok {
		t.Fatal("failed reconfiguration partially attached container to target network")
	}
}

func TestMacForContainerShortIDDoesNotPanic(t *testing.T) {
	for _, id := range []string{"123456", "1234567"} {
		if got := macForContainer(id); got != "02:42:7f:14:00:02" {
			t.Fatalf("macForContainer(%q) = %q", id, got)
		}
	}
	if got := macForContainer("12345678"); got != "02:42:12:34:56:78" {
		t.Fatalf("8-byte id MAC = %q", got)
	}
}

func TestUnixSocketPathLengthDiagnostic(t *testing.T) {
	path := "/tmp/" + strings.Repeat("x", 108)
	err := validateUnixSocketPath(path)
	if err == nil || !strings.Contains(err.Error(), "too long for AF_UNIX") {
		t.Fatalf("validateUnixSocketPath error = %v", err)
	}
}

func TestEventsAndSystemDFAreMachineClassifiedUnsupported(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer ledger.Close()
	api := &DockerAPI{engine: engine}
	for _, path := range []string{"/events", "/system/df"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		api.ServeHTTP(response, request)
		if response.Code != http.StatusNotImplemented {
			t.Fatalf("%s status = %d, expected %d", path, response.Code, http.StatusNotImplemented)
		}
		if got := response.Header().Get("X-Docker-Zero-Unsupported"); got != "true" {
			t.Fatalf("%s unsupported header = %q", path, got)
		}
	}
}

func TestPauseUnpauseAndExecConflict(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer ledger.Close()
	container, err := engine.createContainer("pause-test", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	container.start()
	api := &DockerAPI{engine: engine}

	request := httptest.NewRequest(http.MethodPost, "/containers/pause-test/pause", nil)
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("pause status = %d body=%s", response.Code, response.Body.String())
	}
	state := container.stateSnapshot()
	if state.Status != "paused" || !state.Running || !state.Paused {
		t.Fatalf("paused state = %#v", state)
	}

	request = httptest.NewRequest(http.MethodPost, "/containers/pause-test/exec", strings.NewReader(`{"Cmd":["true"]}`))
	response = httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("exec while paused status = %d, expected 409", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/containers/pause-test/unpause", nil)
	response = httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("unpause status = %d body=%s", response.Code, response.Body.String())
	}
	state = container.stateSnapshot()
	if state.Status != "running" || !state.Running || state.Paused {
		t.Fatalf("unpaused state = %#v", state)
	}
}
