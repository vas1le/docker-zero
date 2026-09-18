package main

import (
	"archive/tar"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestArchiveOperationsFailClosed(t *testing.T) {
	e, ledger := testEngine(t, 0)
	defer testCloseLedger(t, ledger)
	c, err := e.createContainer("archive-test", "nginx", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var valid bytes.Buffer
	tw := tar.NewWriter(&valid)
	if err := tw.WriteHeader(&tar.Header{Name: "config.txt", Mode: 0600, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, "data"); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodPut, http.MethodHead, http.MethodGet} {
		for _, body := range []string{"not-a-tar", valid.String()} {
			r := httptest.NewRecorder()
			(&DockerAPI{engine: e}).ServeHTTP(r, httptest.NewRequest(method, "/containers/"+c.ID+"/archive?path=/missing", strings.NewReader(body)))
			if r.Code != http.StatusNotImplemented || r.Header().Get("X-Docker-Zero-Unsupported") != "true" {
				t.Fatalf("archive %s silently succeeded: %d", method, r.Code)
			}
			if c.eventCount("docker.archive") != 0 {
				t.Fatal("unsupported archive advanced the scenario")
			}
		}
	}
}
