package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var syntheticPID atomic.Int64

type HealthState struct {
	Status        string      `json:"Status"`
	FailingStreak int         `json:"FailingStreak"`
	Log           []HealthLog `json:"Log"`
}

type HealthLog struct {
	Start    string `json:"Start"`
	End      string `json:"End"`
	ExitCode int    `json:"ExitCode"`
	Output   string `json:"Output"`
}

type Container struct {
	mu      sync.Mutex
	waiters map[*containerWaiter]struct{}
	removed bool

	ID       string
	Name     string
	Image    string
	ImageID  string
	Kind     string
	Seed     int
	Created  time.Time
	Started  time.Time
	Finished time.Time

	Spec     ServiceDefaults
	Scenario Scenario

	Status       string
	Running      bool
	Paused       bool
	Restarting   bool
	OOMKilled    bool
	Dead         bool
	Pid          int
	ExitCode     int
	Error        string
	RestartCount int
	Health       HealthState
	NetworkMode  string
	PrimaryIP    string
	PortBindings map[string][]PortBinding
	Redis        RedisReplicationState

	Counters           map[string]int
	AppliedTransitions map[int]bool
	Labels             map[string]string
	Env                []string
	Command            []string
	KV                 map[string]string
	Logs               []string
	StartCount         int
}

type ContainerStateSnapshot struct {
	Status       string `json:"status"`
	Running      bool   `json:"running"`
	Restarting   bool   `json:"restarting"`
	Paused       bool   `json:"paused"`
	Dead         bool   `json:"dead"`
	Health       string `json:"health"`
	ExitCode     int    `json:"exit_code"`
	RestartCount int    `json:"restart_count"`
	Error        string `json:"error,omitempty"`
}

type ContainerSnapshot struct {
	ID            string                 `json:"id"`
	Name          string                 `json:"name"`
	Image         string                 `json:"image"`
	Kind          string                 `json:"kind"`
	Seed          int                    `json:"seed"`
	Created       string                 `json:"created"`
	Started       string                 `json:"started,omitempty"`
	Finished      string                 `json:"finished,omitempty"`
	State         ContainerStateSnapshot `json:"state"`
	Counters      map[string]int         `json:"counters"`
	IPAddress     string                 `json:"ip_address"`
	ContainerPort int                    `json:"container_port"`
	HostPort      int                    `json:"host_port"`
	NetworkMode   string                 `json:"network_mode"`
}

type eventAdvance struct {
	Count      int
	Before     ContainerStateSnapshot
	After      ContainerStateSnapshot
	Transition string
}

func newContainer(id, name, image string, cb *Cookbook, seed int) *Container {
	scenario := cb.Seeds[fmt.Sprintf("%d", seed)]
	return &Container{
		ID:                 id,
		Name:               name,
		Image:              image,
		ImageID:            imageID(image),
		Kind:               cb.Kind,
		Seed:               seed,
		Created:            time.Now().UTC(),
		Spec:               cb.Defaults,
		Scenario:           scenario,
		Status:             "created",
		Health:             HealthState{Status: ""},
		NetworkMode:        "default",
		PortBindings:       make(map[string][]PortBinding),
		Redis:              RedisReplicationState{Role: "master", MasterLinkStatus: "up"},
		Counters:           make(map[string]int),
		AppliedTransitions: make(map[int]bool),
		Labels:             make(map[string]string),
		KV:                 make(map[string]string),
		Logs:               []string{"docker-zero: container created\n"},
	}
}

func imageID(image string) string {
	h := sha256.Sum256([]byte("image:" + image))
	return "sha256:" + hex.EncodeToString(h[:])
}

func containerID(name string, n int64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("container:%s:%d", name, n)))
	return hex.EncodeToString(h[:])
}

func (c *Container) snapshot() ContainerSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked()
}

