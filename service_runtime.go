package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ContainerRuntime struct {
	engine    *Engine
	container *Container

	lifecycleMu sync.Mutex
	mu          sync.Mutex
	active      bool
	listeners   []net.Listener
	servers     []*http.Server
	connections map[net.Conn]struct{}
	wg          sync.WaitGroup
}

func newContainerRuntime(engine *Engine, container *Container) *ContainerRuntime {
	return &ContainerRuntime{engine: engine, container: container, connections: make(map[net.Conn]struct{})}
}

type runtimeListenSpec struct {
	address      string
	portKey      string
	bindingIndex int
	direct       bool
}

func (r *ContainerRuntime) Start() error {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()

	r.mu.Lock()
	if r.active {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	specs := r.engine.runtimeListenSpecs(r.container)
	opened := make([]net.Listener, 0, len(specs))
	seen := make(map[string]runtimeListenSpec)
	for _, spec := range specs {
		if previous, duplicate := seen[spec.address]; duplicate && !strings.HasSuffix(spec.address, ":0") {
			if previous.direct == spec.direct && previous.portKey == spec.portKey {
				continue
			}
			for _, item := range opened {
				_ = item.Close()
			}
			r.container.resetEphemeralAssignedPorts()
			return fmt.Errorf("driver failed programming external connectivity on endpoint %s: Bind for %s failed: port is already allocated", r.container.Name, spec.address)
		}
		seen[spec.address] = spec

		listener, err := net.Listen("tcp", spec.address)
		if err != nil {
			if spec.direct && r.engine.endpointMode == "auto" {
				r.engine.ledger.Log(LedgerEntry{
					Container: r.container.Name, Kind: r.container.Kind, Scenario: r.container.Seed,
					Channel: "docker-zero.network", Event: "direct_endpoint.skipped",
					Request: map[string]any{"address": spec.address}, Response: map[string]any{"error": err.Error()},
				})
				continue
			}
			for _, item := range opened {
				_ = item.Close()
			}
			r.container.resetEphemeralAssignedPorts()
			if spec.direct {
				return fmt.Errorf("cannot bind virtual container endpoint %s: %w", spec.address, err)
			}
			return fmt.Errorf("driver failed programming external connectivity on endpoint %s: Bind for %s failed: port is already allocated: %w", r.container.Name, spec.address, err)
		}
		opened = append(opened, listener)
		if spec.bindingIndex >= 0 {
			_, portText, splitErr := net.SplitHostPort(listener.Addr().String())
			if splitErr == nil {
				if port, convErr := strconv.Atoi(portText); convErr == nil {
					r.container.updateAssignedPort(spec.portKey, spec.bindingIndex, port)
				}
			}
		}
	}

	servers := make([]*http.Server, 0, len(opened))
	if r.container.Kind != "nginx" && r.container.Kind != "redis" {
		for _, item := range opened {
			_ = item.Close()
		}
		r.container.resetEphemeralAssignedPorts()
		return fmt.Errorf("unsupported service runtime kind %q", r.container.Kind)
	}
	if r.container.Kind == "nginx" {
		for range opened {
			servers = append(servers, &http.Server{
				Handler:           &NginxContainerHandler{engine: r.engine, container: r.container},
				ReadHeaderTimeout: 5 * time.Second,
				IdleTimeout:       30 * time.Second,
			})
		}
	}

	r.mu.Lock()
	r.listeners = opened
	r.servers = servers
	r.active = true
	r.mu.Unlock()

	for index, listener := range opened {
		switch r.container.Kind {
		case "nginx":
			server := servers[index]
			r.wg.Add(1)
			go func(server *http.Server, listener net.Listener) {
				defer r.wg.Done()
				if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
					reportRuntimeError(r.engine.internalErrors, fmt.Errorf("container %s HTTP endpoint %s: %w", r.container.Name, listener.Addr(), err))
				}
			}(server, listener)
		case "redis":
			r.wg.Add(1)
			go func(listener net.Listener) {
				defer r.wg.Done()
				r.serveRedisListener(listener)
			}(listener)
		}
	}
	return nil
}

func (r *ContainerRuntime) Stop(ctx context.Context) error {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()

	r.mu.Lock()
	if !r.active && len(r.listeners) == 0 && len(r.connections) == 0 {
		r.mu.Unlock()
		return nil
	}
	listeners := append([]net.Listener(nil), r.listeners...)
	servers := append([]*http.Server(nil), r.servers...)
	connections := make([]net.Conn, 0, len(r.connections))
	for connection := range r.connections {
		connections = append(connections, connection)
	}
	r.listeners = nil
	r.servers = nil
	r.active = false
	r.mu.Unlock()

	for _, listener := range listeners {
		_ = listener.Close()
	}
	for _, connection := range connections {
		_ = connection.Close()
	}
	for _, server := range servers {
		_ = server.Shutdown(ctx)
	}

	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *ContainerRuntime) Active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

