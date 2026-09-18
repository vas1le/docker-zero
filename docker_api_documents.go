package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

func containerListDocument(c *Container, networks map[string]any) map[string]any {
	snapshot := c.snapshot()
	state := snapshot.State
	c.mu.Lock()
	imageID := c.ImageID
	command := strings.Join(append([]string(nil), c.Command...), " ")
	created := c.Created.Unix()
	labels := cloneStringMap(c.Labels)
	c.mu.Unlock()
	return map[string]any{
		"Id": snapshot.ID, "Names": []string{"/" + snapshot.Name}, "Image": snapshot.Image, "ImageID": imageID,
		"Command": command, "Created": created, "Ports": dockerPortSummary(c),
		"Labels": labels, "State": state.Status, "Status": humanContainerStatus(c),
		"HostConfig":      map[string]any{"NetworkMode": snapshot.NetworkMode, "Annotations": nil},
		"NetworkSettings": map[string]any{"Networks": networks}, "Mounts": []any{},
	}
}

func dockerPortSummary(c *Container) []any {
	bindings := c.clonePortBindings()
	keys := make([]string, 0, len(bindings))
	for key := range bindings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]any, 0)
	for _, key := range keys {
		parts := strings.SplitN(key, "/", 2)
		privatePort, _ := strconv.Atoi(parts[0])
		protocol := "tcp"
		if len(parts) == 2 {
			protocol = parts[1]
		}
		for _, binding := range bindings[key] {
			item := map[string]any{"PrivatePort": privatePort, "Type": protocol}
			if binding.HostPort > 0 {
				item["IP"] = dockerReportedHostIP(binding.HostIP)
				item["PublicPort"] = binding.HostPort
			}
			out = append(out, item)
		}
	}
	return out
}

func dockerReportedHostIP(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return "0.0.0.0"
	}
	return host
}

func dockerPortBindingDocuments(c *Container) (map[string]any, map[string]any, map[string]any) {
	bindings := c.clonePortBindings()
	hostConfig := make(map[string]any)
	networkPorts := make(map[string]any)
	exposed := map[string]any{fmt.Sprintf("%d/tcp", c.Spec.ContainerPort): map[string]any{}}
	c.mu.Lock()
	for port := range c.ExposedPorts {
		exposed[port] = map[string]any{}
	}
	c.mu.Unlock()
	for key, items := range bindings {
		exposed[key] = map[string]any{}
		hostItems := make([]any, 0, len(items))
		netItems := make([]any, 0, len(items))
		for _, binding := range items {
			hostPort := ""
			if binding.HostPort > 0 {
				hostPort = strconv.Itoa(binding.HostPort)
			}
			hostItems = append(hostItems, map[string]string{"HostIp": dockerReportedHostIP(binding.HostIP), "HostPort": hostPort})
			if binding.HostPort > 0 {
				netItems = append(netItems, map[string]string{"HostIp": dockerReportedHostIP(binding.HostIP), "HostPort": hostPort})
			}
		}
		hostConfig[key] = hostItems
		if len(netItems) == 0 {
			networkPorts[key] = nil
		} else {
			networkPorts[key] = netItems
		}
	}
	for key := range exposed {
		if _, ok := networkPorts[key]; !ok {
			networkPorts[key] = nil
		}
	}
	return hostConfig, networkPorts, exposed
}

