package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Network struct {
	Name       string                    `json:"Name"`
	ID         string                    `json:"Id"`
	Created    string                    `json:"Created"`
	Scope      string                    `json:"Scope"`
	Driver     string                    `json:"Driver"`
	EnableIPv6 bool                      `json:"EnableIPv6"`
	Internal   bool                      `json:"Internal"`
	Attachable bool                      `json:"Attachable"`
	Ingress    bool                      `json:"Ingress"`
	IPAM       map[string]any            `json:"IPAM"`
	Containers map[string]map[string]any `json:"Containers"`
	Options    map[string]string         `json:"Options"`
	Labels     map[string]string         `json:"Labels"`
	Index      int                       `json:"-"`
	NextHost   int                       `json:"-"`
}

type Volume struct {
	CreatedAt  string            `json:"CreatedAt"`
	Driver     string            `json:"Driver"`
	Labels     map[string]string `json:"Labels"`
	Mountpoint string            `json:"Mountpoint"`
	Name       string            `json:"Name"`
	Options    map[string]string `json:"Options"`
	Scope      string            `json:"Scope"`
}

type ExecInstance struct {
	mu sync.Mutex

	ID          string
	ContainerID string
	Command     []string
	Running     bool
	ExitCode    int
	Output      string
}

type Engine struct {
	mu sync.RWMutex

	cookbooks map[string]*Cookbook
	seed      int
	overrides map[string]int
	ledger    *Ledger
	startedAt time.Time
	counter   atomic.Int64

	containers   map[string]*Container
	names        map[string]string
	networks     map[string]*Network
	volumes      map[string]*Volume
	execs        map[string]*ExecInstance
	runtimes     map[string]*ContainerRuntime
	endpointMode string

	runtimeGate    sync.RWMutex
	runtimeClosing bool
	internalErrors chan error
}

func newEngine(cookbooks map[string]*Cookbook, seed int, overrides map[string]int, ledger *Ledger) *Engine {
	e := &Engine{
		cookbooks:      cookbooks,
		seed:           seed,
		overrides:      overrides,
		ledger:         ledger,
		startedAt:      time.Now().UTC(),
		containers:     make(map[string]*Container),
		names:          make(map[string]string),
		networks:       make(map[string]*Network),
		volumes:        make(map[string]*Volume),
		execs:          make(map[string]*ExecInstance),
		runtimes:       make(map[string]*ContainerRuntime),
		endpointMode:   "auto",
		internalErrors: make(chan error, 4),
	}
	e.networks["bridge"] = &Network{
		Name:       "bridge",
		ID:         hashID("network:bridge"),
		Created:    formatDockerTime(time.Now().UTC()),
		Scope:      "local",
		Driver:     "bridge",
		IPAM:       map[string]any{"Driver": "default", "Options": nil, "Config": []any{map[string]any{"Subnet": "127.19.0.0/24", "Gateway": "127.19.0.1"}}},
		Containers: make(map[string]map[string]any),
		Options:    map[string]string{},
		Labels:     map[string]string{},
		Index:      0,
		NextHost:   2,
	}
	return e
}

func (e *Engine) seedFor(kind, name string) int {
	if value, ok := e.overrides[strings.ToLower(strings.TrimPrefix(name, "/"))]; ok {
		return value
	}
	if value, ok := e.overrides[strings.ToLower(kind)]; ok {
		return value
	}
	return e.seed
}

func (e *Engine) cookbookFor(name, image string) (*Cookbook, error) {
	nameLower := strings.ToLower(name)
	imageLower := strings.ToLower(image)
	for _, kind := range sortedCookbookKinds(e.cookbooks) {
		cb := e.cookbooks[kind]
		if strings.Contains(nameLower, strings.ToLower(cb.Kind)) {
			return cb, nil
		}
		for _, candidate := range cb.ImageNames {
			candidate = strings.ToLower(candidate)
			if imageLower == candidate || strings.HasPrefix(imageLower, candidate+":") || strings.Contains(imageLower, candidate) {
				return cb, nil
			}
		}
	}
	return nil, fmt.Errorf("no cookbook matches name=%q image=%q", name, image)
}

