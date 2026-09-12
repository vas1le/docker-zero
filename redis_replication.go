package main

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

func handleBuiltInRedisForEngine(engine *Engine, container *Container, writer *bufio.Writer, command []string) bool {
	name := strings.ToUpper(command[0])
	if isRedisWriteCommand(name) && containerRedisRole(container) == "slave" {
		_, _ = writer.WriteString("-READONLY You can't write against a read only replica.\r\n")
		return false
	}

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
			engine.replicateRedisMutation(container, command)
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
		engine.replicateRedisMutation(container, command)
		_, _ = writer.WriteString(fmt.Sprintf(":%d\r\n", deleted))
	case "EXISTS":
		exists := 0
		container.mu.Lock()
		for _, key := range command[1:] {
			if _, ok := container.KV[key]; ok {
				exists++
			}
		}
		container.mu.Unlock()
		_, _ = writer.WriteString(fmt.Sprintf(":%d\r\n", exists))
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
		engine.replicateRedisMutation(container, []string{"SET", command[1], strconv.Itoa(value)})
		_, _ = writer.WriteString(fmt.Sprintf(":%d\r\n", value))
	case "INFO":
		writeRESPBulk(writer, engine.redisInfo(container))
	case "ROLE":
		container.mu.Lock()
		role := container.Redis.Role
		masterHost := container.Redis.MasterHost
		masterPort := container.Redis.MasterPort
		link := container.Redis.MasterLinkStatus
		container.mu.Unlock()
		if role == "slave" {
			state := "connected"
			if link != "up" {
				state = "connect"
			}
			_, _ = writer.WriteString(fmt.Sprintf("*5\r\n$5\r\nslave\r\n$%d\r\n%s\r\n:%d\r\n$%d\r\n%s\r\n:0\r\n", len(masterHost), masterHost, masterPort, len(state), state))
		} else {
			_, _ = writer.WriteString("*3\r\n$6\r\nmaster\r\n:0\r\n*0\r\n")
		}
	case "REPLICAOF", "SLAVEOF":
		if len(command) != 3 {
			_, _ = writer.WriteString("-ERR wrong number of arguments for 'replicaof' command\r\n")
			break
		}
		if strings.EqualFold(command[1], "NO") && strings.EqualFold(command[2], "ONE") {
			engine.setRedisMaster(container)
			_, _ = writer.WriteString("+OK\r\n")
			break
		}
		port, err := strconv.Atoi(command[2])
		if err != nil || port < 1 || port > 65535 {
			_, _ = writer.WriteString("-ERR Invalid master port\r\n")
			break
		}
		engine.setRedisReplica(container, command[1], port)
		_, _ = writer.WriteString("+OK\r\n")
	case "AUTH", "SELECT", "READONLY", "READWRITE":
		_, _ = writer.WriteString("+OK\r\n")
	case "CLIENT":
		if len(command) > 1 && strings.EqualFold(command[1], "GETNAME") {
			_, _ = writer.WriteString("$-1\r\n")
		} else {
			_, _ = writer.WriteString("+OK\r\n")
		}
	case "CONFIG", "COMMAND":
		_, _ = writer.WriteString("*0\r\n")
	case "QUIT":
		_, _ = writer.WriteString("+OK\r\n")
		return true
	default:
		_, _ = writer.WriteString("-ERR unknown command '" + strings.ToLower(name) + "'\r\n")
	}
	return false
}

func isRedisWriteCommand(name string) bool {
	switch strings.ToUpper(name) {
	case "SET", "DEL", "INCR":
		return true
	default:
		return false
	}
}

func containerRedisRole(c *Container) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Redis.Role == "" {
		return "master"
	}
	return c.Redis.Role
}

func (e *Engine) setRedisMaster(c *Container) {
	c.mu.Lock()
	c.Redis = RedisReplicationState{Role: "master", MasterLinkStatus: "up"}
	c.mu.Unlock()
}

func (e *Engine) setRedisReplica(c *Container, host string, port int) {
	c.mu.Lock()
	c.Redis.Role = "slave"
	c.Redis.MasterHost = host
	c.Redis.MasterPort = port
	c.Redis.MasterID = ""
	c.Redis.MasterLinkStatus = "down"
	c.mu.Unlock()
	e.refreshRedisReplication(c)
}

func (e *Engine) refreshRedisReplication(c *Container) {
	if c == nil || c.Kind != "redis" {
		return
	}
	if host, port, ok := redisReplicaTargetFromCommand(c.Command); ok {
		c.mu.Lock()
		c.Redis.Role = "slave"
		c.Redis.MasterHost = host
		c.Redis.MasterPort = port
		if c.Redis.MasterLinkStatus == "" {
			c.Redis.MasterLinkStatus = "down"
		}
		c.mu.Unlock()
	}

	c.mu.Lock()
	role := c.Redis.Role
	host := c.Redis.MasterHost
	port := c.Redis.MasterPort
	c.mu.Unlock()
	if role != "slave" || host == "" || port == 0 {
		return
	}

	var master *Container
	if ip := netParseIPText(host); ip != "" {
		master = e.findContainerByEndpointIP(ip)
	} else {
		results := e.resolveDNS(c, host)
		for _, result := range results {
			candidate, err := e.findContainer(result.Container)
			if err == nil && candidate.Kind == "redis" && candidate.ID != c.ID {
				master = candidate
				break
			}
		}
	}

	if master == nil || !master.isRunning() {
		c.mu.Lock()
		c.Redis.MasterID = ""
		c.Redis.MasterLinkStatus = "down"
		c.mu.Unlock()
		return
	}

	master.mu.Lock()
	masterKV := make(map[string]string, len(master.KV))
	for key, value := range master.KV {
		masterKV[key] = value
	}
	master.mu.Unlock()

	c.mu.Lock()
	c.Redis.MasterID = master.ID
	c.Redis.MasterLinkStatus = "up"
	c.KV = masterKV
	c.mu.Unlock()
	e.ledger.Log(LedgerEntry{Container: c.Name, Kind: c.Kind, Scenario: c.Seed, Channel: "service.redis.replication", Event: "link.up", Request: map[string]any{"master_host": host, "master_port": port}, Response: map[string]any{"master": master.Name}})
}

