package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type RedisMock struct {
	engine         *Engine
	address        string
	listener       net.Listener
	wg             sync.WaitGroup
	closed         chan struct{}
	errors         chan error
	connections    map[net.Conn]struct{}
	connMu         sync.Mutex
	closing        bool
	beforeRegister func() // test synchronization hook; nil in production
}

func newRedisMock(engine *Engine, address string) *RedisMock {
	return &RedisMock{
		engine: engine, address: address, closed: make(chan struct{}), errors: make(chan error, 1),
		connections: make(map[net.Conn]struct{}),
	}
}

func (r *RedisMock) Start() error {
	listener, err := net.Listen("tcp", r.address)
	if err != nil {
		return err
	}
	r.listener = listener
	r.wg.Add(1)
	go r.acceptLoop()
	return nil
}

func (r *RedisMock) acceptLoop() {
	defer r.wg.Done()
	defer close(r.errors)
	for {
		connection, err := r.listener.Accept()
		if err != nil {
			select {
			case <-r.closed:
				return
			default:
				reportRuntimeError(r.errors, err)
				return
			}
		}
		if r.beforeRegister != nil {
			r.beforeRegister()
		}
		r.connMu.Lock()
		if r.closing {
			r.connMu.Unlock()
			_ = connection.Close()
			return
		}
		r.connections[connection] = struct{}{}
		r.wg.Add(1)
		r.connMu.Unlock()
		go r.handleConnection(connection)
	}
}

func (r *RedisMock) Errors() <-chan error { return r.errors }

func (r *RedisMock) Close(ctx context.Context) error {
	r.connMu.Lock()
	if !r.closing {
		r.closing = true
		close(r.closed)
	}
	connections := make([]net.Conn, 0, len(r.connections))
	for connection := range r.connections {
		connections = append(connections, connection)
	}
	r.connMu.Unlock()

	if r.listener != nil {
		_ = r.listener.Close()
	}
	for _, connection := range connections {
		_ = connection.Close()
	}

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *RedisMock) handleConnection(connection net.Conn) {
	defer r.wg.Done()
	defer func() { _ = connection.Close() }()
	defer func() {
		r.connMu.Lock()
		delete(r.connections, connection)
		r.connMu.Unlock()
	}()
	defer recoverTCPHandler(r.engine, "redis service")
	_ = connection.SetDeadline(time.Now().Add(5 * time.Minute))
	reader := bufio.NewReader(connection)
	writer := bufio.NewWriter(connection)

	for {
		command, err := readRESPCommand(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				_, _ = writer.WriteString("-ERR " + sanitizeRedisError(err.Error()) + "\r\n")
				_ = writer.Flush()
			}
			return
		}
		if len(command) == 0 {
			continue
		}
		container, err := r.engine.findContainerByKind("redis")
		if err != nil {
			_, _ = writer.WriteString("-LOADING docker-zero redis container does not exist\r\n")
			_ = writer.Flush()
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
		r.engine.ledger.Log(LedgerEntry{
			Container: container.Name, Kind: container.Kind, Scenario: container.Seed, Channel: "service.redis", Event: event,
			Count: count, Request: command, Response: map[string]any{"reply": responseDescription, "close": response.Close},
			Transition: advance.Transition, Before: &before, After: &after,
		})
		container.appendLog(fmt.Sprintf("redis-zero: %s -> %s (request #%d)\n", strings.Join(command, " "), responseDescription, count))

		if response.Close || !state.Running || state.Restarting || state.Dead {
			return
		}
		if hasOverride && response.Reply != "" {
			_, _ = writer.WriteString(response.Reply)
		} else {
			if quit := handleBuiltInRedis(container, writer, command); quit {
				_ = writer.Flush()
				return
			}
		}
		if err := writer.Flush(); err != nil {
			return
		}
	}
}

