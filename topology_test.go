package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func createComposeNetworkForTest(t *testing.T, api *DockerAPI, name, project string) {
	t.Helper()
	body := fmt.Sprintf(`{"Name":%q,"Driver":"bridge","Labels":{"com.docker.compose.project":%q}}`, name, project)
	r := httptest.NewRequest(http.MethodPost, "/v1.43/networks/create", strings.NewReader(body))
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("network create status=%d body=%s", w.Code, w.Body.String())
	}
}

func createComposeContainerForTest(t *testing.T, api *DockerAPI, name, image, service, network string, portBindings string, command []string) {
	t.Helper()
	cmd, _ := json.Marshal(command)
	if portBindings == "" {
		portBindings = `{}`
	}
	body := fmt.Sprintf(`{
		"Image":%q,
		"Cmd":%s,
		"Labels":{"com.docker.compose.project":"project","com.docker.compose.service":%q},
		"HostConfig":{"NetworkMode":%q,"PortBindings":%s},
		"NetworkingConfig":{"EndpointsConfig":{%q:{"Aliases":[%q,%q]}}}
	}`, image, cmd, service, network, portBindings, network, service, name)
	r := httptest.NewRequest(http.MethodPost, "/v1.43/containers/create?name="+name, strings.NewReader(body))
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create %s status=%d body=%s", name, w.Code, w.Body.String())
	}
}

func startContainerForTest(t *testing.T, api *DockerAPI, name string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1.43/containers/"+name+"/start", nil)
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)
	return w
}

func inspectContainerForTest(t *testing.T, api *DockerAPI, name string) map[string]any {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1.43/containers/"+name+"/json", nil)
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("inspect %s status=%d body=%s", name, w.Code, w.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func testContainerIP(doc map[string]any, network string) string {
	return doc["NetworkSettings"].(map[string]any)["Networks"].(map[string]any)[network].(map[string]any)["IPAddress"].(string)
}

func TestComposeReplicasGetDistinctIPsAndNetworkScopedDNS(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	engine.endpointMode = "off"
	api := &DockerAPI{engine: engine}
	createComposeNetworkForTest(t, api, "project_default", "project")

	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("project-nginx-%d", i)
		createComposeContainerForTest(t, api, name, "nginx:alpine", "nginx", "project_default", "", nil)
	}

	seen := map[string]bool{}
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("project-nginx-%d", i)
		doc := inspectContainerForTest(t, api, name)
		ip := testContainerIP(doc, "project_default")
		if ip == "" || seen[ip] {
			t.Fatalf("replica %s has duplicate/empty IP %q", name, ip)
		}
		seen[ip] = true
	}
	requester, err := engine.findContainer("project-nginx-1")
	if err != nil {
		t.Fatal(err)
	}
	answers := engine.resolveDNS(requester, "nginx")
	if len(answers) != 3 {
		t.Fatalf("DNS answers=%d want 3: %#v", len(answers), answers)
	}
	for _, answer := range answers {
		if answer.Network != "project_default" {
			t.Fatalf("DNS leaked network: %#v", answer)
		}
		if !seen[answer.IP] {
			t.Fatalf("DNS returned unknown IP: %#v", answer)
		}
	}
}

func TestEphemeralPublishedPortsAreDistinctAndRouteToReplica(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	engine.endpointMode = "on"
	defer testCloseRuntimes(t, engine)
	api := &DockerAPI{engine: engine}
	createComposeNetworkForTest(t, api, "project_default", "project")
	binding := `{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":""}]}`
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("project-nginx-%d", i)
		createComposeContainerForTest(t, api, name, "nginx:alpine", "nginx", "project_default", binding, nil)
		if w := startContainerForTest(t, api, name); w.Code != http.StatusNoContent {
			t.Fatalf("start %s status=%d body=%s", name, w.Code, w.Body.String())
		}
	}

	ports := map[int]bool{}
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("project-nginx-%d", i)
		doc := inspectContainerForTest(t, api, name)
		portDocs := doc["NetworkSettings"].(map[string]any)["Ports"].(map[string]any)["80/tcp"].([]any)
		port, _ := strconv.Atoi(portDocs[0].(map[string]any)["HostPort"].(string))
		if port == 0 || ports[port] {
			t.Fatalf("bad ephemeral port %d for %s", port, name)
		}
		ports[port] = true
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
		if err != nil {
			t.Fatalf("GET %s: %v", name, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if got := resp.Header.Get("X-Docker-Zero-Container"); got != name {
			t.Fatalf("port %d routed to %q want %q", port, got, name)
		}
	}
}