func redisReplicaTargetFromCommand(command []string) (string, int, bool) {
	tokens := make([]string, 0)
	for _, part := range command {
		tokens = append(tokens, strings.Fields(part)...)
	}
	for i := 0; i+2 < len(tokens); i++ {
		if strings.EqualFold(tokens[i], "--replicaof") || strings.EqualFold(tokens[i], "--slaveof") {
			port, err := strconv.Atoi(tokens[i+2])
			if err == nil && port > 0 && port <= 65535 {
				return tokens[i+1], port, true
			}
		}
	}
	return "", 0, false
}

func netParseIPText(host string) string {
	host = strings.TrimSpace(host)
	parts := strings.Split(host, ".")
	if len(parts) == 4 {
		for _, p := range parts {
			if _, err := strconv.Atoi(p); err != nil {
				return ""
			}
		}
		return host
	}
	return ""
}

func (e *Engine) refreshRedisDependents(master *Container) {
	if master == nil || master.Kind != "redis" {
		return
	}
	e.mu.RLock()
	items := make([]*Container, 0)
	for _, c := range e.containers {
		if c.Kind == "redis" && c.ID != master.ID {
			items = append(items, c)
		}
	}
	e.mu.RUnlock()
	for _, c := range items {
		e.refreshRedisReplication(c)
	}
}

func (e *Engine) markRedisMasterDown(master *Container) {
	if master == nil || master.Kind != "redis" {
		return
	}
	e.mu.RLock()
	items := make([]*Container, 0)
	for _, c := range e.containers {
		if c.Kind == "redis" && c.ID != master.ID {
			items = append(items, c)
		}
	}
	e.mu.RUnlock()
	for _, c := range items {
		c.mu.Lock()
		if c.Redis.MasterID == master.ID {
			c.Redis.MasterLinkStatus = "down"
		}
		c.mu.Unlock()
	}
}

func (e *Engine) replicateRedisMutation(master *Container, command []string) {
	if master == nil || master.Kind != "redis" || containerRedisRole(master) != "master" {
		return
	}
	e.mu.RLock()
	replicas := make([]*Container, 0)
	for _, c := range e.containers {
		if c.Kind != "redis" || c.ID == master.ID {
			continue
		}
		c.mu.Lock()
		linked := c.Redis.Role == "slave" && c.Redis.MasterID == master.ID && c.Redis.MasterLinkStatus == "up"
		c.mu.Unlock()
		if linked && c.isRunning() {
			replicas = append(replicas, c)
		}
	}
	e.mu.RUnlock()
	for _, replica := range replicas {
		applyRedisMutation(replica, command)
		e.ledger.Log(LedgerEntry{Container: replica.Name, Kind: replica.Kind, Scenario: replica.Seed, Channel: "service.redis.replication", Event: "apply", Request: command, Response: map[string]any{"master": master.Name}})
	}
}

func applyRedisMutation(c *Container, command []string) {
	if len(command) == 0 {
		return
	}
	name := strings.ToUpper(command[0])
	c.mu.Lock()
	defer c.mu.Unlock()
	switch name {
	case "SET":
		if len(command) >= 3 {
			c.KV[command[1]] = command[2]
		}
	case "DEL":
		for _, key := range command[1:] {
			delete(c.KV, key)
		}
	case "INCR":
		if len(command) == 2 {
			value, _ := strconv.Atoi(c.KV[command[1]])
			c.KV[command[1]] = strconv.Itoa(value + 1)
		}
	}
}

func (e *Engine) redisInfo(c *Container) string {
	c.mu.Lock()
	role := c.Redis.Role
	masterHost := c.Redis.MasterHost
	masterPort := c.Redis.MasterPort
	link := c.Redis.MasterLinkStatus
	seed := c.Seed
	health := c.Health.Status
	c.mu.Unlock()
	if role == "" {
		role = "master"
	}
	if role == "slave" {
		return fmt.Sprintf("# Server\r\nredis_version:7.4.0-docker-zero\r\nprocess_id:1\r\n# Replication\r\nrole:slave\r\nmaster_host:%s\r\nmaster_port:%d\r\nmaster_link_status:%s\r\n# DockerZero\r\nseed:%d\r\nhealth:%s\r\n", masterHost, masterPort, link, seed, health)
	}

	connected := 0
	e.mu.RLock()
	for _, other := range e.containers {
		if other.Kind != "redis" || other.ID == c.ID {
			continue
		}
		other.mu.Lock()
		if other.Redis.Role == "slave" && other.Redis.MasterID == c.ID && other.Redis.MasterLinkStatus == "up" {
			connected++
		}
		other.mu.Unlock()
	}
	e.mu.RUnlock()
	return fmt.Sprintf("# Server\r\nredis_version:7.4.0-docker-zero\r\nprocess_id:1\r\n# Replication\r\nrole:master\r\nconnected_slaves:%d\r\n# DockerZero\r\nseed:%d\r\nhealth:%s\r\n", connected, seed, health)
}
