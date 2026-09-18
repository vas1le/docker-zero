package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testEngine(t *testing.T, seed int) (*Engine, *Ledger) {
	t.Helper()
	cookbooks, err := loadCookbooks("")
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := newLedger(filepath.Join(t.TempDir(), "runs"), seed)
	if err != nil {
		t.Fatal(err)
	}
	return newEngine(cookbooks, seed, nil, ledger), ledger
}

func TestUnsupportedDockerRequestIsMachineClassified(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	api := &DockerAPI{engine: engine}
	request := httptest.NewRequest(http.MethodGet, "/v1.43/not-implemented", nil)
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, expected 404", response.Code)
	}
	if got := response.Header().Get("X-Docker-Zero-Unsupported"); got != "true" {
		t.Fatalf("unsupported header = %q", got)
	}
	runDir := ledger.RunDir()
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(runDir, "global.ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		var entry LedgerEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Channel != "docker.http" || entry.Event != "GET /not-implemented" {
			continue
		}
		responseDoc, ok := entry.Response.(map[string]any)
		if !ok || responseDoc["unsupported"] != true {
			t.Fatalf("ledger response is not marked unsupported: %#v", entry.Response)
		}
		found = true
	}
	if !found {
		t.Fatal("unsupported Docker request was not written to the ledger")
	}
}

func TestDockerStartTransitionRunsAfterBaseStart(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	cookbook := engine.cookbooks["nginx"]
	scenario := cookbook.Seeds["0"]
	scenario.Transitions = append(scenario.Transitions, Transition{
		Event: "docker.start",
		Count: 1,
		Once:  true,
		Set: StatePatch{
			Status:     stringPtr("exited"),
			Running:    boolPtr(false),
			Restarting: boolPtr(false),
			Health:     stringPtr("unhealthy"),
			ExitCode:   intPtr(42),
		},
	})
	cookbook.Seeds["0"] = scenario
	container, err := engine.createContainer("nginx-zero", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	api := &DockerAPI{engine: engine}
	request := httptest.NewRequest(http.MethodPost, "/v1.43/containers/nginx-zero/start", nil)
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	state := container.stateSnapshot()
	if state.Status != "exited" || state.Running || state.ExitCode != 42 {
		t.Fatalf("post-start transition was overwritten: %+v", state)
	}
}

func TestDockerRequestJSONRejectsTrailingDocument(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	api := &DockerAPI{engine: engine}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1.43/containers/create?name=nginx-zero",
		strings.NewReader(`{"Image":"nginx:alpine"} {"second":true}`),
	)
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "trailing JSON") {
		t.Fatalf("unexpected body: %s", response.Body.String())
	}
}

func TestInternalPanicIsReportedAndWrittenToLedger(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	runDir := ledger.RunDir()
	response := httptest.NewRecorder()
	func() {
		defer recoverHTTPHandler(engine, "unit-test HTTP handler", response)
		panic("deliberate test panic")
	}()
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case err := <-engine.Errors():
		if err == nil || !strings.Contains(err.Error(), "deliberate test panic") {
			t.Fatalf("unexpected internal error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("internal panic was not reported")
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(runDir, "global.ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"channel":"docker-zero.internal"`)) || !bytes.Contains(data, []byte("deliberate test panic")) {
		t.Fatalf("panic evidence missing from ledger:\n%s", data)
	}
}

func TestRedisShutdownClosesIdleClientsPromptly(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	mock := newRedisMock(engine, "127.0.0.1:0")
	if err := mock.Start(); err != nil {
		t.Fatal(err)
	}
	connection, err := net.Dial("tcp", mock.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer testClose(t, connection)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	if err := mock.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("idle Redis client delayed shutdown by %s", elapsed)
	}
}

func TestRedisShutdownCoversAcceptedButUnregisteredConnection(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	mock := newRedisMock(engine, "127.0.0.1:0")
	accepted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	mock.beforeRegister = func() {
		once.Do(func() { close(accepted) })
		<-release
	}
	if err := mock.Start(); err != nil {
		t.Fatal(err)
	}

	connection, err := net.Dial("tcp", mock.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer testClose(t, connection)
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("accept loop did not reach the pre-registration boundary")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- mock.Close(ctx) }()
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("shutdown missed a connection accepted before registration")
	}
}

func TestConcurrentNetworkInspectAndConnectIsRaceSafe(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	defer testCloseRuntimes(t, engine)
	container, err := engine.createContainer("nginx-zero", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	container.start()
	network, err := engine.createNetwork("test-network", "bridge", nil)
	if err != nil {
		t.Fatal(err)
	}
	api := &DockerAPI{engine: engine}

	const workers = 16
	const iterations = 50
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				if (worker+iteration)%2 == 0 {
					request := httptest.NewRequest(http.MethodGet, "/v1.43/networks/"+network.ID, nil)
					response := httptest.NewRecorder()
					api.ServeHTTP(response, request)
					if response.Code != http.StatusOK {
						t.Errorf("network inspect status=%d body=%s", response.Code, response.Body.String())
						return
					}
				} else {
					action := "connect"
					if iteration%3 == 0 {
						action = "disconnect"
					}
					body := strings.NewReader(`{"Container":"nginx-zero"}`)
					request := httptest.NewRequest(http.MethodPost, "/v1.43/networks/"+network.ID+"/"+action, body)
					response := httptest.NewRecorder()
					api.ServeHTTP(response, request)
					if response.Code != http.StatusOK {
						// Concurrent callers can race to attach an already-attached endpoint.
						// Docker reports this as an endpoint collision; that is expected
						// behavior, not a race-safety failure.
						if action == "connect" && response.Code == http.StatusBadRequest && strings.Contains(response.Body.String(), "already exists in network") {
							continue
						}
						t.Errorf("network %s status=%d body=%s", action, response.Code, response.Body.String())
						return
					}
				}
			}
		}(worker)
	}
	wg.Wait()
}

func TestDecodeJSONAcceptsWhitespaceOnlyAfterDocument(t *testing.T) {
	var target map[string]any
	if err := decodeJSON(strings.NewReader("{\"ok\":true}\n\t "), &target); err != nil {
		t.Fatal(err)
	}
	if target["ok"] != true {
		t.Fatalf("decoded target = %#v", target)
	}
}

func TestWriteDockerStreamFrameUsesEightByteHeader(t *testing.T) {
	var output bytes.Buffer
	writeDockerStreamFrame(&output, 1, []byte("abc"))
	if got := output.Bytes(); len(got) != 11 || got[0] != 1 || string(got[8:]) != "abc" {
		t.Fatalf("invalid Docker stream frame: %v", got)
	}
}

func TestRedisMalformedProtocolReturnsErrorWithoutEngineFailure(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	container, err := engine.createContainer("redis-zero", "redis:7-alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	container.start()
	mock := newRedisMock(engine, "127.0.0.1:0")
	if err := mock.Start(); err != nil {
		t.Fatal(err)
	}
	defer testCloseRedisMock(t, mock)
	connection, err := net.Dial("tcp", mock.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer testClose(t, connection)
	if _, err := io.WriteString(connection, "*x\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	data := make([]byte, 256)
	n, err := connection.Read(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data[:n], []byte("-ERR invalid RESP array length")) {
		t.Fatalf("unexpected Redis protocol error: %q", data[:n])
	}
	select {
	case err := <-engine.Errors():
		t.Fatalf("client protocol error was misclassified as engine failure: %v", err)
	default:
	}
}