func (e *Engine) createContainer(name, image string, labels map[string]string, env, command []string) (*Container, error) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	if name == "" {
		name = fmt.Sprintf("docker-zero-%d", e.counter.Add(1))
	}
	if err := validateContainerName(name); err != nil {
		return nil, err
	}
	if image == "" {
		return nil, errors.New("image is required")
	}
	cb, err := e.cookbookFor(name, image)
	if err != nil {
		return nil, err
	}
	seed := e.seedFor(cb.Kind, name)
	if _, ok := cb.Seeds[fmt.Sprintf("%d", seed)]; !ok {
		return nil, fmt.Errorf("cookbook %s has no seed %d", cb.Kind, seed)
	}

	e.mu.Lock()
	if _, exists := e.names[name]; exists {
		e.mu.Unlock()
		return nil, fmt.Errorf("conflict: the container name %q is already in use", name)
	}
	id := containerID(name, e.counter.Add(1))
	container := newContainer(id, name, image, cb, seed)
	container.Labels = cloneStringMap(labels)
	container.Env = append([]string(nil), env...)
	container.Command = append([]string(nil), command...)
	e.containers[id] = container
	e.names[name] = id
	e.mu.Unlock()

	after := container.stateSnapshot()
	e.ledger.Log(LedgerEntry{
		Container: name, Kind: container.Kind, Scenario: container.Seed, Channel: "docker", Event: "container.create",
		Request: map[string]any{"image": image, "name": name}, Response: map[string]any{"id": id}, After: &after,
	})
	return container, nil
}

func validateContainerName(name string) error {
	if len(name) > 255 {
		return fmt.Errorf("invalid container name %q: maximum length is 255 bytes", name)
	}
	for index, r := range name {
		isAlphaNumeric := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if index == 0 {
			if !isAlphaNumeric {
				return fmt.Errorf("invalid container name %q: first character must be ASCII alphanumeric", name)
			}
			continue
		}
		if !isAlphaNumeric && r != '_' && r != '.' && r != '-' {
			return fmt.Errorf("invalid container name %q: allowed characters are ASCII letters, digits, underscore, period, and hyphen", name)
		}
	}
	return nil
}

func (e *Engine) findContainer(ref string) (*Container, error) {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "/")
	e.mu.RLock()
	defer e.mu.RUnlock()
	if id, ok := e.names[ref]; ok {
		return e.containers[id], nil
	}
	if container, ok := e.containers[ref]; ok {
		return container, nil
	}
	var match *Container
	for id, container := range e.containers {
		if strings.HasPrefix(id, ref) {
			if match != nil {
				return nil, fmt.Errorf("ambiguous container reference %q", ref)
			}
			match = container
		}
	}
	if match == nil {
		return nil, fmt.Errorf("no such container: %s", ref)
	}
	return match, nil
}

func (e *Engine) findContainerByKind(kind string) (*Container, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var candidates []*Container
	for _, container := range e.containers {
		if container.Kind == kind {
			candidates = append(candidates, container)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no %s container exists", kind)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Created.Before(candidates[j].Created) })
	for _, candidate := range candidates {
		if candidate.isRunning() {
			return candidate, nil
		}
	}
	return candidates[0], nil
}

func (e *Engine) listContainers(all bool) []*Container {
	e.mu.RLock()
	defer e.mu.RUnlock()
	containers := make([]*Container, 0, len(e.containers))
	for _, container := range e.containers {
		if !all && !container.isRunning() {
			continue
		}
		containers = append(containers, container)
	}
	sort.Slice(containers, func(i, j int) bool { return containers[i].Created.Before(containers[j].Created) })
	return containers
}

func (e *Engine) removeContainer(ref string, force bool) error {
	container, err := e.findContainer(ref)
	if err != nil {
		return err
	}
	if container.isRunning() && !force {
		return fmt.Errorf("cannot remove running container %s: stop it first or force remove", container.ID)
	}
	_ = e.stopContainerRuntime(container)
	e.mu.Lock()
	delete(e.containers, container.ID)
	delete(e.runtimes, container.ID)
	delete(e.names, container.Name)
	for _, network := range e.networks {
		delete(network.Containers, container.ID)
	}
	e.mu.Unlock()
	before := container.stateSnapshot()
	if before.Running {
		container.stop(137)
	}
	container.markRemoved()
	e.ledger.Log(LedgerEntry{Container: container.Name, Kind: container.Kind, Scenario: container.Seed, Channel: "docker", Event: "container.remove", Before: &before})
	return nil
}

func (e *Engine) logContainerEvent(c *Container, event string, advance eventAdvance, request, response any) {
	before := advance.Before
	after := advance.After
	e.ledger.Log(LedgerEntry{
		Container: c.Name, Kind: c.Kind, Scenario: c.Seed, Channel: "docker", Event: event, Count: advance.Count,
		Request: request, Response: response, Transition: advance.Transition, Before: &before, After: &after,
	})
}

func (e *Engine) snapshots() []ContainerSnapshot {
	containers := e.listContainers(true)
	out := make([]ContainerSnapshot, 0, len(containers))
	for _, c := range containers {
		out = append(out, c.snapshot())
	}
	return out
}