func handleBuiltInRedis(container *Container, writer *bufio.Writer, command []string) bool {
	name := strings.ToUpper(command[0])
	switch name {
	case "PING":
		if len(command) > 1 {
			writeRESPBulk(writer, command[1])
		} else {
			_, _ = writer.WriteString("+PONG\r\n")
		}
	case "ECHO":
		if len(command) != 2 {
			_, _ = writer.WriteString("-ERR wrong number of arguments for 'echo' command\r\n")
		} else {
			writeRESPBulk(writer, command[1])
		}
	case "SET":
		if len(command) < 3 {
			_, _ = writer.WriteString("-ERR wrong number of arguments for 'set' command\r\n")
		} else {
			container.mu.Lock()
			container.KV[command[1]] = command[2]
			container.mu.Unlock()
			_, _ = writer.WriteString("+OK\r\n")
		}
	case "GET":
		if len(command) != 2 {
			_, _ = writer.WriteString("-ERR wrong number of arguments for 'get' command\r\n")
		} else {
			container.mu.Lock()
			value, ok := container.KV[command[1]]
			container.mu.Unlock()
			if !ok {
				_, _ = writer.WriteString("$-1\r\n")
			} else {
				writeRESPBulk(writer, value)
			}
		}
	case "DEL":
		deleted := 0
		container.mu.Lock()
		for _, key := range command[1:] {
			if _, ok := container.KV[key]; ok {
				delete(container.KV, key)
				deleted++
			}
		}
		container.mu.Unlock()
		_, _ = fmt.Fprintf(writer, ":%d\r\n", deleted)
	case "EXISTS":
		exists := 0
		container.mu.Lock()
		for _, key := range command[1:] {
			if _, ok := container.KV[key]; ok {
				exists++
			}
		}
		container.mu.Unlock()
		_, _ = fmt.Fprintf(writer, ":%d\r\n", exists)
	case "INCR":
		if len(command) != 2 {
			_, _ = writer.WriteString("-ERR wrong number of arguments for 'incr' command\r\n")
			break
		}
		container.mu.Lock()
		value, _ := strconv.Atoi(container.KV[command[1]])
		value++
		container.KV[command[1]] = strconv.Itoa(value)
		container.mu.Unlock()
		_, _ = fmt.Fprintf(writer, ":%d\r\n", value)
	case "INFO":
		state := container.stateSnapshot()
		body := fmt.Sprintf("# Server\r\nredis_version:7.4.0-docker-zero\r\nprocess_id:1\r\n# Replication\r\nrole:master\r\n# DockerZero\r\nseed:%d\r\nhealth:%s\r\n", container.Seed, state.Health)
		writeRESPBulk(writer, body)
	case "AUTH", "SELECT", "READONLY", "READWRITE":
		_, _ = writer.WriteString("+OK\r\n")
	case "CLIENT":
		if len(command) > 1 && strings.EqualFold(command[1], "GETNAME") {
			_, _ = writer.WriteString("$-1\r\n")
		} else {
			_, _ = writer.WriteString("+OK\r\n")
		}
	case "CONFIG":
		_, _ = writer.WriteString("*0\r\n")
	case "COMMAND":
		_, _ = writer.WriteString("*0\r\n")
	case "QUIT":
		_, _ = writer.WriteString("+OK\r\n")
		return true
	default:
		_, _ = writer.WriteString("-ERR unknown command '" + strings.ToLower(name) + "'\r\n")
	}
	return false
}

func readRESPCommand(reader *bufio.Reader) ([]string, error) {
	first, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	if first != '*' {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		return strings.Fields(strings.TrimSpace(string(first) + line)), nil
	}
	countLine, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(countLine))
	if err != nil || count < 0 || count > 1024 {
		return nil, fmt.Errorf("invalid RESP array length")
	}
	result := make([]string, 0, count)
	for i := 0; i < count; i++ {
		prefix, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if prefix != '$' {
			return nil, fmt.Errorf("expected bulk string")
		}
		lengthLine, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		length, err := strconv.Atoi(strings.TrimSpace(lengthLine))
		if err != nil || length < 0 || length > 16<<20 {
			return nil, fmt.Errorf("invalid bulk string length")
		}
		payload := make([]byte, length+2)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return nil, err
		}
		if payload[length] != '\r' || payload[length+1] != '\n' {
			return nil, fmt.Errorf("invalid bulk string terminator")
		}
		result = append(result, string(payload[:length]))
	}
	return result, nil
}

func writeRESPBulk(writer *bufio.Writer, value string) {
	_, _ = fmt.Fprintf(writer, "$%d\r\n%s\r\n", len(value), value)
}

func sanitizeRedisError(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}
