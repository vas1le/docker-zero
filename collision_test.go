package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func dockerRequestForTest(api *DockerAPI, method, path, body string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, reader)
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)
	return w
}

func TestDeleteNetworkWithRunningContainerFailsWithoutMutatingTopology(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	engine.endpointMode = "on"
	defer testCloseRuntimes(t, engine)
	api := &DockerAPI{engine: engine}
	createComposeNetworkForTest(t, api, "project_default", "project")
	createComposeContainerForTest(t, api, "project-nginx-1", "nginx:alpine", "nginx", "project_default", "", nil)
	if w := startContainerForTest(t, api, "project-nginx-1"); w.Code != http.StatusNoContent {
		t.Fatalf("start status=%d body=%s", w.Code, w.Body.String())
	}

	docBefore := inspectContainerForTest(t, api, "project-nginx-1")
	ipBefore := testContainerIP(docBefore, "project_default")
	client := &http.Client{Timeout: 2 * time.Second}
	res, err := client.Get("http://" + ipBefore + "/health")
	if err != nil {
		t.Fatalf("endpoint before illegal network delete: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("endpoint before delete status=%d", res.StatusCode)
	}

	w := dockerRequestForTest(api, http.MethodDelete, "/v1.43/networks/project_default", "")
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "has active endpoints") {
		t.Fatalf("network delete status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "project-nginx-1") {
		t.Fatalf("active-endpoint diagnostic did not identify attached container: %s", w.Body.String())
	}

	if _, err := engine.findNetwork("project_default"); err != nil {
		t.Fatalf("failed network delete removed network: %v", err)
	}
	docAfter := inspectContainerForTest(t, api, "project-nginx-1")
	ipAfter := testContainerIP(docAfter, "project_default")
	if ipAfter != ipBefore {
		t.Fatalf("failed network delete changed endpoint IP: before=%s after=%s", ipBefore, ipAfter)
	}
	state := docAfter["State"].(map[string]any)
	if running, _ := state["Running"].(bool); !running {
		t.Fatalf("failed network delete stopped running container: %#v", state)
	}
	res, err = client.Get("http://" + ipAfter + "/health")
	if err != nil {
		t.Fatalf("endpoint after illegal network delete: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("endpoint after delete status=%d", res.StatusCode)
	}
}

func TestDeleteNetworkWithStoppedAttachedContainerStillFails(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	engine.endpointMode = "off"
	api := &DockerAPI{engine: engine}
	createComposeNetworkForTest(t, api, "project_default", "project")
	createComposeContainerForTest(t, api, "project-nginx-1", "nginx:alpine", "nginx", "project_default", "", nil)

	w := dockerRequestForTest(api, http.MethodDelete, "/v1.43/networks/project_default", "")
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "has active endpoints") {
		t.Fatalf("network delete status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := engine.findNetwork("project_default"); err != nil {
		t.Fatalf("stopped endpoint did not protect network: %v", err)
	}

	remove := dockerRequestForTest(api, http.MethodDelete, "/v1.43/containers/project-nginx-1", "")
	if remove.Code != http.StatusNoContent {
		t.Fatalf("container remove status=%d body=%s", remove.Code, remove.Body.String())
	}
	w = dockerRequestForTest(api, http.MethodDelete, "/v1.43/networks/project_default", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("network delete after endpoint cleanup status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPredefinedBridgeNetworkDeleteIsForbidden(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	api := &DockerAPI{engine: engine}
	w := dockerRequestForTest(api, http.MethodDelete, "/v1.43/networks/bridge", "")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "pre-defined") {
		t.Fatalf("bridge delete status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestContainerNameCollisionReturns409AndPreservesOriginal(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	engine.endpointMode = "off"
	api := &DockerAPI{engine: engine}
	createComposeNetworkForTest(t, api, "project_default", "project")
	createComposeContainerForTest(t, api, "collision-nginx", "nginx:alpine", "nginx", "project_default", "", nil)
	original, err := engine.findContainer("collision-nginx")
	if err != nil {
		t.Fatal(err)
	}

	body := `{"Image":"nginx:alpine","Labels":{"com.docker.compose.service":"nginx"},"HostConfig":{"NetworkMode":"project_default"},"NetworkingConfig":{"EndpointsConfig":{"project_default":{}}}}`
	w := dockerRequestForTest(api, http.MethodPost, "/v1.43/containers/create?name=collision-nginx", body)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "already in use") {
		t.Fatalf("duplicate container status=%d body=%s", w.Code, w.Body.String())
	}
	after, err := engine.findContainer("collision-nginx")
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != original.ID {
		t.Fatalf("name collision replaced original container: before=%s after=%s", original.ID, after.ID)
	}
}

func TestNetworkNameCollisionReturns409AndPreservesOriginal(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	api := &DockerAPI{engine: engine}
	createComposeNetworkForTest(t, api, "collision_default", "project-a")
	original, err := engine.findNetwork("collision_default")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"Name":"collision_default","Driver":"bridge","CheckDuplicate":true,"Labels":{"com.docker.compose.project":"project-b"}}`
	w := dockerRequestForTest(api, http.MethodPost, "/v1.43/networks/create", body)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "already exists") {
		t.Fatalf("duplicate network status=%d body=%s", w.Code, w.Body.String())
	}
	after, err := engine.findNetwork("collision_default")
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != original.ID || after.Labels["com.docker.compose.project"] != "project-a" {
		t.Fatalf("network collision replaced original: before=%#v after=%#v", original, after)
	}
}

func TestDuplicateNetworkConnectIsRejectedWithoutChangingEndpoint(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	engine.endpointMode = "on"
	defer testCloseRuntimes(t, engine)
	api := &DockerAPI{engine: engine}
	createComposeNetworkForTest(t, api, "project_default", "project")
	createComposeContainerForTest(t, api, "project-nginx-1", "nginx:alpine", "nginx", "project_default", "", nil)
	if w := startContainerForTest(t, api, "project-nginx-1"); w.Code != http.StatusNoContent {
		t.Fatalf("start status=%d body=%s", w.Code, w.Body.String())
	}
	before := inspectContainerForTest(t, api, "project-nginx-1")
	ipBefore := testContainerIP(before, "project_default")

	body := `{"Container":"project-nginx-1","EndpointConfig":{"Aliases":["second-alias"]}}`
	w := dockerRequestForTest(api, http.MethodPost, "/v1.43/networks/project_default/connect", body)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "already exists in network") {
		t.Fatalf("duplicate connect status=%d body=%s", w.Code, w.Body.String())
	}
	after := inspectContainerForTest(t, api, "project-nginx-1")
	if ipAfter := testContainerIP(after, "project_default"); ipAfter != ipBefore {
		t.Fatalf("duplicate connect changed endpoint IP: %s -> %s", ipBefore, ipAfter)
	}
	container, _ := engine.findContainer("project-nginx-1")
	if answers := engine.resolveDNS(container, "second-alias"); len(answers) != 0 {
		t.Fatalf("rejected duplicate connect leaked new alias: %#v", answers)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	res, err := client.Get("http://" + ipBefore + "/health")
	if err != nil {
		t.Fatalf("duplicate connect interrupted running endpoint: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("duplicate connect changed endpoint status=%d", res.StatusCode)
	}
}

func TestNetworkDeleteCollisionDiagnosticIsDeterministic(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	engine.endpointMode = "off"
	api := &DockerAPI{engine: engine}
	createComposeNetworkForTest(t, api, "project_default", "project")
	for _, name := range []string{"project-nginx-2", "project-nginx-1"} {
		createComposeContainerForTest(t, api, name, "nginx:alpine", "nginx", "project_default", "", nil)
	}
	w := dockerRequestForTest(api, http.MethodDelete, "/v1.43/networks/project_default", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	msg, _ := payload["message"].(string)
	want := "project-nginx-1, project-nginx-2"
	if !strings.Contains(msg, want) {
		t.Fatalf("attached endpoint diagnostic is not deterministic: got=%q want substring=%q", msg, want)
	}
}

func TestCollisionMessagesRemainDockerShaped(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	api := &DockerAPI{engine: engine}
	createComposeNetworkForTest(t, api, "collision_default", "project")
	w := dockerRequestForTest(api, http.MethodPost, "/v1.43/networks/create", `{"Name":"collision_default","Driver":"bridge"}`)
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("collision response content-type=%q", ct)
	}
	var doc struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || doc.Message == "" {
		t.Fatalf("collision response not Docker-shaped: err=%v body=%s", err, w.Body.String())
	}
	if !strings.Contains(doc.Message, fmt.Sprintf("network with name %s", "collision_default")) {
		t.Fatalf("unexpected collision message: %q", doc.Message)
	}
}