func (c *Container) snapshotLocked() ContainerSnapshot {
	counters := make(map[string]int, len(c.Counters))
	for key, value := range c.Counters {
		counters[key] = value
	}
	return ContainerSnapshot{
		ID:            c.ID,
		Name:          c.Name,
		Image:         c.Image,
		Kind:          c.Kind,
		Seed:          c.Seed,
		Created:       formatDockerTime(c.Created),
		Started:       optionalDockerTime(c.Started),
		Finished:      optionalDockerTime(c.Finished),
		State:         c.stateSnapshotLocked(),
		Counters:      counters,
		IPAddress:     c.PrimaryIP,
		ContainerPort: c.Spec.ContainerPort,
		HostPort:      c.primaryHostPortLocked(),
		NetworkMode:   c.NetworkMode,
	}
}

func (c *Container) primaryHostPortLocked() int {
	key := fmt.Sprintf("%d/tcp", c.Spec.ContainerPort)
	items := c.PortBindings[key]
	if len(items) == 0 {
		return 0
	}
	return items[0].HostPort
}

func (c *Container) setPrimaryIP(ip string) {
	c.mu.Lock()
	c.PrimaryIP = ip
	c.mu.Unlock()
}

func (c *Container) clonePortBindings() map[string][]PortBinding {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string][]PortBinding, len(c.PortBindings))
	for key, values := range c.PortBindings {
		out[key] = append([]PortBinding(nil), values...)
	}
	return out
}

func (c *Container) setPortBindings(bindings map[string][]PortBinding) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.PortBindings = make(map[string][]PortBinding, len(bindings))
	for key, values := range bindings {
		c.PortBindings[key] = append([]PortBinding(nil), values...)
	}
}

func (c *Container) updateAssignedPort(key string, index int, hostPort int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	items := c.PortBindings[key]
	if index < 0 || index >= len(items) {
		return
	}
	items[index].HostPort = hostPort
	c.PortBindings[key] = items
}

func (c *Container) resetEphemeralAssignedPorts() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, items := range c.PortBindings {
		for index := range items {
			if items[index].RequestedHostPort == 0 {
				items[index].HostPort = 0
			}
		}
		c.PortBindings[key] = items
	}
}

func (c *Container) stateSnapshot() ContainerStateSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stateSnapshotLocked()
}

func (c *Container) stateSnapshotLocked() ContainerStateSnapshot {
	return ContainerStateSnapshot{
		Status:       c.Status,
		Running:      c.Running,
		Restarting:   c.Restarting,
		Paused:       c.Paused,
		Dead:         c.Dead,
		Health:       c.Health.Status,
		ExitCode:     c.ExitCode,
		RestartCount: c.RestartCount,
		Error:        c.Error,
	}
}

func (c *Container) advance(event string) eventAdvance {
	c.mu.Lock()
	defer c.mu.Unlock()

	before := c.stateSnapshotLocked()
	c.Counters[event]++
	count := c.Counters[event]
	if isDockerObserveEvent(event) {
		c.Counters["docker.observe"]++
	}
	transitionDescription := ""

	for index, transition := range c.Scenario.Transitions {
		if transition.Once && c.AppliedTransitions[index] {
			continue
		}
		if !transitionEventMatches(transition.Event, event) {
			continue
		}
		transitionCount := count
		if transition.Event == "docker.observe" {
			transitionCount = c.Counters["docker.observe"]
		}
		if transition.Count > 0 && transition.Count != transitionCount {
			continue
		}
		if transition.WhenStatus != "" && transition.WhenStatus != c.Status {
			continue
		}
		if transition.WhenHealth != "" && transition.WhenHealth != c.Health.Status {
			continue
		}
		c.applyPatchLocked(transition.Set)
		if transition.Once {
			c.AppliedTransitions[index] = true
		}
		transitionDescription = fmt.Sprintf("transition[%d]", index)
		// A single external event advances at most one lifecycle edge. This makes
		// exited -> restarting -> running observable over separate inspections.
		break
	}

	return eventAdvance{
		Count:      count,
		Before:     before,
		After:      c.stateSnapshotLocked(),
		Transition: transitionDescription,
	}
}

func isDockerObserveEvent(event string) bool {
	return event == "docker.inspect" || event == "docker.list"
}

func transitionEventMatches(pattern, event string) bool {
	return pattern == event || (pattern == "docker.observe" && isDockerObserveEvent(event))
}