func containerInspectDocument(c *Container, networks map[string]any) map[string]any {
	c.mu.Lock()
	state := map[string]any{
		"Status": c.Status, "Running": c.Running, "Paused": c.Paused, "Restarting": c.Restarting,
		"OOMKilled": c.OOMKilled, "Dead": c.Dead, "Pid": c.Pid, "ExitCode": c.ExitCode, "Error": c.Error,
		"StartedAt": formatDockerTime(c.Started), "FinishedAt": formatDockerTime(c.Finished),
	}
	if c.Health.Status != "" && !c.createMetadata.healthOverride {
		state["Health"] = map[string]any{"Status": c.Health.Status, "FailingStreak": c.Health.FailingStreak, "Log": slices.Clone(c.Health.Log)}
	}
	id, created, imageID, name, restartCount, networkMode := c.ID, c.Created, c.ImageID, c.Name, c.RestartCount, c.NetworkMode
	image, env, command := c.Image, append([]string(nil), c.Env...), append([]string(nil), c.Command...)
	labels := cloneStringMap(c.Labels)
	primaryIP := c.PrimaryIP
	containerPort := c.Spec.ContainerPort
	metadata := c.createMetadata
	restartPolicy := c.RestartPolicy
	c.mu.Unlock()

	var entrypoint []string
	if raw := metadata.config["Entrypoint"]; len(raw) != 0 {
		_ = json.Unmarshal(raw, &entrypoint) // Already validated by create decoding.
	}
	processCommand := append(entrypoint, command...)
	hostPortBindings, networkPorts, exposed := dockerPortBindingDocuments(c)
	document := map[string]any{
		"Id": id, "Created": formatDockerTime(created), "Path": firstOrEmpty(processCommand), "Args": restOrEmpty(processCommand), "State": state,
		"Image": imageID, "ResolvConfPath": "/var/lib/docker-zero/containers/" + id + "/resolv.conf",
		"HostnamePath": "/var/lib/docker-zero/containers/" + id + "/hostname", "HostsPath": "/var/lib/docker-zero/containers/" + id + "/hosts",
		"LogPath": "/var/lib/docker-zero/containers/" + id + "/" + id + "-json.log", "Name": "/" + name,
		"RestartCount": restartCount, "Driver": "docker-zero", "Platform": "linux", "MountLabel": "", "ProcessLabel": "", "AppArmorProfile": "", "ExecIDs": nil,
		"HostConfig": map[string]any{
			"Binds": nil, "ContainerIDFile": "", "LogConfig": map[string]any{"Type": "json-file", "Config": map[string]string{}},
			"NetworkMode": networkMode, "PortBindings": hostPortBindings,
			"RestartPolicy": restartPolicy, "AutoRemove": false,
			"VolumeDriver": "", "VolumesFrom": nil, "ConsoleSize": []int{0, 0}, "CapAdd": nil, "CapDrop": nil,
			"CgroupnsMode": "private", "Dns": []string{"127.0.0.11"}, "DnsOptions": nil, "DnsSearch": nil, "ExtraHosts": nil, "GroupAdd": nil,
			"IpcMode": "private", "Cgroup": "", "Links": nil, "OomScoreAdj": 0, "PidMode": "", "Privileged": false,
			"PublishAllPorts": false, "ReadonlyRootfs": false, "SecurityOpt": nil, "UTSMode": "", "UsernsMode": "", "ShmSize": 67108864, "Runtime": "runc", "Isolation": "", "CpuShares": 0, "Memory": 0, "NanoCpus": 0,
		},
		"GraphDriver": map[string]any{"Data": map[string]string{}, "Name": "docker-zero"}, "Mounts": []any{},
		"Config": map[string]any{
			"Hostname": id[:12], "Domainname": "", "User": "", "AttachStdin": false, "AttachStdout": true, "AttachStderr": true,
			"ExposedPorts": exposed, "Tty": false, "OpenStdin": false, "StdinOnce": false, "Env": env, "Cmd": command,
			"Healthcheck": map[string]any{"Test": []string{"CMD-SHELL", "docker-zero-health"}, "Interval": int64(5 * time.Second), "Timeout": int64(2 * time.Second), "Retries": 3},
			"Image":       image, "Volumes": nil, "WorkingDir": "", "Entrypoint": nil, "OnBuild": nil, "Labels": labels,
		},
		"NetworkSettings": map[string]any{
			"Bridge": "", "SandboxID": hashID("sandbox:" + id), "SandboxKey": "/var/run/docker/netns/" + id[:12], "Ports": networkPorts,
			"HairpinMode": false, "LinkLocalIPv6Address": "", "LinkLocalIPv6PrefixLen": 0, "SecondaryIPAddresses": nil, "SecondaryIPv6Addresses": nil,
			"EndpointID": hashID("endpoint:" + id), "Gateway": "127.0.0.1", "GlobalIPv6Address": "", "GlobalIPv6PrefixLen": 0,
			"IPAddress": primaryIP, "IPPrefixLen": 24, "IPv6Gateway": "", "MacAddress": macForContainer(id), "Networks": networks,
		},
		"DockerZero": map[string]any{"ContainerPort": containerPort, "MetadataOnly": slices.Clone(metadata.metadataOnly)},
	}
	config := document["Config"].(map[string]any)
	for field, raw := range metadata.config {
		config[field] = raw
	}
	hostConfig := document["HostConfig"].(map[string]any)
	for field, raw := range metadata.hostConfig {
		hostConfig[field] = raw
	}
	return document
}

func networkEndpointFor(c *Container, network *Network, endpoint map[string]any) map[string]any {
	aliases := []string{}
	switch values := endpoint["Aliases"].(type) {
	case []string:
		aliases = append(aliases, values...)
	case []any:
		for _, value := range values {
			if text, ok := value.(string); ok {
				aliases = append(aliases, text)
			}
		}
	}
	aliases = uniqueStrings(aliases)
	ip := endpointIPv4(endpoint)
	dnsNames := uniqueStrings(append(append([]string{}, aliases...), c.Name, c.ID[:minInt(12, len(c.ID))]))
	return map[string]any{
		"IPAMConfig": nil, "Links": nil, "Aliases": aliases, "MacAddress": macForContainer(c.ID), "DriverOpts": nil, "GwPriority": 0,
		"NetworkID": network.ID, "EndpointID": hashID("endpoint:" + network.ID + c.ID), "Gateway": networkGateway(network),
		"IPAddress": ip, "IPPrefixLen": 24, "IPv6Gateway": "", "GlobalIPv6Address": "", "GlobalIPv6PrefixLen": 0, "DNSNames": dnsNames,
	}
}

func networkGateway(network *Network) string {
	configs, _ := network.IPAM["Config"].([]any)
	if len(configs) > 0 {
		if item, ok := configs[0].(map[string]any); ok {
			if gateway, ok := item["Gateway"].(string); ok {
				return gateway
			}
		}
	}
	if network.Index == 0 {
		return "127.19.0.1"
	}
	return fmt.Sprintf("127.20.%d.1", network.Index)
}

func humanContainerStatus(c *Container) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.Status {
	case "running":
		age := time.Since(c.Started).Round(time.Second)
		if c.Started.IsZero() {
			age = 0
		}
		if c.Health.Status != "" && !c.createMetadata.healthOverride {
			return fmt.Sprintf("Up %s (%s)", age, c.Health.Status)
		}
		return fmt.Sprintf("Up %s", age)
	case "restarting":
		return fmt.Sprintf("Restarting (%d) 1 second ago", c.ExitCode)
	case "exited":
		return fmt.Sprintf("Exited (%d) 1 second ago", c.ExitCode)
	default:
		if c.Status == "" {
			return "Unknown"
		}
		return strings.ToUpper(c.Status[:1]) + c.Status[1:]
	}
}
