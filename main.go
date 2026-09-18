package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

const (
	exitOK      = 0
	exitConfig  = 2
	exitRuntime = 3
)

type options struct {
	socketPath         string
	seed               int
	scenarioFlag       string
	cookbookDir        string
	ledgerDir          string
	nginxAddr          string
	redisAddr          string
	containerEndpoints string
	socketMode         string
	bootstrap          bool
	strict             bool
	check              bool
	showVersion        bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		return exitConfig
	}
	if opts.showVersion {
		_, _ = fmt.Fprintf(stdout, "docker-zero %s\n", version)
		return exitOK
	}

	endpointMode, err := normalizeEndpointMode(opts.containerEndpoints)
	if err != nil {
		printFailure(stderr, "CONFIG_ERROR", err)
		return exitConfig
	}
	overrides, err := parseScenarioOverrides(opts.scenarioFlag)
	if err != nil {
		printFailure(stderr, "CONFIG_ERROR", err)
		return exitConfig
	}
	cookbooks, err := loadCookbooks(opts.cookbookDir)
	if err != nil {
		printFailure(stderr, "CONFIG_ERROR", err)
		return exitConfig
	}
	if err := applyListenAddresses(cookbooks, opts.nginxAddr, opts.redisAddr); err != nil {
		printFailure(stderr, "CONFIG_ERROR", err)
		return exitConfig
	}
	if err := validateScenarioOverrides(cookbooks, overrides); err != nil {
		printFailure(stderr, "CONFIG_ERROR", err)
		return exitConfig
	}
	mode, err := parseSocketMode(opts.socketMode)
	if err != nil {
		printFailure(stderr, "CONFIG_ERROR", err)
		return exitConfig
	}
	if err := validatePathArguments(opts.socketPath, opts.ledgerDir); err != nil {
		printFailure(stderr, "CONFIG_ERROR", err)
		return exitConfig
	}

	if opts.check {
		warnings, err := checkConfiguration(opts, cookbooks, endpointMode, mode)
		if err != nil {
			printFailure(stderr, "CHECK_FAILED", err)
			return exitConfig
		}
		printCheckSummary(stdout, opts, cookbooks, overrides, endpointMode, mode, warnings)
		return exitOK
	}

	return serve(opts, cookbooks, overrides, endpointMode, mode, stdout, stderr)
}

func parseOptions(args []string, stderr io.Writer) (options, error) {
	var opts options
	flags := flag.NewFlagSet("docker-zero", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.socketPath, "socket", "/tmp/docker-zero.sock", "Docker-compatible Unix socket")
	flags.IntVar(&opts.seed, "seed", 0, "global cookbook seed/scenario (0, 1, or 2)")
	flags.StringVar(&opts.scenarioFlag, "scenario", "", "per-kind or per-container overrides, e.g. nginx=2,redis-zero=1")
	flags.StringVar(&opts.cookbookDir, "cookbook-dir", "", "external cookbook directory; embedded defaults are used when empty")
	flags.StringVar(&opts.ledgerDir, "ledger-dir", "./runs", "directory for per-run and per-container ledgers")
	flags.StringVar(&opts.nginxAddr, "nginx-address", "127.0.0.1:18080", "bootstrap fake nginx published listen address")
	flags.StringVar(&opts.redisAddr, "redis-address", "127.0.0.1:16379", "bootstrap fake Redis published listen address")
	flags.StringVar(&opts.containerEndpoints, "container-endpoints", "auto", "bind cookbook container IP:port endpoints: auto, on, or off")
	flags.StringVar(&opts.socketMode, "socket-mode", "0660", "Unix socket mode in octal")
	flags.BoolVar(&opts.strict, "strict", false, "reject container settings whose behavior is not simulated (metadata-only fields)")
	flags.BoolVar(&opts.bootstrap, "bootstrap", false, "pre-create and start nginx-zero and redis-zero")
	flags.BoolVar(&opts.check, "check", false, "validate cookbooks, paths, and listener availability, then exit")
	flags.BoolVar(&opts.showVersion, "version", false, "print version and exit")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 {
		err := fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
		printFailure(stderr, "CLI_ERROR", err)
		return options{}, err
	}
	if opts.seed < 0 || opts.seed > 2 {
		err := fmt.Errorf("seed must be 0, 1, or 2; got %d", opts.seed)
		printFailure(stderr, "CLI_ERROR", err)
		return options{}, err
	}
	return opts, nil
}

