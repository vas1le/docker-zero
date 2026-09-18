package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func contractAPI(t *testing.T, seed int) (*DockerAPI, *Container) {
	t.Helper()
	engine, ledger := testEngine(t, seed)
	engine.endpointMode = "off"
	t.Cleanup(func() { testCloseRuntimes(t, engine); testCloseLedger(t, ledger) })
	container, err := engine.createContainer("contract", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &DockerAPI{engine: engine}, container
}

func contractRequest(api *DockerAPI, method, path, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	api.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
	return recorder
}

// waitRequest returns after response headers arrive, just as the Docker SDK's
// ContainerWait does. Reading the body must still block until the lifecycle event.
func waitRequest(t *testing.T, server *httptest.Server, condition string) (<-chan []byte, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/containers/contract/wait?condition="+condition, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		cancel()
		t.Fatalf("wait must send headers before the exit: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		cancel()
		t.Fatalf("wait status = %d", response.StatusCode)
	}
	body := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		body <- data
	}()
	return body, cancel
}

func assertWaitPending(t *testing.T, body <-chan []byte) {
	t.Helper()
	select {
	case data := <-body:
		t.Fatalf("wait completed before its event: %s", data)
	case <-time.After(15 * time.Millisecond):
	}
}

func assertWaitExit(t *testing.T, body <-chan []byte, want int) {
	t.Helper()
	select {
	case data := <-body:
		var result struct{ StatusCode int }
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatalf("invalid wait response %q: %v", data, err)
		}
		if result.StatusCode != want {
			t.Fatalf("wait exit code = %d, want %d", result.StatusCode, want)
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not complete after its event")
	}
}

func TestWaitBlocksUntilExitAndPreservesExitCode(t *testing.T) {
	for _, condition := range []string{"", "not-running", "next-exit"} {
		t.Run(condition, func(t *testing.T) {
			api, container := contractAPI(t, 0)
			container.start()
			server := httptest.NewServer(api)
			defer server.Close()
			body, cancel := waitRequest(t, server, condition)
			defer cancel()
			assertWaitPending(t, body)
			container.stop(42)
			container.start() // A fast restart must not erase the captured exit.
			assertWaitExit(t, body, 42)
		})
	}
}

func TestWaitNextExitDoesNotUsePreviousExit(t *testing.T) {
	api, container := contractAPI(t, 0)
	container.start()
	container.stop(7)
	server := httptest.NewServer(api)
	defer server.Close()
	body, cancel := waitRequest(t, server, "next-exit")
	defer cancel()
	assertWaitPending(t, body)
	container.start()
	assertWaitPending(t, body)
	container.stop(19)
	assertWaitExit(t, body, 19)
}

func TestWaitNotRunningReturnsExistingExit(t *testing.T) {
	api, container := contractAPI(t, 0)
	container.start()
	container.stop(13)
	response := contractRequest(api, http.MethodPost, "/containers/contract/wait", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"StatusCode":13`) {
		t.Fatalf("existing exit response: %d %s", response.Code, response.Body.String())
	}
}

func TestWaitRemovedAndForceRemoval(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(map[bool]string{false: "created", true: "running"}[running], func(t *testing.T) {
			api, container := contractAPI(t, 0)
			if running {
				container.start()
			}
			server := httptest.NewServer(api)
			defer server.Close()
			body, cancel := waitRequest(t, server, "removed")
			defer cancel()
			assertWaitPending(t, body)
			if err := api.engine.removeContainer(container.ID, true); err != nil {
				t.Fatal(err)
			}
			want := 0
			if running {
				want = 137
			}
			assertWaitExit(t, body, want)
		})
	}
}

func TestWaitRemovedDoesNotCompleteOnStop(t *testing.T) {
	api, container := contractAPI(t, 0)
	container.start()
	server := httptest.NewServer(api)
	defer server.Close()
	body, cancel := waitRequest(t, server, "removed")
	defer cancel()
	container.stop(9)
	assertWaitPending(t, body)
	if err := api.engine.removeContainer(container.ID, false); err != nil {
		t.Fatal(err)
	}
	assertWaitExit(t, body, 9)
}

func TestWaitCancellationAndInvalidCondition(t *testing.T) {
	api, container := contractAPI(t, 0)
	container.start()
	response := contractRequest(api, http.MethodPost, "/containers/contract/wait?condition=bogus", "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid condition status = %d, want 400", response.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/containers/contract/wait", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		api.ServeHTTP(httptest.NewRecorder(), request)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled wait handler leaked")
	}
	if state := container.stateSnapshot(); !state.Running {
		t.Fatalf("cancellation changed lifecycle: %+v", state)
	}
}

func TestWaitSubscriptionsAreReleased(t *testing.T) {
	_, container := contractAPI(t, 0)
	container.start()
	for i := 0; i < 100; i++ {
		_, cancel := container.registerWait("next-exit")
		cancel()
		cancel() // Cleanup must be safe after either cancellation or completion.
	}
	container.mu.Lock()
	count := len(container.waiters)
	container.mu.Unlock()
	if count != 0 {
		t.Fatalf("cancelled waiters retained: %d", count)
	}
	first, cancelFirst := container.registerWait("not-running")
	defer cancelFirst()
	second, cancelSecond := container.registerWait("next-exit")
	defer cancelSecond()
	container.stop(23)
	for _, result := range []<-chan containerWaitResult{first, second} {
		select {
		case status := <-result:
			if status.StatusCode != 23 {
				t.Fatalf("exit code = %d", status.StatusCode)
			}
		default:
			t.Fatal("concurrent waiter was not notified")
		}
	}
	container.mu.Lock()
	count = len(container.waiters)
	container.mu.Unlock()
	if count != 0 {
		t.Fatalf("completed waiters retained: %d", count)
	}
}

func TestWaitNextExitReleasedByRemovingCreatedContainer(t *testing.T) {
	api, container := contractAPI(t, 0)
	result, cancel := container.registerWait("next-exit")
	defer cancel()
	if err := api.engine.removeContainer(container.ID, false); err != nil {
		t.Fatal(err)
	}
	select {
	case status := <-result:
		if status.StatusCode != 0 {
			t.Fatalf("exit code = %d", status.StatusCode)
		}
	default:
		t.Fatal("removal did not release next-exit waiter")
	}
}
