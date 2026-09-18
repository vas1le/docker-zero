package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveIsExplicitlyUnsupportedAndLedgered(t *testing.T) {
	engine, ledger := testEngine(t, 0)
	runDir := ledger.RunDir()
	c, err := engine.createContainer("archive-contract", "redis:alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := c.stateSnapshot()
	for _, method := range []string{http.MethodHead, http.MethodGet, http.MethodPut} {
		response := httptest.NewRecorder()
		(&DockerAPI{engine: engine}).ServeHTTP(response, httptest.NewRequest(method, "/containers/archive-contract/archive?path=/missing", strings.NewReader("not a tar archive")))
		if response.Code != http.StatusNotImplemented || response.Header().Get("X-Docker-Zero-Unsupported") != "true" {
			t.Errorf("%s: status=%d unsupported=%q", method, response.Code, response.Header().Get("X-Docker-Zero-Unsupported"))
		}
	}
	if c.stateSnapshot() != before {
		t.Error("unsupported archive changed lifecycle state")
	}
	testCloseLedger(t, ledger)
	data, err := os.ReadFile(filepath.Join(runDir, "global.ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), `"unsupported":true`) != 3 {
		t.Fatalf("unsupported operations not all recorded: %s", data)
	}
}