func serve(opts options, cookbooks map[string]*Cookbook, overrides map[string]int, endpointMode string, socketMode os.FileMode, stdout, stderr io.Writer) int {
	ledger, err := newLedger(opts.ledgerDir, opts.seed, overrides)
	if err != nil {
		printFailure(stderr, "STARTUP_ERROR", fmt.Errorf("create ledger under %s: %w", opts.ledgerDir, err))
		return exitRuntime
	}
	engine := newEngine(cookbooks, opts.seed, overrides, ledger)
	engine.strict = opts.strict
	engine.endpointMode = endpointMode

	if opts.bootstrap {
		for _, kind := range sortedCookbookKinds(cookbooks) {
			cb := cookbooks[kind]
			container, createErr := engine.createContainer(cb.Defaults.Name, cb.Defaults.Image, map[string]string{"docker-zero.bootstrap": "true"}, nil, nil)
			if createErr != nil {
				_ = ledger.Close()
				printFailure(stderr, "STARTUP_ERROR", fmt.Errorf("bootstrap %s: %w", kind, createErr))
				return exitRuntime
			}
			container.setPortBindings(map[string][]PortBinding{
				fmt.Sprintf("%d/tcp", cb.Defaults.ContainerPort): {{HostIP: cb.Defaults.HostIP, RequestedHostPort: cb.Defaults.HostPort, HostPort: cb.Defaults.HostPort, Protocol: "tcp", ContainerPort: cb.Defaults.ContainerPort}},
			})
			if networkErr := engine.configureContainerNetworks(container, "bridge", nil); networkErr != nil {
				_ = ledger.Close()
				printFailure(stderr, "STARTUP_ERROR", fmt.Errorf("bootstrap %s network: %w", kind, networkErr))
				return exitRuntime
			}
			before, _ := container.start()
			if runtimeErr := engine.startContainerRuntime(container); runtimeErr != nil {
				_ = ledger.Close()
				printFailure(stderr, "STARTUP_ERROR", fmt.Errorf("bootstrap %s runtime: %w", kind, runtimeErr))
				return exitRuntime
			}
			advance := container.advance("docker.start")
			advance.Before = before
			engine.logContainerEvent(container, "container.start.bootstrap", advance, nil, nil)
		}
	}

	listener, err := listenUnixSocket(opts.socketPath, socketMode)
	if err != nil {
		_ = engine.closeRuntimes(context.Background())
		_ = ledger.Close()
		printFailure(stderr, "STARTUP_ERROR", fmt.Errorf("listen on Docker socket %s: %w", opts.socketPath, err))
		return exitRuntime
	}

	dockerServer := &http.Server{Handler: &DockerAPI{engine: engine}, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	runtimeErrors := make(chan error, 16)
	go func() {
		if err := dockerServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			reportRuntimeError(runtimeErrors, fmt.Errorf("docker API server: %w", err))
		}
	}()
	forwardRuntimeErrors(runtimeErrors, "ledger", ledger.Errors())
	forwardRuntimeErrors(runtimeErrors, "engine", engine.Errors())

	printStartupSummary(stdout, opts, overrides, endpointMode, ledger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var runtimeErr error
	select {
	case <-ctx.Done():
	case runtimeErr = <-runtimeErrors:
		printFailure(stderr, "INTERNAL_RUNTIME_ERROR", runtimeErr)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdownErrors := []error{
		dockerServer.Shutdown(shutdownCtx),
		engine.closeRuntimes(shutdownCtx),
		listener.Close(),
		os.Remove(opts.socketPath),
		ledger.Close(),
	}
	for _, closeErr := range shutdownErrors {
		if closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) && !errors.Is(closeErr, net.ErrClosed) && !os.IsNotExist(closeErr) && runtimeErr == nil {
			runtimeErr = closeErr
		}
	}
	if runtimeErr != nil {
		return exitRuntime
	}
	return exitOK
}

func printStartupSummary(stdout io.Writer, opts options, overrides map[string]int, endpointMode string, ledger *Ledger) {
	_, _ = fmt.Fprintf(stdout, "docker-zero %s READY\n", version)
	_, _ = fmt.Fprintf(stdout, "  Docker socket     : %s\n", opts.socketPath)
	_, _ = fmt.Fprintf(stdout, "  Global seed       : %d\n", opts.seed)
	if len(overrides) > 0 {
		_, _ = fmt.Fprintf(stdout, "  Overrides         : %s\n", formatOverrides(overrides))
	}
	_, _ = fmt.Fprintf(stdout, "  Container endpoints: %s\n", endpointMode)
	_, _ = fmt.Fprintf(stdout, "  Published ports   : Compose/Docker-driven; conflicts are enforced at container start\n")
	if opts.bootstrap {
		_, _ = fmt.Fprintf(stdout, "  Bootstrap nginx   : %s\n", opts.nginxAddr)
		_, _ = fmt.Fprintf(stdout, "  Bootstrap redis   : %s\n", opts.redisAddr)
	}
	_, _ = fmt.Fprintf(stdout, "  Ledger            : %s\n", ledger.RunDir())
	_, _ = fmt.Fprintf(stdout, "  Docker host       : DOCKER_HOST=unix://%s\n", opts.socketPath)
}

func printCheckSummary(stdout io.Writer, opts options, cookbooks map[string]*Cookbook, overrides map[string]int, endpointMode string, mode os.FileMode, warnings []string) {
	_ = cookbooks
	_, _ = fmt.Fprintf(stdout, "docker-zero %s CHECK OK\n", version)
	_, _ = fmt.Fprintf(stdout, "  Cookbooks         : nginx, redis\n")
	_, _ = fmt.Fprintf(stdout, "  Seeds             : 0, 1, 2\n")
	_, _ = fmt.Fprintf(stdout, "  Docker socket     : %s (%#o)\n", opts.socketPath, mode.Perm())
	_, _ = fmt.Fprintf(stdout, "  Published ports   : Docker/Compose-driven; allocated and conflict-checked on start\n")
	_, _ = fmt.Fprintf(stdout, "  Container IPs     : allocated independently per virtual network\n")
	_, _ = fmt.Fprintf(stdout, "  Container mode    : %s\n", endpointMode)
	if opts.bootstrap {
		_, _ = fmt.Fprintf(stdout, "  Bootstrap nginx   : %s\n", opts.nginxAddr)
		_, _ = fmt.Fprintf(stdout, "  Bootstrap redis   : %s\n", opts.redisAddr)
	}
	if len(overrides) > 0 {
		_, _ = fmt.Fprintf(stdout, "  Overrides         : %s\n", formatOverrides(overrides))
	}
	for _, warning := range warnings {
		_, _ = fmt.Fprintf(stdout, "  WARNING           : %s\n", warning)
	}
}

func checkConfiguration(opts options, cookbooks map[string]*Cookbook, endpointMode string, mode os.FileMode) ([]string, error) {
	if err := checkWritableDirectory(opts.ledgerDir, "ledger directory"); err != nil {
		return nil, err
	}
	if err := checkSocketPath(opts.socketPath, mode); err != nil {
		return nil, err
	}
	warnings := make([]string, 0)
	if !opts.bootstrap {
		return warnings, nil
	}
	addresses := []struct{ name, address string }{{"nginx bootstrap published endpoint", opts.nginxAddr}, {"redis bootstrap published endpoint", opts.redisAddr}}
	listeners := make([]net.Listener, 0, len(addresses))
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	for _, item := range addresses {
		listener, err := net.Listen("tcp", item.address)
		if err != nil {
			return nil, fmt.Errorf("%s %s cannot be bound: %w", item.name, item.address, err)
		}
		listeners = append(listeners, listener)
	}
	return warnings, nil
}

func checkWritableDirectory(path, description string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("create %s %s: %w", description, path, err)
	}
	file, err := os.CreateTemp(path, ".docker-zero-check-*")
	if err != nil {
		return fmt.Errorf("write to %s %s: %w", description, path, err)
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		return fmt.Errorf("close write probe %s: %w", name, err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("remove write probe %s: %w", name, err)
	}
	return nil
}

func checkSocketPath(path string, mode os.FileMode) error {
	listener, err := listenUnixSocket(path, mode)
	if err != nil {
		return fmt.Errorf("create Docker socket probe %s: %w", path, err)
	}
	if err := listener.Close(); err != nil {
		return fmt.Errorf("close Docker socket probe %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove Docker socket probe %s: %w", path, err)
	}
	return nil
}

func validatePathArguments(socketPath, ledgerDir string) error {
	if strings.TrimSpace(socketPath) == "" {
		return fmt.Errorf("--socket cannot be empty")
	}
	if strings.IndexByte(socketPath, 0) >= 0 {
		return fmt.Errorf("--socket contains a NUL byte")
	}
	if strings.TrimSpace(ledgerDir) == "" {
		return fmt.Errorf("--ledger-dir cannot be empty")
	}
	if info, err := os.Stat(ledgerDir); err == nil && !info.IsDir() {
		return fmt.Errorf("--ledger-dir %s is not a directory", ledgerDir)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect --ledger-dir %s: %w", ledgerDir, err)
	}
	return nil
}

func parseSocketMode(value string) (os.FileMode, error) {
	mode, err := strconv.ParseUint(value, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid socket mode %q: expected octal such as 0660", value)
	}
	if mode > 0o777 {
		return 0, fmt.Errorf("invalid socket mode %q: permissions must be between 0000 and 0777", value)
	}
	return os.FileMode(mode), nil
}

func parseScenarioOverrides(value string) (map[string]int, error) {
	result := make(map[string]int)
	value = strings.TrimSpace(value)
	if value == "" {
		return result, nil
	}
	for _, part := range strings.Split(value, ",") {
		pieces := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(pieces) != 2 || strings.TrimSpace(pieces[0]) == "" {
			return nil, fmt.Errorf("invalid scenario override %q; expected kind-or-container=seed", part)
		}
		seed, err := strconv.Atoi(strings.TrimSpace(pieces[1]))
		if err != nil || seed < 0 || seed > 2 {
			return nil, fmt.Errorf("invalid seed in override %q; expected 0, 1, or 2", part)
		}
		key := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(pieces[0]), "/"))
		if _, duplicate := result[key]; duplicate {
			return nil, fmt.Errorf("duplicate scenario override for %q", key)
		}
		result[key] = seed
	}
	return result, nil
}

