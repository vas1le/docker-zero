package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
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
		return nil, errors.New("Image is required")
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
	defer e.mu.Unlock()
	if _, exists := e.names[name]; exists {
		return nil, fmt.Errorf("Conflict. The container name %q is already in use", name)
	}
	id := containerID(name, e.counter.Add(1))
	container := newContainer(id, name, image, cb, seed)
	container.Labels = cloneStringMap(labels)
	container.Env = append([]string(nil), env...)
	container.Command = append([]string(nil), command...)
	e.containers[id] = container
	e.names[name] = id

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
		return nil, fmt.Errorf("No such container: %s", ref)
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
		return fmt.Errorf("You cannot remove a running container %s. Stop the container before attempting removal or force remove", container.ID)
	}
	_ = e.stopContainerRuntime(container)
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.containers, container.ID)
	delete(e.runtimes, container.ID)
	delete(e.names, container.Name)
	for _, network := range e.networks {
		delete(network.Containers, container.ID)
	}
	before := container.stateSnapshot()
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

func (e *Engine) patchAndLog(c *Container, channel, event string, patch StatePatch, request, response any) {
	before, after := c.applyPatch(patch)
	e.ledger.Log(LedgerEntry{
		Container: c.Name, Kind: c.Kind, Scenario: c.Seed, Channel: channel, Event: event,
		Request: request, Response: response, Before: &before, After: &after,
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
		return nil, fmt.Errorf("No such exec instance: %s", id)
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

func marshalJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func isDefaultNetworkMode(mode string) bool {
	mode = strings.TrimSpace(mode)
	return mode == "" || mode == "default" || mode == "bridge"
}

func (e *Engine) configureContainerNetworks(c *Container, networkMode string, aliases map[string][]string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	wanted := make(map[string][]string, len(aliases)+1)
	for name, values := range aliases {
		wanted[name] = append([]string(nil), values...)
	}

	mode := strings.TrimSpace(networkMode)
	if mode == "host" || mode == "none" {
		for _, network := range e.networks {
			delete(network.Containers, c.ID)
		}
		c.mu.Lock()
		c.NetworkMode = mode
		c.PrimaryIP = ""
		c.mu.Unlock()
		return nil
	}

	if !isDefaultNetworkMode(mode) {
		if _, ok := wanted[mode]; !ok {
			wanted[mode] = nil
		}
	}
	if len(wanted) == 0 {
		wanted["bridge"] = nil
	}

	resolved := make(map[string]*Network, len(wanted))
	for ref := range wanted {
		var found *Network
		for id, network := range e.networks {
			if id == ref || network.Name == ref || strings.HasPrefix(id, ref) {
				found = network
				break
			}
		}
		if found == nil {
			return fmt.Errorf("network %s not found", ref)
		}
		resolved[ref] = found
	}

	// Docker create attaches the container only to the explicitly selected
	// network(s). This matters for Compose project isolation and DNS.
	for _, network := range e.networks {
		delete(network.Containers, c.ID)
	}

	refs := make([]string, 0, len(resolved))
	for ref := range resolved {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	primaryIP := ""
	resolvedNames := make([]string, 0, len(refs))
	for _, ref := range refs {
		network := resolved[ref]
		ip, err := allocateNetworkIPLocked(network)
		if err != nil {
			return err
		}
		aliasesForEndpoint := uniqueStrings(append(append([]string(nil), wanted[ref]...), c.Name))
		if service := strings.TrimSpace(c.Labels["com.docker.compose.service"]); service != "" {
			aliasesForEndpoint = uniqueStrings(append(aliasesForEndpoint, service))
		}
		network.Containers[c.ID] = map[string]any{
			"Name":        c.Name,
			"EndpointID":  hashID("endpoint:" + network.ID + c.ID),
			"MacAddress":  macForContainer(c.ID),
			"IPv4Address": ip + "/24",
			"IPv6Address": "",
			"Aliases":     aliasesForEndpoint,
		}
		if primaryIP == "" {
			primaryIP = ip
		}
		resolvedNames = append(resolvedNames, network.Name)
	}

	if isDefaultNetworkMode(mode) {
		sort.Strings(resolvedNames)
		if len(resolvedNames) != 0 {
			mode = resolvedNames[0]
		} else {
			mode = "default"
		}
	}
	c.mu.Lock()
	c.NetworkMode = mode
	c.PrimaryIP = primaryIP
	c.mu.Unlock()
	return nil
}

func allocateNetworkIPLocked(network *Network) (string, error) {
	usedHosts := make(map[int]struct{}, len(network.Containers))
	for _, endpoint := range network.Containers {
		ip := endpointIPv4(endpoint)
		if ip == "" {
			continue
		}
		parts := strings.Split(ip, ".")
		if len(parts) != 4 {
			continue
		}
		if host, err := strconv.Atoi(parts[3]); err == nil {
			usedHosts[host] = struct{}{}
		}
	}
	for host := 2; host <= 254; host++ {
		if _, used := usedHosts[host]; used {
			continue
		}
		if host >= network.NextHost {
			network.NextHost = host + 1
		}
		if network.Index == 0 {
			return fmt.Sprintf("127.19.0.%d", host), nil
		}
		return fmt.Sprintf("127.20.%d.%d", network.Index, host), nil
	}
	return "", fmt.Errorf("network %s exhausted its docker-zero /24 address space", network.Name)
}

func macForContainer(id string) string {
	if len(id) < 6 {
		return "02:42:7f:14:00:02"
	}
	return fmt.Sprintf("02:42:%s:%s:%s:%s", id[0:2], id[2:4], id[4:6], id[6:8])
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func endpointIPv4(endpoint map[string]any) string {
	value, _ := endpoint["IPv4Address"].(string)
	if value == "" {
		return ""
	}
	if ip, _, err := net.ParseCIDR(value); err == nil {
		return ip.String()
	}
	return strings.SplitN(value, "/", 2)[0]
}

func (e *Engine) containerEndpointAddresses(c *Container) []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	addresses := make([]string, 0)
	for _, network := range e.networks {
		endpoint, ok := network.Containers[c.ID]
		if !ok {
			continue
		}
		ip := endpointIPv4(endpoint)
		if ip != "" {
			addresses = append(addresses, net.JoinHostPort(ip, strconv.Itoa(c.Spec.ContainerPort)))
		}
	}
	sort.Strings(addresses)
	return addresses
}

func (e *Engine) containerNetworkEndpoints(c *Container) map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]any)
	for _, network := range e.networks {
		endpoint, ok := network.Containers[c.ID]
		if !ok {
			continue
		}
		out[network.Name] = networkEndpointFor(c, network, endpoint)
	}
	return out
}

func (e *Engine) resolveDNS(requester *Container, name string) []DNSResult {
	name = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(name, ".")))
	if name == "" || requester == nil {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	results := make([]DNSResult, 0)
	for _, network := range e.networks {
		if _, joined := network.Containers[requester.ID]; !joined {
			continue
		}
		for containerID, endpoint := range network.Containers {
			candidate := e.containers[containerID]
			if candidate == nil || !endpointMatchesDNSName(candidate, endpoint, name) {
				continue
			}
			ip := endpointIPv4(endpoint)
			if ip == "" {
				continue
			}
			results = append(results, DNSResult{Name: name, Network: network.Name, Container: candidate.Name, IP: ip})
		}
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Network == results[j].Network {
			if results[i].Container == results[j].Container {
				return results[i].IP < results[j].IP
			}
			return results[i].Container < results[j].Container
		}
		return results[i].Network < results[j].Network
	})
	return results
}

