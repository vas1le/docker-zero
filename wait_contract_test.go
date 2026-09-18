package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The real HTTP client also verifies that headers are flushed before waiting:
// Docker SDK callers must be able to subscribe before starting a container.
func TestWaitLifecycleContract(t *testing.T) {
	for _, condition := range []string{"", "not-running", "next-exit", "removed"} {
		t.Run("condition="+condition, func(t *testing.T) {
			engine, ledger := testEngine(t, 0)
			defer testCloseLedger(t, ledger)
			c, err := engine.createContainer("waiter", "redis:alpine", nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			c.start()
			server := httptest.NewServer(&DockerAPI{engine: engine})
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/containers/waiter/wait?condition="+condition, nil)
			response, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := response.Body.Close(); err != nil {
					t.Errorf("close wait response: %v", err)
				}
			}()
			result := make(chan int, 1)
			go func() {
				var body struct{ StatusCode int }
				if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
					result <- -999
					return
				}
				result <- body.StatusCode
			}()
			select {
			case code := <-result:
				t.Fatalf("wait returned %d before its condition was met", code)
			case <-time.After(20 * time.Millisecond):
			}
			c.stop(42)
			if condition == "removed" {
				select {
				case code := <-result:
					t.Fatalf("removed wait returned %d on stop", code)
				case <-time.After(20 * time.Millisecond):
				}
				if err := engine.removeContainer(c.ID, false); err != nil {
					t.Fatal(err)
				}
			} else {
				// The result belongs to the exit, not the restarted process.
				c.start()
			}
			select {
			case code := <-result:
				if code != 42 {
					t.Fatalf("exit status = %d, want 42", code)
				}
			case <-ctx.Done():
				t.Fatal("wait did not wake up")
			}
		})
	}
}

func TestWaitNextExitOnStoppedContainer(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("next-exit", "redis:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	c.stop(7)
	server := httptest.NewServer(&DockerAPI{engine: engine})
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/containers/next-exit/wait?condition=next-exit", nil)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close wait response: %v", err)
		}
	}()
	result := make(chan int, 1)
	go func() {
		var body struct{ StatusCode int }
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			result <- -999
			return
		}
		result <- body.StatusCode
	}()
	select {
	case code := <-result:
		t.Fatalf("next-exit reused the previous exit: %d", code)
	case <-time.After(20 * time.Millisecond):
	}
	c.start()
	c.stop(23)
	select {
	case code := <-result:
		if code != 23 {
			t.Fatalf("exit = %d, want 23", code)
		}
	case <-ctx.Done():
		t.Fatal("next-exit did not wake up")
	}
}

func TestWaitRejectsInvalidCondition(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	if _, err := engine.createContainer("invalid-wait", "redis:alpine", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	(&DockerAPI{engine: engine}).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/containers/invalid-wait/wait?condition=bogus", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
}

func TestWaitSubscriptionsCancelAndWakeOnRemoval(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("wait-cancel", "redis:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	cancelled, cancel := c.subscribeWait("next-exit")
	cancel()
	waiter, cleanup := c.subscribeWait("removed")
	defer cleanup()
	if err := engine.removeContainer(c.ID, true); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-waiter.result:
		if result.StatusCode != 137 {
			t.Fatalf("forced removal exit = %d", result.StatusCode)
		}
	default:
		t.Fatal("removal did not wake waiter")
	}
	select {
	case <-cancelled.result:
		t.Fatal("cancelled waiter received an exit")
	default:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.waiters) != 0 {
		t.Fatalf("leaked %d waiters", len(c.waiters))
	}
}

// Restarting a stopped container starts a new process; it does not constitute
// the next exit for an observer subscribed after the previous process stopped.
func TestWaitNextExitIgnoresRestartOfStoppedContainer(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := engine.createContainer("next-exit-restart", "redis:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.start()
	c.stop(7)
	waiter, cancel := c.subscribeWait("next-exit")
	defer cancel()
	c.restart()
	c.start()
	select {
	case result := <-waiter.result:
		t.Fatalf("restart of a stopped container fabricated an exit: %+v", result)
	default:
	}
	c.stop(23)
	select {
	case result := <-waiter.result:
		if result.StatusCode != 23 {
			t.Fatalf("next exit = %d, want 23", result.StatusCode)
		}
	default:
		t.Fatal("waiter did not observe the new process exiting")
	}
}