func (r *ContainerRuntime) registerRedisConnection(connection net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active {
		_ = connection.Close()
		return false
	}
	r.connections[connection] = struct{}{}
	r.wg.Add(1)
	return true
}

func (r *ContainerRuntime) finishRedisConnection(connection net.Conn) {
	r.mu.Lock()
	delete(r.connections, connection)
	r.mu.Unlock()
	r.wg.Done()
}

func (r *ContainerRuntime) serveRedisListener(listener net.Listener) {
	defer recoverTCPHandler(r.engine, "redis container service")
	for {
		connection, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			reportRuntimeError(r.engine.internalErrors, fmt.Errorf("container %s Redis accept %s: %w", r.container.Name, listener.Addr(), err))
			return
		}
		if !r.registerRedisConnection(connection) {
			continue
		}
		go func(connection net.Conn) {
			defer r.finishRedisConnection(connection)
			handleRedisContainerConnection(r.engine, r.container, connection)
		}(connection)
	}
}

type NginxContainerHandler struct {
	engine    *Engine
	container *Container
}

func (n *NginxContainerHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	defer recoverHTTPHandler(n.engine, "nginx container service", w)
	container := n.container

	method := req.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	event := fmt.Sprintf("http.%s %s", method, req.URL.Path)
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
	before := advance.Before
	after := state
	n.engine.ledger.Log(LedgerEntry{
		Container: container.Name, Kind: container.Kind, Scenario: container.Seed, Channel: "service.http", Event: event,
		Count:      count,
		Request:    map[string]any{"method": req.Method, "path": req.URL.RequestURI(), "remote": req.RemoteAddr},
		Response:   map[string]any{"status": response.Status, "close": response.Close, "bytes": len(response.Body)},
		Transition: advance.Transition, Before: &before, After: &after,
	})
	container.appendLog(fmt.Sprintf("nginx-zero[%s]: %s %s -> %d (request #%d)\n", container.Name, req.Method, req.URL.Path, response.Status, count))

	if response.Close || !state.Running || state.Paused || state.Restarting || state.Dead {
		go n.engine.syncRuntimeForState(container)
		closeHTTPConnection(w)
		return
	}
	w.Header().Set("Server", "nginx/1.27.5 (docker-zero)")
	for key, value := range response.Headers {
		w.Header().Set(key, value)
	}
	w.Header().Set("X-Docker-Zero-Container", container.Name)
	w.Header().Set("X-Docker-Zero-Seed", fmt.Sprintf("%d", container.Seed))
	w.Header().Set("X-Docker-Zero-Request", fmt.Sprintf("%d", count))
	w.WriteHeader(response.Status)
	if req.Method != http.MethodHead {
		_, _ = w.Write([]byte(response.Body))
	}
}

func handleRedisContainerConnection(engine *Engine, container *Container, connection net.Conn) {
	defer connection.Close()
	defer recoverTCPHandler(engine, "redis container connection")
	_ = connection.SetDeadline(time.Now().Add(5 * time.Minute))
	reader := bufio.NewReader(connection)
	writer := bufio.NewWriter(connection)
	for {
		command, err := readRESPCommand(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				_, _ = writer.WriteString("-ERR " + sanitizeRedisError(err.Error()) + "\r\n")
				_ = writer.Flush()
			}
			return
		}
		if len(command) == 0 {
			continue
		}
		name := strings.ToUpper(command[0])
		event := "redis." + name
		advance := container.advance(event)
		count := advance.Count
		response, hasOverride := container.redisResponse(name, count)
		if response.DelayMS > 0 {
			time.Sleep(time.Duration(response.DelayMS) * time.Millisecond)
		}
		if response.StatePatch != (StatePatch{}) {
			_, after := container.applyPatch(response.StatePatch)
			advance.After = after
		}
		state := container.stateSnapshot()

		responseDescription := "built-in"
		if hasOverride {
			responseDescription = strings.TrimSpace(response.Reply)
			if len(responseDescription) > 100 {
				responseDescription = responseDescription[:100]
			}
		}
		before := advance.Before
		after := state
		engine.ledger.Log(LedgerEntry{
			Container: container.Name, Kind: container.Kind, Scenario: container.Seed, Channel: "service.redis", Event: event,
			Count: count, Request: command, Response: map[string]any{"reply": responseDescription, "close": response.Close},
			Transition: advance.Transition, Before: &before, After: &after,
		})
		container.appendLog(fmt.Sprintf("redis-zero[%s]: %s -> %s (request #%d)\n", container.Name, strings.Join(command, " "), responseDescription, count))

		if response.Close || !state.Running || state.Paused || state.Restarting || state.Dead {
			go engine.syncRuntimeForState(container)
			return
		}
		if hasOverride && response.Reply != "" {
			_, _ = writer.WriteString(response.Reply)
		} else if quit := handleBuiltInRedisForEngine(engine, container, writer, command); quit {
			_ = writer.Flush()
			return
		}
		if err := writer.Flush(); err != nil {
			return
		}
	}
}

