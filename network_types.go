package main

import "fmt"

// PortBinding models one Docker HostConfig.PortBindings entry. RequestedHostPort
// is zero when Docker/Compose requested an ephemeral host port. HostPort is the
// concrete port currently assigned (and is preserved across stop/start).
type PortBinding struct {
	HostIP            string `json:"host_ip"`
	RequestedHostPort int    `json:"requested_host_port"`
	HostPort          int    `json:"host_port"`
	Protocol          string `json:"protocol"`
	ContainerPort     int    `json:"container_port"`
}

func (p PortBinding) key() string {
	protocol := p.Protocol
	if protocol == "" {
		protocol = "tcp"
	}
	return fmt.Sprintf("%d/%s", p.ContainerPort, protocol)
}

type RedisReplicationState struct {
	Role             string `json:"role"`
	MasterHost       string `json:"master_host,omitempty"`
	MasterPort       int    `json:"master_port,omitempty"`
	MasterID         string `json:"master_id,omitempty"`
	MasterLinkStatus string `json:"master_link_status"`
}

type DNSResult struct {
	Name      string `json:"name"`
	Network   string `json:"network"`
	Container string `json:"container"`
	IP        string `json:"ip"`
}