func validateScenarioOverrides(cookbooks map[string]*Cookbook, overrides map[string]int) error {
	known := make(map[string]*Cookbook)
	for kind, cb := range cookbooks {
		known[strings.ToLower(kind)] = cb
		known[strings.ToLower(cb.Defaults.Name)] = cb
	}
	for key, value := range overrides {
		cb, ok := known[key]
		if !ok {
			continue
		}
		if _, ok := cb.Seeds[strconv.Itoa(value)]; !ok {
			return fmt.Errorf("cookbook %s has no seed %d", cb.Kind, value)
		}
	}
	return nil
}

func formatOverrides(overrides map[string]int) string {
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, overrides[key]))
	}
	return strings.Join(parts, ",")
}

func normalizeEndpointMode(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "auto", "on", "off":
		return value, nil
	default:
		return "", fmt.Errorf("container-endpoints must be auto, on, or off; got %q", value)
	}
}

func applyListenAddresses(cookbooks map[string]*Cookbook, nginxAddress, redisAddress string) error {
	for kind, address := range map[string]string{"nginx": nginxAddress, "redis": redisAddress} {
		host, portText, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("%s address %q: %w", kind, address, err)
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("%s address %q has invalid port; expected 1-65535", kind, address)
		}
		if parsed := net.ParseIP(host); host != "" && parsed == nil {
			return fmt.Errorf("%s address %q uses a non-IP host; use an explicit bind IP", kind, address)
		}
		if cb := cookbooks[kind]; cb != nil {
			cb.Defaults.HostIP = clientHostForBind(host)
			cb.Defaults.HostPort = port
		}
	}
	return nil
}

