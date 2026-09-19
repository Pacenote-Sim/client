# Pacenote client — developer entry points. Every target is what CI runs.
SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
export PATH := $(PATH):$(shell go env GOPATH)/bin

# The tools, each named once. The PATH above is not enough on its own: GNU Make
# runs a recipe line directly, without a shell, when the line holds no shell
# metacharacters, and that direct execution searches make's own PATH. A tool
# installed by `go install` is invisible to exactly the recipes that are a
# single command, so each is resolved here.
GOBIN        := $(shell go env GOPATH)/bin
GOFUMPT      := $(shell command -v gofumpt       2>/dev/null || echo $(GOBIN)/gofumpt)
GOLANGCILINT := $(shell command -v golangci-lint 2>/dev/null || echo $(GOBIN)/golangci-lint)
GOVULNCHECK  := $(shell command -v govulncheck   2>/dev/null || echo $(GOBIN)/govulncheck)

TESTFLAGS := -race -shuffle=on -count=1
DEMO_SERVER ?= http://localhost:8080

# The official plugins, for `make official`. This module requires none of them:
# a client is the app plus whatever a build imported, and nothing is loaded at
# runtime. Change the list to build a different client.
OFFICIAL ?= client-iracing client-engineer client-visual-telemetry client-voice

# The Go this repository is responsible for. $(DIST) holds what a build wrote,
# including the main a plugin build is made of, which is generated and belongs
# to nobody's style.
GOFILES = $(shell find . -name '*.go' -not -path './$(DIST)/*')
COVER_MIN := 90
DIST      := dist


.DEFAULT_GOAL := check

.PHONY: help check build build-window build-windows official demo vet lint lint-fix fmt fmt-check test test-postgres cover bench bench-smoke tidy-check vuln clean

## help: list targets
help:
	@grep -E '^## [a-z-]+:' $(MAKEFILE_LIST) | sed -E 's/^## ([a-z-]+): */\1\t/' | column -t -s $$'\t'

## check: everything CI runs, in order
check: fmt-check build vet lint test cover bench-smoke tidy-check build-windows

## build: compile the app
build:
	go build ./...

## vet: go vet, the headless and the window builds
vet:
	go vet ./...
	go vet -tags wails ./...
	

## lint: golangci-lint, the headless and the window builds
lint:
	$(GOLANGCILINT) run ./...
	$(GOLANGCILINT) run --build-tags wails ./cmd/...

## lint-fix: golangci-lint, fixing what it can
lint-fix:
	$(GOLANGCILINT) run --fix ./...

## fmt: gofumpt in place
fmt:
	$(GOFUMPT) -w $(GOFILES)

## fmt-check: fail if anything is unformatted
fmt-check:
	@out=$$($(GOFUMPT) -l $(GOFILES)); if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

## test: the suite, with the race detector
test:
	go test $(TESTFLAGS) ./...

## bench: what a sample and a lap cost
bench:
	go test -run XXX -bench . -benchmem ./...

## bench-smoke: run each benchmark once, so they cannot rot unnoticed
bench-smoke:
	go test -run XXX -bench . -benchtime=1x ./...

## cover: coverage — every package over $(COVER_MIN)% (cmd/pacenote excluded: main is wiring)
cover:
	go test -covermode=atomic -coverprofile=coverage.out ./...
	scripts/coverage.sh coverage.out $(COVER_MIN)

## tidy-check: fail if go.mod or go.sum would change. In workspace mode until the contract is published.
tidy-check:
	go mod tidy -diff

## build-windows: the exe a driver runs — the window — cross-compiled; no cgo, no toolchain but Go. And the headless one beside it.
build-windows:
	mkdir -p $(DIST)
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -tags wails -trimpath -buildvcs=false -ldflags "-s -w -H windowsgui" -o $(DIST)/pacenote.exe ./cmd/pacenote
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "-s -w" -o $(DIST)/pacenote-headless.exe ./cmd/pacenote

## build-window: the window for this machine, to open and look at
build-window:
	mkdir -p $(DIST)
	go build -tags wails -o $(DIST)/pacenote-window ./cmd/pacenote

## official: the window and the exe with the official plugins in them, built the way a server builds a driver's — from the checkouts beside this one
official:
	scripts/bundle.sh $(DIST)/build $(OFFICIAL)
	cd $(DIST)/build && GOWORK=off go build -tags wails -o ../pacenote-window .
	cd $(DIST)/build && GOWORK=off GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -tags wails -trimpath -buildvcs=false -ldflags "-s -w -H windowsgui" -o ../pacenote.exe .
	@echo "built with: $(OFFICIAL)"

## demo: drive the demo source through the lap and corner code, fast, against a server on :8080
demo:
	go run ./cmd/pacenote --server $(DEMO_SERVER) --demo --laps 2 --speed 50

## vuln: govulncheck
vuln:
	$(GOVULNCHECK) ./...

## clean: remove build outputs
clean:
	rm -rf dist coverage.out
