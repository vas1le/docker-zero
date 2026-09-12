package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

type NginxMock struct {
	engine   *Engine
	address  string
	listener net.Listener
	server   *http.Server
	errors   chan error
}

func newNginxMock(engine *Engine, address string) *NginxMock {
	return &NginxMock{engine: engine, address: address, errors: make(chan error, 1)}
}

func (n *NginxMock) Start() error {
	listener, err := net.Listen("tcp", n.address)
	if err != nil {
		return err
	}
	n.listener = listener
	n.server = &http.Server{
		Handler:           n,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	go func() {
		defer close(n.errors)
		if err := n.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			reportRuntimeError(n.errors, err)
		}
	}()
	return nil
}

func (n *NginxMock) Errors() <-chan error { return n.errors }

func (n *NginxMock) Close(ctx context.Context) error {
	if n.server == nil {
		return nil
	}
	return n.server.Shutdown(ctx)
}

func (n *NginxMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer recoverHTTPHandler(n.engine, "nginx service", w)
	container, err := n.engine.findContainerByKind("nginx")
	if err != nil {
		w.Header().Set("Server", "nginx/docker-zero")
		http.Error(w, "docker-zero: nginx container does not exist", http.StatusServiceUnavailable)
		return
	}

	method := r.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	event := fmt.Sprintf("http.%s %s", method, r.URL.Path)
	advance := container.advance(event)
	count := advance.Count

	response, ok := container.routeResponse(strings.TrimPrefix(event, "http."), count)
	if !ok {
		response = HTTPResponse{From: 1, Status: http.StatusNotFound, Headers: map[string]string{"Content-Type": "text/plain"}, Body: "404 Not Found\n"}
	}
	if response.Status == 0 {
		response.Status = http.StatusOK
	}
	if response.DelayMS > 0 {
		time.Sleep(time.Duration(response.DelayMS) * time.Millisecond)
	}
	if response.StatePatch != (StatePatch{}) {
		_, after := container.applyPatch(response.StatePatch)
		advance.After = after
	}

	state := container.stateSnapshot()
	requestDoc := map[string]any{"method": r.Method, "path": r.URL.RequestURI(), "remote": r.RemoteAddr}
	responseDoc := map[string]any{"status": response.Status, "close": response.Close, "bytes": len(response.Body)}
	before := advance.Before
	after := state
	n.engine.ledger.Log(LedgerEntry{
		Container: container.Name, Kind: container.Kind, Scenario: container.Seed, Channel: "service.http", Event: event,
		Count: count, Request: requestDoc, Response: responseDoc, Transition: advance.Transition,
		Before: &before, After: &after,
	})
	container.appendLog(fmt.Sprintf("nginx-zero: %s %s -> %d (request #%d)\n", r.Method, r.URL.Path, response.Status, count))

	if response.Close || !state.Running || state.Restarting || state.Dead {
		closeHTTPConnection(w)
		return
	}
	w.Header().Set("Server", "nginx/1.27.5 (docker-zero)")
	for key, value := range response.Headers {
		w.Header().Set(key, value)
	}
	w.Header().Set("X-Docker-Zero-Seed", fmt.Sprintf("%d", container.Seed))
	w.Header().Set("X-Docker-Zero-Request", fmt.Sprintf("%d", count))
	w.WriteHeader(response.Status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(response.Body))
	}
}

func closeHTTPConnection(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	connection, _, err := hijacker.Hijack()
	if err == nil {
		_ = connection.Close()
	}
}
