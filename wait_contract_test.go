package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWaitRetainsExitAcrossImmediateRestart(t *testing.T) {
	for _, condition := range []string{"not-running", "next-exit"} {
		t.Run(condition, func(t *testing.T) {
			engine, ledger := testEngine(t, 0)
			defer testCloseLedger(t, ledger)
			c, err := engine.createContainer("wait-test", "nginx", nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			c.start()
			result, cancel := c.registerWait(condition)
			defer cancel()
			select {
			case state := <-result:
				t.Fatalf("completed while running: %+v", state)
			default:
			}
			c.stop(42)
			c.start()
			select {
			case state := <-result:
				if state.ExitCode != 42 || state.Running {
					t.Fatalf("lost exit: %+v", state)
				}
			case <-time.After(time.Second):
				t.Fatal("wait did not receive exit")
			}
		})
	}
}

func TestWaitStoppedNextExitAndRemoval(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("wait-stopped", "nginx", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	immediate, cancel := c.registerWait("not-running")
	defer cancel()
	select {
	case <-immediate:
	default:
		t.Fatal("created container should satisfy not-running")
	}
	next, cancelNext := c.registerWait("next-exit")
	defer cancelNext()
	removed, cancelRemoved := c.registerWait("removed")
	defer cancelRemoved()
	select {
	case <-next:
		t.Fatal("next-exit completed without an exit")
	default:
	}
	c.start()
	c.stop(7)
	select {
	case state := <-next:
		if state.ExitCode != 7 {
			t.Fatal(state)
		}
	default:
		t.Fatal("missing exit")
	}
	select {
	case <-removed:
		t.Fatal("stop satisfied removal")
	default:
	}
	if err := engine.removeContainer(c.ID, false); err != nil {
		t.Fatal(err)
	}
	select {
	case state := <-removed:
		if state.ExitCode != 7 {
			t.Fatal(state)
		}
	default:
		t.Fatal("removal did not wake waiter")
	}
}

func TestWaitForceRemovalAndCancellation(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("wait-remove", "nginx", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	for i := 0; i < 100; i++ {
		_, cancel := c.registerWait("next-exit")
		cancel()
	}
	c.mu.Lock()
	count := len(c.waiters)
	c.mu.Unlock()
	if count != 0 {
		t.Fatalf("cancelled waiters retained: %d", count)
	}
	var results []<-chan ContainerStateSnapshot
	for _, condition := range []string{"not-running", "next-exit", "removed"} {
		result, cancel := c.registerWait(condition)
		defer cancel()
		results = append(results, result)
	}
	if err := engine.removeContainer(c.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		select {
		case state := <-result:
			if state.ExitCode != 137 || state.Running {
				t.Fatal(state)
			}
		default:
			t.Fatal("removal missed waiter")
		}
	}
}

func TestWaitHTTPHeadersPrecedeExitAndBodyContainsActualExit(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("wait-http", "nginx", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	server := httptest.NewServer(&DockerAPI{engine: engine})
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/containers/"+c.ID+"/wait?condition=next-exit", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("wait did not flush registration headers: %v", err)
	}
	defer testClose(t, response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatal(response.Status)
	}
	c.stop(42)
	var body struct {
		StatusCode int
		Error      any
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.StatusCode != 42 || body.Error != nil {
		t.Fatalf("wait body: %+v", body)
	}
}

func TestWaitHTTPInvalidConditionAndClientCancellation(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("wait-cancel", "nginx", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	api := &DockerAPI{engine: engine}
	response := httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/containers/"+c.ID+"/wait?condition=invalid", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid condition returned %d", response.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	api.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/containers/"+c.ID+"/wait", nil).WithContext(ctx))
	c.mu.Lock()
	count := len(c.waiters)
	c.mu.Unlock()
	if count != 0 {
		t.Fatalf("disconnected client retained %d waiters", count)
	}
}

func TestWaitHTTPDisconnectUnregistersPendingWait(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("wait-disconnect", "nginx", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	server := httptest.NewServer(&DockerAPI{engine: engine})
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/containers/"+c.ID+"/wait", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer testClose(t, res.Body)
	cancel()
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		count := len(c.waiters)
		c.mu.Unlock()
		if count == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("disconnected client retained %d waiters", count)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWaitRemovalOfNeverStartedContainer(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("wait-created-removal", "nginx", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	next, cancelNext := c.registerWait("next-exit")
	defer cancelNext()
	removed, cancelRemoved := c.registerWait("removed")
	defer cancelRemoved()
	if err := engine.removeContainer(c.ID, false); err != nil {
		t.Fatal(err)
	}
	for _, result := range []<-chan ContainerStateSnapshot{next, removed} {
		select {
		case <-result:
		default:
			t.Fatal("waiter missed removal")
		}
	}
	afterRemoval, cancel := c.registerWait("removed")
	defer cancel()
	select {
	case <-afterRemoval:
	default:
		t.Fatal("removal raced registration")
	}
}