func clientHostForBind(host string) string {
	switch host {
	case "", "0.0.0.0":
		return "127.0.0.1"
	case "::":
		return "::1"
	default:
		return host
	}
}

func listenUnixSocket(path string, mode os.FileMode) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := prepareUnixSocketPath(path); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, mode); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return listener, nil
}

func prepareUnixSocketPath(path string) error {
	if err := validateUnixSocketPath(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect socket path %s: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket path %s", path)
	}
	connection, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		return fmt.Errorf("docker socket %s is active; stop the existing docker-zero process or choose another --socket", path)
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, syscall.ENOENT) {
		return fmt.Errorf("cannot verify whether existing Docker socket %s is stale: %w", path, dialErr)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale Docker socket %s: %w", path, err)
	}
	return nil
}

func validateUnixSocketPath(path string) error {
	const maxUnixSocketPathBytes = 107
	if path == "" {
		return errors.New("docker socket path is empty")
	}
	if len([]byte(path)) > maxUnixSocketPathBytes {
		return fmt.Errorf("docker socket path is too long for AF_UNIX: %d bytes (max %d): %s", len([]byte(path)), maxUnixSocketPathBytes, path)
	}
	return nil
}

func printFailure(stderr io.Writer, category string, err error) {
	var diagnostic *DiagnosticError
	if errors.As(err, &diagnostic) {
		_, _ = fmt.Fprintln(stderr, diagnostic.Render())
		return
	}
	_, _ = fmt.Fprintf(stderr, "%s: %s\n", category, renderError(err))
}

func reportRuntimeError(target chan<- error, err error) {
	select {
	case target <- err:
	default:
	}
}

func forwardRuntimeErrors(target chan<- error, component string, source <-chan error) {
	if source == nil {
		return
	}
	go func() {
		for err := range source {
			if err != nil {
				reportRuntimeError(target, fmt.Errorf("%s: %w", component, err))
			}
		}
	}()
}
