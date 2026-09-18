package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"
)

const (
	engineVersion = "26.1.5-docker-zero"
	apiVersion    = "1.43"
	minAPIVersion = "1.24"
)

type DockerAPI struct {
	engine *Engine
}

type dockerResponseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

// Unwrap lets http.ResponseController preserve flushing through the recorder.
func (r *dockerResponseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *dockerResponseRecorder) WriteHeader(status int) {
	if r.status != 0 {
		return
	}
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *dockerResponseRecorder) Write(payload []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(payload)
	r.bytes += n
	return n, err
}

func (api *DockerAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	recorder := &dockerResponseRecorder{ResponseWriter: w}
	w = recorder
	path := stripAPIVersion(r.URL.Path)
	defer func() {
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		api.engine.ledger.Log(LedgerEntry{
			Channel: "docker.http",
			Event:   r.Method + " " + path,
			Request: map[string]any{
				"method": r.Method, "path": r.URL.Path, "normalized_path": path,
				"query": r.URL.RawQuery, "remote": r.RemoteAddr,
			},
			Response: map[string]any{
				"status": status, "bytes": recorder.bytes,
				"duration_us": time.Since(started).Microseconds(),
				"unsupported": recorder.Header().Get("X-Docker-Zero-Unsupported") == "true",
			},
		})
	}()
	defer recoverDockerHTTPHandler(api.engine, w)

	w.Header().Set("Server", "Docker/"+engineVersion)
	w.Header().Set("Api-Version", apiVersion)
	w.Header().Set("Docker-Experimental", "false")
	w.Header().Set("Ostype", "linux")
	w.Header().Set("X-Docker-Zero-Version", version)

	if path == "/_ping" {
		api.handlePing(w, r)
		return
	}
	if path == "/version" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{
			"Platform":      map[string]string{"Name": "Docker Zero"},
			"Components":    []any{map[string]any{"Name": "Engine", "Version": engineVersion, "Details": map[string]string{"ApiVersion": apiVersion, "MinAPIVersion": minAPIVersion, "Os": "linux", "Arch": runtime.GOARCH, "KernelVersion": "mock"}}},
			"Version":       engineVersion,
			"ApiVersion":    apiVersion,
			"MinAPIVersion": minAPIVersion,
			"GitCommit":     "docker-zero",
			"GoVersion":     runtime.Version(),
			"Os":            "linux",
			"Arch":          runtime.GOARCH,
			"KernelVersion": "mock",
			"BuildTime":     formatDockerTime(api.engine.startedAt),
		})
		return
	}
	if path == "/info" && r.Method == http.MethodGet {
		api.handleInfo(w)
		return
	}
	if strings.HasPrefix(path, "/__docker_zero/") || path == "/__docker_zero" {
		api.handleAdmin(w, r, path)
		return
	}
	if path == "/containers/json" && r.Method == http.MethodGet {
		api.handleContainerList(w, r)
		return
	}
	if path == "/containers/create" && r.Method == http.MethodPost {
		api.handleContainerCreate(w, r)
		return
	}
	if strings.HasPrefix(path, "/containers/") {
		api.handleContainerAction(w, r, strings.TrimPrefix(path, "/containers/"))
		return
	}
	if path == "/images/json" && r.Method == http.MethodGet {
		api.handleImageList(w)
		return
	}
	if path == "/images/create" && r.Method == http.MethodPost {
		api.handleImageCreate(w, r)
		return
	}
	if strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json") && r.Method == http.MethodGet {
		name := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
		decoded, _ := url.PathUnescape(name)
		api.handleImageInspect(w, decoded)
		return
	}
	if path == "/networks" && r.Method == http.MethodGet {
		api.handleNetworkList(w, r)
		return
	}
	if path == "/networks/create" && r.Method == http.MethodPost {
		api.handleNetworkCreate(w, r)
		return
	}
	if strings.HasPrefix(path, "/networks/") {
		api.handleNetworkAction(w, r, strings.TrimPrefix(path, "/networks/"))
		return
	}
	if path == "/volumes" && r.Method == http.MethodGet {
		api.handleVolumeList(w, r)
		return
	}
	if path == "/volumes/create" && r.Method == http.MethodPost {
		api.handleVolumeCreate(w, r)
		return
	}
	if strings.HasPrefix(path, "/volumes/") {
		api.handleVolumeAction(w, r, strings.TrimPrefix(path, "/volumes/"))
		return
	}
	if strings.HasPrefix(path, "/exec/") {
		api.handleExecAction(w, r, strings.TrimPrefix(path, "/exec/"))
		return
	}
	if (path == "/events" || path == "/system/df") && r.Method == http.MethodGet {
		writeUnsupportedDockerError(w, http.StatusNotImplemented, fmt.Sprintf("unsupported Docker API request: %s %s", r.Method, path))
		return
	}

	writeUnsupportedDockerError(w, http.StatusNotFound, fmt.Sprintf("unsupported Docker API request: %s %s", r.Method, path))
}