func (e *Engine) runtimeListenSpecs(c *Container) []runtimeListenSpec {
	specs := make([]runtimeListenSpec, 0)
	if e.endpointMode != "off" {
		for _, item := range e.containerEndpointAddresses(c) {
			specs = append(specs, runtimeListenSpec{address: item, bindingIndex: -1, direct: true})
		}
	}

	bindings := c.clonePortBindings()
	keys := make([]string, 0, len(bindings))
	for key := range bindings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		items := bindings[key]
		for index, binding := range items {
			if binding.Protocol != "" && binding.Protocol != "tcp" {
				continue
			}
			host := normalizePublishedListenIP(binding.HostIP)
			port := binding.HostPort
			if port == 0 {
				port = binding.RequestedHostPort
			}
			specs = append(specs, runtimeListenSpec{
				address: net.JoinHostPort(host, strconv.Itoa(port)), portKey: key, bindingIndex: index, direct: false,
			})
		}
	}
	return specs
}

func normalizePublishedListenIP(host string) string {
	host = strings.TrimSpace(host)
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	default:
		return host
	}
}

var errEngineRuntimeClosing = errors.New("docker-zero engine runtime is shutting down")

func (e *Engine) startContainerRuntime(c *Container) error {
	e.runtimeGate.RLock()
	defer e.runtimeGate.RUnlock()
	if e.runtimeClosing {
		return errEngineRuntimeClosing
	}

	e.mu.Lock()
	runtime := e.runtimes[c.ID]
	if runtime == nil {
		runtime = newContainerRuntime(e, c)
		e.runtimes[c.ID] = runtime
	}
	e.mu.Unlock()

	if err := runtime.Start(); err != nil {
		return err
	}
	if c.Kind == "redis" {
		e.refreshRedisReplication(c)
		e.refreshRedisDependents(c)
	}
	return nil
}

func (e *Engine) stopContainerRuntime(c *Container) error {
	e.runtimeGate.RLock()
	defer e.runtimeGate.RUnlock()

	e.mu.RLock()
	runtime := e.runtimes[c.ID]
	e.mu.RUnlock()
	if runtime == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := runtime.Stop(ctx)
	if c.Kind == "redis" {
		e.markRedisMasterDown(c)
	}
	return err
}

func (e *Engine) syncRuntimeForState(c *Container) {
	state := c.stateSnapshot()
	if state.Running && !state.Restarting && !state.Dead && (state.Status == "running" || state.Status == "paused") {
		if err := e.startContainerRuntime(c); err != nil {
			if errors.Is(err, errEngineRuntimeClosing) {
				return
			}
			before, after := c.applyPatch(StatePatch{
				Status: stringPtr("exited"), Running: boolPtr(false), Restarting: boolPtr(false),
				Health: stringPtr("unhealthy"), ExitCode: intPtr(128), Error: stringPtr(err.Error()),
			})
			e.ledger.Log(LedgerEntry{Container: c.Name, Kind: c.Kind, Scenario: c.Seed, Channel: "docker-zero.runtime", Event: "bind.failed", Response: map[string]any{"error": err.Error()}, Before: &before, After: &after})
		}
		return
	}
	_ = e.stopContainerRuntime(c)
}

func (e *Engine) closeRuntimes(ctx context.Context) error {
	e.runtimeGate.Lock()
	defer e.runtimeGate.Unlock()
	e.runtimeClosing = true

	e.mu.RLock()
	runtimes := make([]*ContainerRuntime, 0, len(e.runtimes))
	for _, runtime := range e.runtimes {
		runtimes = append(runtimes, runtime)
	}
	e.mu.RUnlock()
	var first error
	for _, runtime := range runtimes {
		if err := runtime.Stop(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}
