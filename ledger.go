package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type LedgerEntry struct {
	Seq        uint64                  `json:"seq"`
	Timestamp  string                  `json:"timestamp"`
	Seed       int                     `json:"seed"`
	Scenario   int                     `json:"scenario"`
	Container  string                  `json:"container,omitempty"`
	Kind       string                  `json:"kind,omitempty"`
	Channel    string                  `json:"channel"`
	Event      string                  `json:"event"`
	Count      int                     `json:"count,omitempty"`
	Request    any                     `json:"request,omitempty"`
	Response   any                     `json:"response,omitempty"`
	Transition string                  `json:"transition,omitempty"`
	Before     *ContainerStateSnapshot `json:"before,omitempty"`
	After      *ContainerStateSnapshot `json:"after,omitempty"`
	Note       string                  `json:"note,omitempty"`
}

type Ledger struct {
	mu       sync.Mutex
	seq      uint64
	seed     int
	runDir   string
	global   *os.File
	files    map[string]*os.File
	firstErr error
	errors   chan error
	closed   bool
}

func newLedger(baseDir string, seed int, overrides ...map[string]int) (*Ledger, error) {
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	runDir := filepath.Join(baseDir, fmt.Sprintf("seed-%d-%s", seed, stamp))
	if err := os.MkdirAll(filepath.Join(runDir, "containers"), 0o755); err != nil {
		return nil, err
	}
	global, err := os.OpenFile(filepath.Join(runDir, "global.ledger.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	selectedOverrides := map[string]int{}
	if len(overrides) > 0 && overrides[0] != nil {
		for key, value := range overrides[0] {
			selectedOverrides[key] = value
		}
	}
	meta := map[string]any{
		"name":       "docker-zero",
		"version":    version,
		"seed":       seed,
		"overrides":  selectedOverrides,
		"started_at": time.Now().UTC().Format(time.RFC3339Nano),
		"run_dir":    runDir,
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		_ = global.Close()
		return nil, fmt.Errorf("marshal run metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), append(data, '\n'), 0o644); err != nil {
		_ = global.Close()
		return nil, err
	}
	return &Ledger{
		seed: seed, runDir: runDir, global: global, files: make(map[string]*os.File),
		errors: make(chan error, 1),
	}, nil
}

func (l *Ledger) RunDir() string       { return l.runDir }
func (l *Ledger) Errors() <-chan error { return l.errors }

func (l *Ledger) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.firstErr
}

func (l *Ledger) Log(entry LedgerEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.firstErr != nil {
		return
	}
	l.seq++
	entry.Seq = l.seq
	entry.Seed = l.seed
	entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(entry)
	if err != nil {
		l.failLocked(fmt.Errorf("marshal ledger entry %d: %w", entry.Seq, err))
		return
	}
	data = append(data, '\n')
	if err := writeAndSync(l.global, data); err != nil {
		l.failLocked(fmt.Errorf("write global ledger entry %d: %w", entry.Seq, err))
		return
	}
	if entry.Container == "" {
		return
	}
	file, ok := l.files[entry.Container]
	if !ok {
		path := filepath.Join(l.runDir, "containers", sanitizeFilename(entry.Container)+".ledger.jsonl")
		file, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			l.failLocked(fmt.Errorf("open container ledger %s: %w", path, err))
			return
		}
		l.files[entry.Container] = file
	}
	if err := writeAndSync(file, data); err != nil {
		l.failLocked(fmt.Errorf("write container ledger for %s entry %d: %w", entry.Container, entry.Seq, err))
	}
}

func writeAndSync(file *os.File, data []byte) error {
	if file == nil {
		return errors.New("ledger file is nil")
	}
	written, err := file.Write(data)
	if err != nil {
		return err
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	return file.Sync()
}

func (l *Ledger) failLocked(err error) {
	if err == nil || l.firstErr != nil {
		return
	}
	l.firstErr = err
	select {
	case l.errors <- err:
	default:
	}
}

func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return l.firstErr
	}
	l.closed = true
	var first = l.firstErr
	for _, file := range l.files {
		if err := file.Close(); err != nil && first == nil {
			first = err
		}
	}
	if l.global != nil {
		if err := l.global.Close(); err != nil && first == nil {
			first = err
		}
	}
	if l.firstErr == nil && first != nil {
		l.firstErr = first
	}
	close(l.errors)
	return first
}

func sanitizeFilename(value string) string {
	out := make([]rune, 0, len(value))
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