func TestFixedPublishedPortConflictFailsContainerStart(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer testClose(t, occupied)
	port := occupied.Addr().(*net.TCPAddr).Port

	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	engine.endpointMode = "off"
	api := &DockerAPI{engine: engine}
	createComposeNetworkForTest(t, api, "project_default", "project")
	binding := fmt.Sprintf(`{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":%q}]}`, strconv.Itoa(port))
	createComposeContainerForTest(t, api, "project-nginx-1", "nginx:alpine", "nginx", "project_default", binding, nil)
	w := startContainerForTest(t, api, "project-nginx-1")
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "port is already allocated") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRedisReplicationUsesNetworkDNSAndIndependentReplicaState(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	engine.endpointMode = "on"
	defer testCloseRuntimes(t, engine)
	api := &DockerAPI{engine: engine}
	createComposeNetworkForTest(t, api, "project_default", "project")

	createComposeContainerForTest(t, api, "project-redis-master-1", "redis:7-alpine", "redis-master", "project_default", "", nil)
	command := []string{"redis-server", "--replicaof", "redis-master", "6379"}
	createComposeContainerForTest(t, api, "project-redis-replica-1", "redis:7-alpine", "redis-replica", "project_default", "", command)
	createComposeContainerForTest(t, api, "project-redis-replica-2", "redis:7-alpine", "redis-replica", "project_default", "", command)

	// Deliberately start replicas first to verify master arrival repairs links.
	for _, name := range []string{"project-redis-replica-1", "project-redis-replica-2", "project-redis-master-1"} {
		if w := startContainerForTest(t, api, name); w.Code != http.StatusNoContent {
			t.Fatalf("start %s status=%d body=%s", name, w.Code, w.Body.String())
		}
	}
	masterIP := testContainerIP(inspectContainerForTest(t, api, "project-redis-master-1"), "project_default")
	replica1IP := testContainerIP(inspectContainerForTest(t, api, "project-redis-replica-1"), "project_default")
	replica2IP := testContainerIP(inspectContainerForTest(t, api, "project-redis-replica-2"), "project_default")

	if got := redisRoundTripForTest(t, masterIP, "SET", "hello", "world"); !strings.HasPrefix(got, "+OK") {
		t.Fatalf("SET master: %q", got)
	}
	for _, ip := range []string{replica1IP, replica2IP} {
		if got := redisRoundTripForTest(t, ip, "GET", "hello"); !strings.Contains(got, "world") {
			t.Fatalf("replica GET %s: %q", ip, got)
		}
	}
	if got := redisRoundTripForTest(t, replica1IP, "SET", "x", "1"); !strings.HasPrefix(got, "-READONLY") {
		t.Fatalf("replica write: %q", got)
	}
	if got := redisRoundTripForTest(t, replica1IP, "INFO", "replication"); !strings.Contains(got, "role:slave") || !strings.Contains(got, "master_link_status:up") {
		t.Fatalf("replica INFO: %q", got)
	}
	if got := redisRoundTripForTest(t, masterIP, "INFO", "replication"); !strings.Contains(got, "connected_slaves:2") {
		t.Fatalf("master INFO: %q", got)
	}

	replica, _ := engine.findContainer("project-redis-replica-1")
	answers := engine.resolveDNS(replica, "redis-master")
	if len(answers) != 1 || answers[0].IP != masterIP {
		t.Fatalf("replication DNS=%#v master=%s", answers, masterIP)
	}
}

