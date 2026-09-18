package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStripAPIVersion(t *testing.T) {
	cases := map[string]string{
		"/_ping":                          "/_ping",
		"/v1.43/containers/json":          "/containers/json",
		"/v1.55/containers/abc/json":      "/containers/abc/json",
		"/version":                        "/version",
		"/vnot-a-version/containers/json": "/vnot-a-version/containers/json",
	}
	for input, expected := range cases {
		if actual := stripAPIVersion(input); actual != expected {
			t.Fatalf("stripAPIVersion(%q) = %q, expected %q", input, actual, expected)
		}
	}
}

func TestResponseSelectionUsesCountAndContainerState(t *testing.T) {
	httpItems := []HTTPResponse{
		{From: 1, WhenHealth: "starting", Status: 503},
		{From: 1, WhenHealth: "healthy", Status: 200},
	}
	if got, ok := selectHTTPResponse(httpItems, 1, "running", "starting"); !ok || got.Status != 503 {
		t.Fatalf("starting HTTP response = %#v, %v; expected 503", got, ok)
	}
	if got, ok := selectHTTPResponse(httpItems, 1, "running", "healthy"); !ok || got.Status != 200 {
		t.Fatalf("healthy HTTP response = %#v, %v; expected 200", got, ok)
	}
	if _, ok := selectHTTPResponse(httpItems, 1, "exited", "unhealthy"); ok {
		t.Fatal("unexpected HTTP response for unmatched state")
	}

	redisItems := []RedisResponse{
		{From: 1, WhenStatus: "running", WhenHealth: "healthy", Reply: "+PONG\r\n"},
		{From: 2, WhenStatus: "exited", Close: true},
	}
	if got, ok := selectRedisResponse(redisItems, 1, "running", "healthy"); !ok || got.Reply != "+PONG\r\n" {
		t.Fatalf("running Redis response = %#v, %v", got, ok)
	}
	if got, ok := selectRedisResponse(redisItems, 2, "exited", "unhealthy"); !ok || !got.Close {
		t.Fatalf("exited Redis response = %#v, %v; expected close", got, ok)
	}
}

func TestCookbooksContainThreeSeedsAndBaselineIsHealthy(t *testing.T) {
	cookbooks, err := loadCookbooks("")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"nginx", "redis"} {
		cb, ok := cookbooks[kind]
		if !ok {
			t.Fatalf("missing %s cookbook", kind)
		}
		for _, seed := range []string{"0", "1", "2"} {
			if _, ok := cb.Seeds[seed]; !ok {
				t.Fatalf("%s cookbook missing seed %s", kind, seed)
			}
		}
		baseline := cb.Seeds["0"].Initial
		if baseline.Status == nil || *baseline.Status != "running" || baseline.Health == nil || *baseline.Health != "healthy" {
			t.Fatalf("%s seed 0 is not running/healthy: %#v", kind, baseline)
		}
	}
}