func (c *Container) applyPatch(patch StatePatch) (ContainerStateSnapshot, ContainerStateSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	before := c.stateSnapshotLocked()
	c.applyPatchLocked(patch)
	return before, c.stateSnapshotLocked()
}

func (c *Container) applyPatchLocked(patch StatePatch) {
	oldStatus := c.Status
	oldHealth := c.Health.Status

	if patch.Status != nil {
		c.Status = *patch.Status
	}
	if patch.Running != nil {
		c.Running = *patch.Running
	}
	if patch.Restarting != nil {
		c.Restarting = *patch.Restarting
	}
	if patch.Dead != nil {
		c.Dead = *patch.Dead
	}
	if patch.Health != nil {
		c.Health.Status = *patch.Health
	}
	if patch.ExitCode != nil {
		c.ExitCode = *patch.ExitCode
	}
	if patch.Error != nil {
		c.Error = *patch.Error
	}
	if patch.RestartCount != nil {
		c.RestartCount = *patch.RestartCount
	}
	if patch.RestartCountDelta != 0 {
		c.RestartCount += patch.RestartCountDelta
	}

	// Docker lifecycle flags are derived from Status. Keeping them canonical
	// prevents a partial cookbook patch from creating impossible combinations
	// such as status=running with Restarting=true or status=exited with a PID.
	switch c.Status {
	case "running":
		c.Running = true
		c.Paused = false
		c.Restarting = false
		c.Dead = false
	case "paused":
		c.Running = true
		c.Paused = true
		c.Restarting = false
		c.Dead = false
	case "restarting":
		c.Running = true
		c.Paused = false
		c.Restarting = true
		c.Dead = false
	case "created", "exited":
		c.Running = false
		c.Paused = false
		c.Restarting = false
		c.Dead = false
	case "dead":
		c.Running = false
		c.Paused = false
		c.Restarting = false
		c.Dead = true
	default:
		panic(fmt.Sprintf("internal invalid container status %q", c.Status))
	}

	now := time.Now().UTC()
	if c.Status == "running" && oldStatus != "running" {
		c.Started = now
		c.Finished = time.Time{}
		if c.Pid == 0 {
			c.Pid = 1000 + int(syntheticPID.Add(1))
		}
	}
	if (c.Status == "exited" || c.Status == "dead") && oldStatus != c.Status {
		c.Finished = now
		c.Pid = 0
	}
	if c.Status == "restarting" && oldStatus != "restarting" {
		c.Finished = now
		c.Pid = 0
	}
	if c.Status == "created" {
		c.Pid = 0
	}
	if c.Status != oldStatus && (c.Status == "exited" || c.Status == "dead" || c.Status == "restarting") {
		c.notifyWaitersLocked(false)
	}
	if c.Health.Status != oldHealth && c.Health.Status != "" {
		exitCode := 0
		switch c.Health.Status {
		case "unhealthy":
			exitCode = 1
			c.Health.FailingStreak++
		case "healthy":
			c.Health.FailingStreak = 0
		}
		c.Health.Log = append(c.Health.Log, HealthLog{
			Start:    formatDockerTime(now),
			End:      formatDockerTime(now),
			ExitCode: exitCode,
			Output:   fmt.Sprintf("docker-zero: health changed to %s\n", c.Health.Status),
		})
		if len(c.Health.Log) > 5 {
			c.Health.Log = c.Health.Log[len(c.Health.Log)-5:]
		}
	}
}

func (c *Container) start() (ContainerStateSnapshot, ContainerStateSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	before := c.stateSnapshotLocked()
	patch := c.startPatchLocked()
	c.applyPatchLocked(patch)
	c.StartCount++
	c.Logs = append(c.Logs, fmt.Sprintf("docker-zero: started with seed %d (%s), start #%d\n", c.Seed, c.Scenario.Description, c.StartCount))
	return before, c.stateSnapshotLocked()
}