func endpointMatchesDNSName(c *Container, endpoint map[string]any, name string) bool {
	candidates := []string{c.Name, strings.TrimPrefix(c.Name, "/"), c.ID, c.ID[:minInt(12, len(c.ID))], c.Labels["com.docker.compose.service"]}
	switch aliases := endpoint["Aliases"].(type) {
	case []string:
		candidates = append(candidates, aliases...)
	case []any:
		for _, value := range aliases {
			if text, ok := value.(string); ok {
				candidates = append(candidates, text)
			}
		}
	}
	for _, candidate := range candidates {
		if strings.EqualFold(strings.TrimSpace(candidate), name) {
			return true
		}
	}
	return false
}

func (e *Engine) findContainerByEndpointIP(ip string) *Container {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, network := range e.networks {
		for id, endpoint := range network.Containers {
			if endpointIPv4(endpoint) == ip {
				return e.containers[id]
			}
		}
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (e *Engine) containerConnectedToNetwork(c *Container, networkRef string) (bool, *Network) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for id, network := range e.networks {
		if id != networkRef && network.Name != networkRef && !strings.HasPrefix(id, networkRef) {
			continue
		}
		_, connected := network.Containers[c.ID]
		return connected, network
	}
	return false, nil
}

func (e *Engine) connectContainerNetwork(c *Container, networkRef string, aliases []string) error {
	e.mu.Lock()
	var network *Network
	for id, candidate := range e.networks {
		if id == networkRef || candidate.Name == networkRef || strings.HasPrefix(id, networkRef) {
			network = candidate
			break
		}
	}
	if network == nil {
		e.mu.Unlock()
		return fmt.Errorf("network %s not found", networkRef)
	}
	if _, exists := network.Containers[c.ID]; exists {
		e.mu.Unlock()
		return fmt.Errorf("endpoint with name %s already exists in network %s", c.Name, network.Name)
	}
	ip, err := allocateNetworkIPLocked(network)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	aliases = uniqueStrings(append(aliases, c.Name, c.Labels["com.docker.compose.service"]))
	network.Containers[c.ID] = map[string]any{
		"Name": c.Name, "EndpointID": hashID("endpoint:" + network.ID + c.ID), "MacAddress": macForContainer(c.ID),
		"IPv4Address": ip + "/24", "IPv6Address": "", "Aliases": aliases,
	}
	primary := ""
	for _, candidate := range e.networks {
		if endpoint, ok := candidate.Containers[c.ID]; ok {
			primary = endpointIPv4(endpoint)
			if primary != "" {
				break
			}
		}
	}
	e.mu.Unlock()
	c.setPrimaryIP(primary)
	return nil
}

func (e *Engine) disconnectContainerNetwork(c *Container, networkRef string) error {
	e.mu.Lock()
	var network *Network
	for id, candidate := range e.networks {
		if id == networkRef || candidate.Name == networkRef || strings.HasPrefix(id, networkRef) {
			network = candidate
			break
		}
	}
	if network == nil {
		e.mu.Unlock()
		return fmt.Errorf("network %s not found", networkRef)
	}
	delete(network.Containers, c.ID)
	primary := ""
	for _, candidate := range e.networks {
		if endpoint, ok := candidate.Containers[c.ID]; ok {
			primary = endpointIPv4(endpoint)
			if primary != "" {
				break
			}
		}
	}
	e.mu.Unlock()
	c.setPrimaryIP(primary)
	return nil
}