func redisRoundTripForTest(t *testing.T, ip string, args ...string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "6379"), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer testClose(t, conn)
	writer := bufio.NewWriter(conn)
	if _, err := fmt.Fprintf(writer, "*%d\r\n", len(args)); err != nil {
		t.Fatal(err)
	}
	for _, arg := range args {
		if _, err := fmt.Fprintf(writer, "$%d\r\n%s\r\n", len(arg), arg); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	reader := bufio.NewReader(conn)
	prefix, err := reader.ReadByte()
	if err != nil {
		t.Fatal(err)
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if prefix == '$' {
		n, _ := strconv.Atoi(strings.TrimSpace(line))
		if n < 0 {
			return "$-1\r\n"
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(reader, buf); err != nil {
			t.Fatal(err)
		}
		return string(prefix) + line + string(buf)
	}
	return string(prefix) + line
}

func TestDockerNetworkConnectDisconnectUpdatesScopedDNS(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	engine.endpointMode = "off"
	api := &DockerAPI{engine: engine}
	createComposeNetworkForTest(t, api, "project_left", "project")
	createComposeNetworkForTest(t, api, "project_right", "project")

	createComposeContainerForTest(t, api, "project-left-1", "nginx:alpine", "left", "project_left", "", nil)
	createComposeContainerForTest(t, api, "project-right-1", "nginx:alpine", "right", "project_right", "", nil)
	left, _ := engine.findContainer("project-left-1")
	if answers := engine.resolveDNS(left, "right-alias"); len(answers) != 0 {
		t.Fatalf("DNS leaked before network connect: %#v", answers)
	}

	connect := httptest.NewRequest(http.MethodPost, "/v1.43/networks/project_left/connect", strings.NewReader(`{"Container":"project-right-1","EndpointConfig":{"Aliases":["right-alias"]}}`))
	cw := httptest.NewRecorder()
	api.ServeHTTP(cw, connect)
	if cw.Code != http.StatusOK {
		t.Fatalf("network connect status=%d body=%s", cw.Code, cw.Body.String())
	}
	answers := engine.resolveDNS(left, "right-alias")
	if len(answers) != 1 || answers[0].Network != "project_left" || answers[0].Container != "project-right-1" || answers[0].IP == "" {
		t.Fatalf("DNS after connect=%#v", answers)
	}

	disconnect := httptest.NewRequest(http.MethodPost, "/v1.43/networks/project_left/disconnect", strings.NewReader(`{"Container":"project-right-1"}`))
	dw := httptest.NewRecorder()
	api.ServeHTTP(dw, disconnect)
	if dw.Code != http.StatusOK {
		t.Fatalf("network disconnect status=%d body=%s", dw.Code, dw.Body.String())
	}
	if answers := engine.resolveDNS(left, "right-alias"); len(answers) != 0 {
		t.Fatalf("DNS remained after disconnect: %#v", answers)
	}
}

func TestVirtualNetworkAndContainerAddressesAreReusableAfterChurn(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	engine.endpointMode = "off"
	api := &DockerAPI{engine: engine}

	// Repeated Compose down/up cycles must not consume a finite subnet index forever.
	for i := 0; i < 300; i++ {
		name := fmt.Sprintf("churn-net-%d", i)
		network, err := engine.createNetwork(name, "bridge", nil)
		if err != nil {
			t.Fatalf("network cycle %d: %v", i, err)
		}
		if network.Index != 1 {
			t.Fatalf("network cycle %d index=%d want reused index 1", i, network.Index)
		}
		if err := engine.removeNetwork(name); err != nil {
			t.Fatalf("remove network cycle %d: %v", i, err)
		}
	}

	createComposeNetworkForTest(t, api, "project_default", "project")
	for i := 0; i < 400; i++ {
		name := fmt.Sprintf("project-nginx-churn-%d", i)
		createComposeContainerForTest(t, api, name, "nginx:alpine", "nginx", "project_default", "", nil)
		doc := inspectContainerForTest(t, api, name)
		if ip := testContainerIP(doc, "project_default"); ip != "127.20.1.2" {
			t.Fatalf("container cycle %d IP=%s want reused 127.20.1.2", i, ip)
		}
		r := httptest.NewRequest(http.MethodDelete, "/v1.43/containers/"+name, nil)
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("remove container cycle %d status=%d body=%s", i, w.Code, w.Body.String())
		}
	}
}

func TestSameContainerPublishedPortConflictFailsAndRollsBackPartialEphemeralBind(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := occupied.Addr().(*net.TCPAddr).Port
	defer testClose(t, occupied)

	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	engine.endpointMode = "off"
	api := &DockerAPI{engine: engine}
	createComposeNetworkForTest(t, api, "project_default", "project")
	bindings := fmt.Sprintf(`{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":""}],"81/tcp":[{"HostIp":"127.0.0.1","HostPort":%q}]}`, strconv.Itoa(port))
	createComposeContainerForTest(t, api, "project-nginx-1", "nginx:alpine", "nginx", "project_default", bindings, nil)

	w := startContainerForTest(t, api, "project-nginx-1")
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "port is already allocated") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	container, err := engine.findContainer("project-nginx-1")
	if err != nil {
		t.Fatal(err)
	}
	state := container.stateSnapshot()
	if state.Running || state.Status != "exited" || state.ExitCode != 128 {
		t.Fatalf("failed start left impossible state: %#v", state)
	}
	bindingsAfter := container.clonePortBindings()
	if got := bindingsAfter["80/tcp"][0].HostPort; got != 0 {
		t.Fatalf("partial ephemeral bind leaked assigned port %d after failed start", got)
	}
}

