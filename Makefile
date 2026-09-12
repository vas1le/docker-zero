SHELL := /bin/bash
VERSION ?= $(shell cat VERSION)
BINARY := dist/docker-zero-linux-amd64
ARM64_BINARY := dist/docker-zero-linux-arm64
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build build-arm64 build-all fmt fmt-check lint test test-repeat race race-repeat vet vuln pycheck integration integration-retest doctor matrix-smoke matrix-harness-test compose-topology fuzz-smoke verify retest checksums release hooks pre-commit-install clean

all: verify

build:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='$(LDFLAGS)' -o $(BINARY) .

build-arm64:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='$(LDFLAGS)' -o $(ARM64_BINARY) .

build-all: build build-arm64

fmt:
	gofmt -w *.go

fmt-check:
	@test -z "$$(gofmt -l *.go)" || { echo "gofmt required:"; gofmt -l *.go; exit 1; }

lint:
	golangci-lint run ./...

test:
	go test ./...

test-repeat:
	go test -shuffle=on -count=100 ./...

race:
	go test -race ./...

race-repeat:
	go test -race -shuffle=on -count=10 ./...

vet:
	go vet ./...

vuln:
	govulncheck ./...

pycheck:
	python3 -m py_compile scripts/*.py

integration: build
	python3 scripts/integration_test.py $(BINARY)

integration-retest: build
	python3 scripts/integration_test.py $(BINARY)
	python3 scripts/integration_test.py $(BINARY)

doctor: build
	python3 scripts/doctor.py $(BINARY)

matrix-smoke: build
	@tmp=$$(mktemp -d /tmp/docker-zero-matrix.XXXXXX); \
	trap 'rm -rf "$$tmp"' EXIT; \
	python3 scripts/orchestrator_matrix.py \
		--engine $(BINARY) \
		--results-dir "$$tmp" \
		--seeds 0,1,2 \
		--timeout 15 \
		--container-endpoints off \
		-- python3 scripts/matrix_probe.py; \
	python3 -c 'import json,pathlib,sys; p=pathlib.Path(sys.argv[1]); s=json.loads((p/"summary.json").read_text()); assert s["counts"] == {"passed": 9}' "$$tmp"

matrix-harness-test: build
	python3 scripts/matrix_harness_test.py $(BINARY)

compose-topology: build
	python3 scripts/compose_topology_test.py $(BINARY) --compose "$${COMPOSE_BIN:-docker-compose}"

fuzz-smoke:
	go test -run='^$$' -fuzz=FuzzStripAPIVersion -fuzztime=2s .
	go test -run='^$$' -fuzz=FuzzReadRESPCommand -fuzztime=2s .
	go test -run='^$$' -fuzz=FuzzRESPReplyValidator -fuzztime=2s .
	go test -run='^$$' -fuzz=FuzzCookbookDecoderNeverPanics -fuzztime=2s .

verify: fmt-check test race vet pycheck integration doctor matrix-smoke matrix-harness-test

retest: verify test-repeat race-repeat integration-retest fuzz-smoke

checksums: build-all
	sha256sum $(BINARY) $(ARM64_BINARY) > dist/SHA256SUMS

release: verify compose-topology checksums

hooks:
	git config core.hooksPath .githooks

pre-commit-install:
	pre-commit install

clean:
	rm -rf dist runs matrix-runs __pycache__ scripts/__pycache__ .pytest_cache .coverage coverage.out