// startPatchLocked applies the complete cookbook initial state only on the first
// start. Later stop/start cycles preserve cumulative runtime facts such as
// RestartCount while restoring the service-facing state defined by the seed.
func (c *Container) startPatchLocked() StatePatch {
	initial := c.Scenario.Initial
	if c.StartCount == 0 {
		if initial.Status == nil {
			initial.Status = stringPtr("running")
		}
		if initial.Running == nil {
			initial.Running = boolPtr(true)
		}
		if initial.Restarting == nil {
			initial.Restarting = boolPtr(false)
		}
		if initial.Dead == nil {
			initial.Dead = boolPtr(false)
		}
		if initial.ExitCode == nil {
			initial.ExitCode = intPtr(0)
		}
		return initial
	}

	patch := StatePatch{
		Status:     initial.Status,
		Running:    boolPtr(true),
		Restarting: boolPtr(false),
		Dead:       boolPtr(false),
		Health:     initial.Health,
		ExitCode:   intPtr(0),
		Error:      stringPtr(""),
	}
	if patch.Status == nil {
		patch.Status = stringPtr("running")
	}
	return patch
}

func (c *Container) stop(exitCode int) (ContainerStateSnapshot, ContainerStateSnapshot) {
	return c.applyPatch(StatePatch{
		Status:     stringPtr("exited"),
		Running:    boolPtr(false),
		Restarting: boolPtr(false),
		ExitCode:   intPtr(exitCode),
	})
}

func (c *Container) pause() (ContainerStateSnapshot, ContainerStateSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	before := c.stateSnapshotLocked()
	if !c.Running || c.Restarting || c.Dead {
		return before, before, fmt.Errorf("container %s is not running", c.Name)
	}
	if c.Paused {
		return before, before, nil
	}
	c.Status = "paused"
	c.Paused = true
	return before, c.stateSnapshotLocked(), nil
}

func (c *Container) unpause() (ContainerStateSnapshot, ContainerStateSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	before := c.stateSnapshotLocked()
	if !c.Paused {
		return before, before, fmt.Errorf("container %s is not paused", c.Name)
	}
	c.Status = "running"
	c.Running = true
	c.Paused = false
	c.Restarting = false
	c.Dead = false
	return before, c.stateSnapshotLocked(), nil
}

func (c *Container) restart() (ContainerStateSnapshot, ContainerStateSnapshot) {
	return c.applyPatch(StatePatch{
		Status:            stringPtr("restarting"),
		Running:           boolPtr(true),
		Restarting:        boolPtr(true),
		Health:            stringPtr("starting"),
		ExitCode:          intPtr(0),
		RestartCountDelta: 1,
	})
}

func (c *Container) eventCount(event string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Counters[event]
}

func (c *Container) isRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Running && !c.Restarting && !c.Dead
}

func (c *Container) isDockerPSVisible() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Running && !c.Dead
}

func (c *Container) routeResponse(route string, count int) (HTTPResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	items, ok := c.Scenario.HTTP[route]
	if !ok {
		items, ok = c.Scenario.HTTP["*"]
	}
	if !ok || len(items) == 0 {
		return HTTPResponse{}, false
	}
	return selectHTTPResponse(items, count, c.Status, c.Health.Status)
}

func (c *Container) redisResponse(command string, count int) (RedisResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	items, ok := c.Scenario.Redis[strings.ToUpper(command)]
	if !ok {
		items, ok = c.Scenario.Redis["*"]
	}
	if !ok || len(items) == 0 {
		return RedisResponse{}, false
	}
	return selectRedisResponse(items, count, c.Status, c.Health.Status)
}

func (c *Container) appendLog(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Logs = append(c.Logs, line)
	if len(c.Logs) > 2000 {
		c.Logs = c.Logs[len(c.Logs)-2000:]
	}
}

func (c *Container) allLogs() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.Logs, "")
}

func formatDockerTime(t time.Time) string {
	if t.IsZero() {
		return "0001-01-01T00:00:00Z"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func optionalDockerTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return formatDockerTime(t)
}

func stringPtr(v string) *string { return &v }
func boolPtr(v bool) *bool       { return &v }
func intPtr(v int) *int          { return &v }
