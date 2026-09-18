package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWaitBlocksUntilRequestedCondition(t *testing.T) {
	for _, condition := range []string{"", "not-running", "next-exit", "removed"} {
		t.Run(condition, func(t *testing.T) {
			engine, ledger := testEngine(t, 0)
			defer testCloseLedger(t, ledger)
			c, err := engine.createContainer("wait-test", "nginx:alpine", nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			c.start()
			if condition == "next-exit" {
				c.stop(7)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := httptest.NewRequest(http.MethodPost, "/containers/"+c.ID+"/wait?condition="+condition, nil).WithContext(ctx)
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { (&DockerAPI{engine: engine}).ServeHTTP(response, request); close(done) }()
			select {
			case <-done:
				t.Fatalf("wait returned before %q event: %s", condition, response.Body.String())
			case <-time.After(30 * time.Millisecond):
			}
			if condition == "next-exit" {
				c.start()
			}
			c.stop(42)
			if condition == "removed" {
				select {
				case <-done:
					t.Fatal("removed wait returned on stop")
				case <-time.After(30 * time.Millisecond):
				}
				if err := engine.removeContainer(c.ID, false); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("wait did not finish")
			}
			var result struct{ StatusCode int }
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if response.Code != 200 || result.StatusCode != 42 {
				t.Fatalf("wait = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestWaitRejectsInvalidCondition(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("wait-invalid", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	(&DockerAPI{engine: engine}).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/containers/"+c.ID+"/wait?condition=invalid", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid wait = %d %s", response.Code, response.Body.String())
	}
}

func TestWaitPreservesExitAcrossImmediateRestart(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("wait-fast-restart", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	result, cancel, err := c.registerWait("next-exit")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	c.stop(42)
	c.start()
	select {
	case status := <-result:
		if status.StatusCode != 42 {
			t.Fatalf("exit code = %d", status.StatusCode)
		}
	default:
		t.Fatal("exit event was lost across restart")
	}
}

func TestWaitCancellationRemovesSubscription(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("wait-cancel", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	ctx, cancel := context.WithCancel(context.Background())
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/containers/"+c.ID+"/wait", nil).WithContext(ctx)
		(&DockerAPI{engine: engine}).ServeHTTP(response, request)
		close(done)
	}()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		n := len(c.waiters)
		c.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("subscription not registered")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled wait did not return")
	}
	c.mu.Lock()
	n := len(c.waiters)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d leaked subscriptions", n)
	}
}

func TestWaitFlushesHeadersBeforeCompletion(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("wait-headers", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(&DockerAPI{engine: engine})
	defer server.Close()
	client := server.Client()
	client.Timeout = time.Second
	// The call must return headers even though next-exit has not occurred yet.
	response, err := client.Post(server.URL+"/containers/"+c.ID+"/wait?condition=next-exit", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	c.start()
	c.stop(23)
	var status containerWaitResult
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.StatusCode != 23 {
		t.Fatalf("wait status = %#v", status)
	}
}

func TestWaitRemovalNotifiesAllConditions(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("wait-remove", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	results := make([]<-chan containerWaitResult, 0, 3)
	for _, condition := range []string{"not-running", "next-exit", "removed"} {
		result, cancel, err := c.registerWait(condition)
		if err != nil {
			t.Fatal(err)
		}
		defer cancel()
		results = append(results, result)
	}
	if err := engine.removeContainer(c.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		select {
		case status := <-result:
			if status.StatusCode != 137 {
				t.Fatalf("forced removal status = %#v", status)
			}
		default:
			t.Fatal("removal did not notify waiter")
		}
	}
}