func TestSeedTwoCrashAndRestartLifecycle(t *testing.T) {
	cookbooks, err := loadCookbooks("")
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := newLedger(filepath.Join(t.TempDir(), "runs"), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer testCloseLedger(t, ledger)
	engine := newEngine(cookbooks, 2, nil, ledger)
	container, err := engine.createContainer("nginx-zero", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	container.RestartPolicy = RestartPolicy{Name: "always"}
	container.start()
	container.advance("http.GET /health")
	crash := container.advance("http.GET /health")
	if crash.After.Status != "exited" || crash.After.ExitCode != 137 {
		t.Fatalf("second health request state = %#v, expected exited/137", crash.After)
	}
	states := []ContainerStateSnapshot{
		container.advance("docker.inspect").After,
		container.advance("docker.inspect").After,
		container.advance("docker.inspect").After,
	}
	expectedStatus := []string{"restarting", "running", "running"}
	expectedHealth := []string{"starting", "starting", "healthy"}
	for i := range expectedStatus {
		if states[i].Status != expectedStatus[i] || states[i].Health != expectedHealth[i] {
			t.Fatalf("inspect transition %d = %#v, expected %s/%s", i+1, states[i], expectedStatus[i], expectedHealth[i])
		}
	}
	if states[2].RestartCount != 1 {
		t.Fatalf("restart count = %d, expected 1", states[2].RestartCount)
	}
}

func TestStopStartPreservesRestartCount(t *testing.T) {
	cookbooks, err := loadCookbooks("")
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := newLedger(filepath.Join(t.TempDir(), "runs"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer testCloseLedger(t, ledger)
	engine := newEngine(cookbooks, 0, nil, ledger)
	container, err := engine.createContainer("nginx-zero", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	container.start()
	container.restart()
	container.advance("docker.inspect")
	if got := container.stateSnapshot().RestartCount; got != 1 {
		t.Fatalf("restart count before stop/start = %d, expected 1", got)
	}
	container.stop(0)
	container.start()
	if got := container.stateSnapshot().RestartCount; got != 1 {
		t.Fatalf("restart count reset after stop/start: %d", got)
	}
}

func TestRestartCreatesNewSyntheticPID(t *testing.T) {
	cookbooks, err := loadCookbooks("")
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := newLedger(filepath.Join(t.TempDir(), "runs"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer testCloseLedger(t, ledger)
	engine := newEngine(cookbooks, 0, nil, ledger)
	container, err := engine.createContainer("redis-zero", "redis:7-alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	container.start()
	container.mu.Lock()
	firstPID := container.Pid
	container.mu.Unlock()
	container.restart()
	container.mu.Lock()
	restartingPID := container.Pid
	container.mu.Unlock()
	if restartingPID != 0 {
		t.Fatalf("PID while restarting = %d, expected 0", restartingPID)
	}
	container.advance("docker.inspect")
	container.mu.Lock()
	secondPID := container.Pid
	container.mu.Unlock()
	if firstPID == 0 || secondPID == 0 || firstPID == secondPID {
		t.Fatalf("PIDs before/after restart = %d/%d, expected different non-zero values", firstPID, secondPID)
	}
}

func TestContainerNameScenarioOverrideWinsOverKind(t *testing.T) {
	cookbooks, err := loadCookbooks("")
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := newLedger(filepath.Join(t.TempDir(), "runs"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer testCloseLedger(t, ledger)
	engine := newEngine(cookbooks, 0, map[string]int{"nginx": 1, "special-nginx": 2}, ledger)
	container, err := engine.createContainer("special-nginx", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if container.Seed != 2 {
		t.Fatalf("container seed = %d, expected exact-name override 2", container.Seed)
	}
}

func TestConcurrentContainerAdvanceIsSafeAndExact(t *testing.T) {
	cookbooks, err := loadCookbooks("")
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := newLedger(filepath.Join(t.TempDir(), "runs"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer testCloseLedger(t, ledger)
	engine := newEngine(cookbooks, 0, nil, ledger)
	container, err := engine.createContainer("nginx-zero", "nginx:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	container.start()

	const goroutines = 32
	const eventsPerGoroutine = 100
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < eventsPerGoroutine; j++ {
				container.advance("test.concurrent")
			}
		}()
	}
	wg.Wait()
	if got, want := container.eventCount("test.concurrent"), goroutines*eventsPerGoroutine; got != want {
		t.Fatalf("concurrent event count = %d, expected %d", got, want)
	}
}

func TestLedgerRecordsContainerScenario(t *testing.T) {
	root := t.TempDir()
	ledger, err := newLedger(filepath.Join(root, "runs"), 0, map[string]int{"nginx-zero": 2})
	if err != nil {
		t.Fatal(err)
	}
	ledger.Log(LedgerEntry{Container: "nginx-zero", Kind: "nginx", Scenario: 2, Channel: "test", Event: "scenario"})
	runDir := ledger.RunDir()
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(runDir, "containers", "nginx-zero.ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var entry map[string]any
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}
	if got := int(entry["scenario"].(float64)); got != 2 {
		t.Fatalf("ledger scenario = %d, expected 2", got)
	}
	metaData, err := os.ReadFile(filepath.Join(runDir, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatal(err)
	}
	overrides := meta["overrides"].(map[string]any)
	if got := int(overrides["nginx-zero"].(float64)); got != 2 {
		t.Fatalf("run metadata override = %d, expected 2", got)
	}
}

func TestRefuseExternalCookbookWithMissingSeeds(t *testing.T) {
	dir := t.TempDir()
	bad := `{"kind":"bad","defaults":{"name":"bad","image":"bad","service_type":"http"},"seeds":{"0":{"description":"x","initial":{}}}}`
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCookbooks(dir); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestContainerStateInvariantsAcrossOperationSequences(t *testing.T) {
	cookbooks, err := loadCookbooks("")
	if err != nil {
		t.Fatal(err)
	}
	cb := cookbooks["nginx"]
	if cb == nil {
		t.Fatal("nginx cookbook is missing")
	}

	operations := []func(*Container){
		func(c *Container) { c.start() },
		func(c *Container) { c.stop(0) },
		func(c *Container) { c.stop(137) },
		func(c *Container) { c.restart() },
		func(c *Container) { c.advance("docker.inspect") },
		func(c *Container) { c.advance("docker.list") },
		func(c *Container) { c.advance("http.GET /health") },
	}

	const sequenceLength = 5
	sequence := make([]int, sequenceLength)
	var visit func(depth int)
	visit = func(depth int) {
		if depth == sequenceLength {
			container := newContainer(containerID("invariant", int64(sequence[0]*10000+sequence[1]*1000+sequence[2]*100+sequence[3]*10+sequence[4]+1)), "nginx-invariant", "nginx:alpine", cb, 2)
			assertContainerStateInvariant(t, container, sequence[:0])
			for index, operation := range sequence {
				operations[operation](container)
				assertContainerStateInvariant(t, container, sequence[:index+1])
			}
			return
		}
		for operation := range operations {
			sequence[depth] = operation
			visit(depth + 1)
		}
	}
	visit(0)
}

func assertContainerStateInvariant(t *testing.T, container *Container, operations []int) {
	t.Helper()
	container.mu.Lock()
	defer container.mu.Unlock()

	fail := func(format string, args ...any) {
		t.Helper()
		allArgs := []any{append([]int(nil), operations...), container.stateSnapshotLocked()}
		allArgs = append(allArgs, args...)
		t.Fatalf("operations=%v state=%+v: "+format, allArgs...)
	}
	if container.RestartCount < 0 {
		fail("negative restart count")
	}
	switch container.Status {
	case "created", "exited", "dead":
		if container.Running || container.Restarting || container.Pid != 0 {
			fail("non-running status has running/restarting/PID set")
		}
	case "restarting":
		if !container.Running || !container.Restarting || container.Pid != 0 {
			fail("restarting state is internally inconsistent")
		}
	case "running":
		if !container.Running || container.Restarting || container.Pid <= 0 {
			fail("running state is internally inconsistent")
		}
	default:
		fail("unknown status %q", container.Status)
	}
	if container.Health.Status != "" && container.Health.Status != "starting" && container.Health.Status != "healthy" && container.Health.Status != "unhealthy" {
		fail("unknown health %q", container.Health.Status)
	}
}