func TestSameContainerCannotBindOneHostPortToDifferentContainerPorts(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	engine.endpointMode = "off"
	api := &DockerAPI{engine: engine}
	createComposeNetworkForTest(t, api, "project_default", "project")
	bindings := fmt.Sprintf(`{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":%q}],"81/tcp":[{"HostIp":"127.0.0.1","HostPort":%q}]}`, strconv.Itoa(port), strconv.Itoa(port))
	createComposeContainerForTest(t, api, "project-nginx-1", "nginx:alpine", "nginx", "project_default", bindings, nil)
	w := startContainerForTest(t, api, "project-nginx-1")
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "port is already allocated") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	// The first listener opened before duplicate detection must have been released.
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("failed start leaked host listener %d: %v", port, err)
	}
	_ = listener.Close()
}

func TestEngineShutdownPreventsBackgroundRuntimeRebind(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	engine.endpointMode = "on"
	api := &DockerAPI{engine: engine}
	createComposeNetworkForTest(t, api, "project_default", "project")
	createComposeContainerForTest(t, api, "project-nginx-1", "nginx:alpine", "nginx", "project_default", "", nil)
	if w := startContainerForTest(t, api, "project-nginx-1"); w.Code != http.StatusNoContent {
		t.Fatalf("start status=%d body=%s", w.Code, w.Body.String())
	}
	container, _ := engine.findContainer("project-nginx-1")
	ip := testContainerIP(inspectContainerForTest(t, api, "project-nginx-1"), "project_default")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := engine.closeRuntimes(ctx); err != nil {
		t.Fatal(err)
	}
	// A queued state-sync after shutdown must never reopen a service listener.
	engine.syncRuntimeForState(container)
	connection, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "80"), 150*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		t.Fatalf("runtime rebound after engine shutdown at %s:80", ip)
	}
}

func TestRedisContainerStopClosesEstablishedClientConnections(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	engine.endpointMode = "off"
	defer testCloseRuntimes(t, engine)
	api := &DockerAPI{engine: engine}
	createComposeNetworkForTest(t, api, "project_default", "project")
	binding := `{"6379/tcp":[{"HostIp":"127.0.0.1","HostPort":""}]}`
	createComposeContainerForTest(t, api, "project-redis-1", "redis:7-alpine", "redis", "project_default", binding, nil)
	if w := startContainerForTest(t, api, "project-redis-1"); w.Code != http.StatusNoContent {
		t.Fatalf("start status=%d body=%s", w.Code, w.Body.String())
	}
	doc := inspectContainerForTest(t, api, "project-redis-1")
	portDocs := doc["NetworkSettings"].(map[string]any)["Ports"].(map[string]any)["6379/tcp"].([]any)
	port, _ := strconv.Atoi(portDocs[0].(map[string]any)["HostPort"].(string))
	connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer testClose(t, connection)
	reader := bufio.NewReader(connection)
	if _, err := io.WriteString(connection, "*1\r\n$4\r\nPING\r\n"); err != nil {
		t.Fatal(err)
	}
	if reply, err := reader.ReadString('\n'); err != nil || reply != "+PONG\r\n" {
		t.Fatalf("initial PING reply=%q err=%v", reply, err)
	}

	stop := httptest.NewRequest(http.MethodPost, "/v1.43/containers/project-redis-1/stop", nil)
	w := httptest.NewRecorder()
	api.ServeHTTP(w, stop)
	if w.Code != http.StatusNoContent {
		t.Fatalf("stop status=%d body=%s", w.Code, w.Body.String())
	}
	_ = connection.SetDeadline(time.Now().Add(500 * time.Millisecond))
	_, _ = io.WriteString(connection, "*1\r\n$4\r\nPING\r\n")
	if _, err := reader.ReadString('\n'); err == nil {
		t.Fatal("established Redis connection remained usable after container stop")
	}
}
