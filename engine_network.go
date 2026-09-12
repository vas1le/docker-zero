package main

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

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

	refs := make([]string, 0, len(resolved))
	for ref := range resolved {
		refs = append(refs, ref)
	}
	sort.Strings(refs)

	type endpointPlan struct {
		network  *Network
		endpoint map[string]any
		ip       string
		nextHost int
	}
	plans := make([]endpointPlan, 0, len(refs))
	primaryIP := ""
	resolvedNames := make([]string, 0, len(refs))

	// Plan every attachment first. No network or container state is mutated until
	// all address allocations succeed, so a failure cannot partially detach or
	// reattach a container.
	for _, ref := range refs {
		network := resolved[ref]
		ip, nextHost, err := planNetworkIPLocked(network, c.ID)
		if err != nil {
			return err
		}
		aliasesForEndpoint := uniqueStrings(append(append([]string(nil), wanted[ref]...), c.Name))
		if service := strings.TrimSpace(c.Labels["com.docker.compose.service"]); service != "" {
			aliasesForEndpoint = uniqueStrings(append(aliasesForEndpoint, service))
		}
		plans = append(plans, endpointPlan{
			network:  network,
			ip:       ip,
			nextHost: nextHost,
			endpoint: map[string]any{
				"Name":        c.Name,
				"EndpointID":  hashID("endpoint:" + network.ID + c.ID),
				"MacAddress":  macForContainer(c.ID),
				"IPv4Address": ip + "/24",
				"IPv6Address": "",
				"Aliases":     aliasesForEndpoint,
			},
		})
		if primaryIP == "" {
			primaryIP = ip
		}
		resolvedNames = append(resolvedNames, network.Name)
	}

	for _, network := range e.networks {
		delete(network.Containers, c.ID)
	}
	for _, plan := range plans {
		plan.network.Containers[c.ID] = plan.endpoint
		if plan.nextHost > plan.network.NextHost {
			plan.network.NextHost = plan.nextHost
		}
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

func planNetworkIPLocked(network *Network, containerID string) (string, int, error) {
	if endpoint, ok := network.Containers[containerID]; ok {
		if existing := endpointIPv4(endpoint); existing != "" {
			return existing, network.NextHost, nil
		}
	}

	usedHosts := make(map[int]struct{}, len(network.Containers))
	for id, endpoint := range network.Containers {
		if id == containerID {
			continue
		}
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
		nextHost := network.NextHost
		if host >= nextHost {
			nextHost = host + 1
		}
		if network.Index == 0 {
			return fmt.Sprintf("127.19.0.%d", host), nextHost, nil
		}
		return fmt.Sprintf("127.20.%d.%d", network.Index, host), nextHost, nil
	}
	return "", network.NextHost, fmt.Errorf("network %s exhausted its docker-zero /24 address space", network.Name)
}

func allocateNetworkIPLocked(network *Network) (string, error) {
	ip, nextHost, err := planNetworkIPLocked(network, "")
	if err != nil {
		return "", err
	}
	if nextHost > network.NextHost {
		network.NextHost = nextHost
	}
	return ip, nil
}

func macForContainer(id string) string {
	if len(id) < 8 {
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