func (api *DockerAPI) handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Builder-Version", "2")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, "OK")
	}
}

func (api *DockerAPI) handleInfo(w http.ResponseWriter) {
	containers := api.engine.listContainers(true)
	running, paused, stopped := 0, 0, 0
	for _, c := range containers {
		state := c.stateSnapshot()
		if state.Paused {
			paused++
		}
		if c.isRunning() {
			running++
		} else {
			stopped++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ID":                 "DOCKERZERO000000000000000000000000000000000000000000000000000",
		"Containers":         len(containers),
		"ContainersRunning":  running,
		"ContainersPaused":   paused,
		"ContainersStopped":  stopped,
		"Images":             len(api.engine.cookbooks),
		"Driver":             "docker-zero",
		"MemoryLimit":        false,
		"SwapLimit":          false,
		"CpuCfsPeriod":       false,
		"CpuCfsQuota":        false,
		"CPUShares":          false,
		"CPUSet":             false,
		"PidsLimit":          false,
		"IPv4Forwarding":     false,
		"BridgeNfIptables":   false,
		"BridgeNfIP6tables":  false,
		"Debug":              false,
		"NFd":                0,
		"NGoroutines":        1,
		"LoggingDriver":      "json-file",
		"CgroupDriver":       "none",
		"NEventsListener":    0,
		"KernelVersion":      "mock",
		"OperatingSystem":    "Docker Zero",
		"OSType":             "linux",
		"Architecture":       dockerMachineArchitecture(),
		"NCPU":               1,
		"MemTotal":           1024 * 1024 * 1024,
		"DockerRootDir":      "/var/lib/docker-zero",
		"Name":               "docker-zero",
		"ServerVersion":      engineVersion,
		"ClusterStore":       "",
		"ClusterAdvertise":   "",
		"DefaultRuntime":     "runc",
		"LiveRestoreEnabled": false,
		"InitBinary":         "docker-init",
		"SecurityOptions":    []string{},
		"Warnings":           []string{"docker-zero is a simulator; no containers are executed"},
	})
}

func (api *DockerAPI) handleAdmin(w http.ResponseWriter, r *http.Request, path string) {
	switch {
	case (path == "/__docker_zero" || path == "/__docker_zero/state") && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, api.engine.stateDocument())
	case path == "/__docker_zero/cookbooks" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, api.engine.cookbooks)
	case path == "/__docker_zero/dns" && r.Method == http.MethodGet:
		from := r.URL.Query().Get("from")
		name := r.URL.Query().Get("name")
		container, err := api.engine.findContainer(from)
		if err != nil {
			writeDockerError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"from": container.Name, "name": name, "answers": api.engine.resolveDNS(container, name)})
	default:
		writeDockerError(w, http.StatusNotFound, "unknown docker-zero admin endpoint")
	}
}
