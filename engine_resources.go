package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

func (e *Engine) createNetwork(name, driver string, labels map[string]string) (*Network, error) {
	if name == "" {
		return nil, errors.New("network name is required")
	}
	if driver == "" {
		driver = "bridge"
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, network := range e.networks {
		if network.Name == name {
			return nil, fmt.Errorf("network with name %s already exists", name)
		}
	}
	index := 0
	usedIndexes := make(map[int]struct{}, len(e.networks))
	for _, network := range e.networks {
		usedIndexes[network.Index] = struct{}{}
	}
	for candidate := 1; candidate <= 220; candidate++ {
		if _, used := usedIndexes[candidate]; !used {
			index = candidate
			break
		}
	}
	if index == 0 {
		return nil, fmt.Errorf("docker-zero exhausted virtual network address space")
	}
	subnet := fmt.Sprintf("127.20.%d.0/24", index)
	gateway := fmt.Sprintf("127.20.%d.1", index)
	n := &Network{
		Name: name, ID: hashID(fmt.Sprintf("network:%s:%d", name, e.counter.Add(1))),
		Created: formatDockerTime(time.Now().UTC()), Scope: "local", Driver: driver,
		IPAM:       map[string]any{"Driver": "default", "Options": nil, "Config": []any{map[string]any{"Subnet": subnet, "Gateway": gateway}}},
		Containers: make(map[string]map[string]any), Options: map[string]string{}, Labels: cloneStringMap(labels),
		Index: index, NextHost: 2,
	}
	e.networks[n.ID] = n
	e.ledger.Log(LedgerEntry{Channel: "docker", Event: "network.create", Request: map[string]any{"name": name, "driver": driver}, Response: map[string]any{"id": n.ID}})
	return n, nil
}

func (e *Engine) findNetwork(ref string) (*Network, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for id, n := range e.networks {
		if id == ref || n.Name == ref || strings.HasPrefix(id, ref) {
			return n, nil
		}
	}
	return nil, fmt.Errorf("network %s not found", ref)
}

var (
	errPredefinedNetwork   = errors.New("operation not supported for pre-defined network")
	errNetworkHasEndpoints = errors.New("network has active endpoints")
)

type networkOperationError struct {
	kind    error
	message string
}

func (e *networkOperationError) Error() string { return e.message }
func (e *networkOperationError) Unwrap() error { return e.kind }

func (e *Engine) removeNetwork(ref string) error {
	n, err := e.findNetwork(ref)
	if err != nil {
		return err
	}
	if n.Name == "bridge" {
		return &networkOperationError{kind: errPredefinedNetwork, message: "bridge is a pre-defined network and cannot be removed"}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(n.Containers) != 0 {
		attached := make([]string, 0, len(n.Containers))
		for containerID := range n.Containers {
			if c := e.containers[containerID]; c != nil {
				attached = append(attached, c.Name)
			} else {
				attached = append(attached, containerID[:minInt(12, len(containerID))])
			}
		}
		sort.Strings(attached)
		return &networkOperationError{
			kind:    errNetworkHasEndpoints,
			message: fmt.Sprintf("error while removing network: network %s id %s has active endpoints (%s)", n.Name, n.ID, strings.Join(attached, ", ")),
		}
	}
	delete(e.networks, n.ID)
	e.ledger.Log(LedgerEntry{Channel: "docker", Event: "network.remove", Request: map[string]any{"id": n.ID, "name": n.Name}})
	return nil
}

func (e *Engine) createVolume(name, driver string, labels, options map[string]string) *Volume {
	if name == "" {
		name = "docker-zero-" + hashID(fmt.Sprintf("volume:%d", e.counter.Add(1)))[:12]
	}
	if driver == "" {
		driver = "local"
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if volume, ok := e.volumes[name]; ok {
		return volume
	}
	v := &Volume{
		CreatedAt: formatDockerTime(time.Now().UTC()), Driver: driver, Labels: cloneStringMap(labels),
		Mountpoint: "/var/lib/docker-zero/volumes/" + name + "/_data", Name: name,
		Options: cloneStringMap(options), Scope: "local",
	}
	e.volumes[name] = v
	e.ledger.Log(LedgerEntry{Channel: "docker", Event: "volume.create", Request: map[string]any{"name": name}, Response: v})
	return v
}

func (e *Engine) createExec(c *Container, command []string) *ExecInstance {
	id := hashID(fmt.Sprintf("exec:%s:%d", c.ID, e.counter.Add(1)))
	exec := &ExecInstance{ID: id, ContainerID: c.ID, Command: command, ExitCode: 0, Output: "docker-zero exec: " + strings.Join(command, " ") + "\n"}
	e.mu.Lock()
	e.execs[id] = exec
	e.mu.Unlock()
	e.ledger.Log(LedgerEntry{Container: c.Name, Kind: c.Kind, Scenario: c.Seed, Channel: "docker", Event: "exec.create", Request: map[string]any{"cmd": command}, Response: map[string]any{"id": id}})
	return exec
}

func (e *Engine) findExec(id string) (*ExecInstance, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	exec, ok := e.execs[id]
	if !ok {
		return nil, fmt.Errorf("no such exec instance: %s", id)
	}
	return exec, nil
}

func (e *Engine) stateDocument() map[string]any {
	e.mu.RLock()
	networkCount := len(e.networks)
	volumeCount := len(e.volumes)
	e.mu.RUnlock()
	return map[string]any{
		"name":       "docker-zero",
		"seed":       e.seed,
		"overrides":  e.overrides,
		"started_at": formatDockerTime(e.startedAt),
		"run_dir":    e.ledger.RunDir(),
		"containers": e.snapshots(),
		"networks":   networkCount,
		"volumes":    volumeCount,
	}
}

func cloneNetwork(network *Network) *Network {
	if network == nil {
		return nil
	}
	clone := *network
	clone.Options = cloneStringMap(network.Options)
	clone.Labels = cloneStringMap(network.Labels)
	clone.IPAM = cloneAnyMap(network.IPAM)
	clone.Containers = make(map[string]map[string]any, len(network.Containers))
	for id, endpoint := range network.Containers {
		clone.Containers[id] = cloneAnyMap(endpoint)
	}
	return &clone
}

func cloneAnyMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func hashID(value string) string {
	return containerID(value, 0)
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
