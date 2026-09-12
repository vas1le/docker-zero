package main

import (
	"embed"
	"fmt"
)

//go:embed cookbooks/*.json
var embeddedCookbooks embed.FS

type Cookbook struct {
	Kind       string              `json:"kind"`
	ImageNames []string            `json:"image_names"`
	Defaults   ServiceDefaults     `json:"defaults"`
	Seeds      map[string]Scenario `json:"seeds"`
}

type ServiceDefaults struct {
	Name          string `json:"name"`
	Image         string `json:"image"`
	ServiceType   string `json:"service_type"`
	IPAddress     string `json:"ip_address"`
	ContainerPort int    `json:"container_port"`
	HostIP        string `json:"host_ip"`
	HostPort      int    `json:"host_port"`
}

type Scenario struct {
	Description string                     `json:"description"`
	Initial     StatePatch                 `json:"initial"`
	Transitions []Transition               `json:"transitions,omitempty"`
	HTTP        map[string][]HTTPResponse  `json:"http,omitempty"`
	Redis       map[string][]RedisResponse `json:"redis,omitempty"`
}

type Transition struct {
	Event      string     `json:"event"`
	Count      int        `json:"count,omitempty"`
	WhenStatus string     `json:"when_status,omitempty"`
	WhenHealth string     `json:"when_health,omitempty"`
	Once       bool       `json:"once,omitempty"`
	Set        StatePatch `json:"set"`
}

type StatePatch struct {
	Status            *string `json:"status,omitempty"`
	Running           *bool   `json:"running,omitempty"`
	Restarting        *bool   `json:"restarting,omitempty"`
	Dead              *bool   `json:"dead,omitempty"`
	Health            *string `json:"health,omitempty"`
	ExitCode          *int    `json:"exit_code,omitempty"`
	Error             *string `json:"error,omitempty"`
	RestartCount      *int    `json:"restart_count,omitempty"`
	RestartCountDelta int     `json:"restart_count_delta,omitempty"`
}

type HTTPResponse struct {
	From       int               `json:"from"`
	WhenStatus string            `json:"when_status,omitempty"`
	WhenHealth string            `json:"when_health,omitempty"`
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	DelayMS    int               `json:"delay_ms,omitempty"`
	Close      bool              `json:"close,omitempty"`
	StatePatch StatePatch        `json:"set,omitempty"`
}

type RedisResponse struct {
	From       int        `json:"from"`
	WhenStatus string     `json:"when_status,omitempty"`
	WhenHealth string     `json:"when_health,omitempty"`
	Reply      string     `json:"reply,omitempty"`
	DelayMS    int        `json:"delay_ms,omitempty"`
	Close      bool       `json:"close,omitempty"`
	StatePatch StatePatch `json:"set,omitempty"`
}

type ValidationError struct {
	Path    string
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

func validationError(path, format string, args ...any) error {
	return &ValidationError{Path: path, Message: fmt.Sprintf(format, args...)}
}
