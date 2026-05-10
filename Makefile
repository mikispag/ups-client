# ups-client — build & test targets.

BINARY  := ups-client
PKG     := ./...
GOFLAGS := -trimpath
LDFLAGS := -s -w

.PHONY: all build install test test-race cover vet check tidy clean help

all: build

## build: compile the static binary into ./bin/$(BINARY)
build:
	@mkdir -p bin
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BINARY) .

## install: install $(BINARY) into $(GOBIN) (or $GOPATH/bin)
install:
	go install $(GOFLAGS) -ldflags '$(LDFLAGS)' .

## test: run unit tests
test:
	go test $(PKG)

## test-race: run unit tests with the race detector
test-race:
	go test -race $(PKG)

## cover: run tests with coverage; writes coverage.out and prints summary
cover:
	go test -race -covermode=atomic -coverprofile=coverage.out $(PKG)
	@go tool cover -func=coverage.out | tail -n 1

## vet: run go vet
vet:
	go vet $(PKG)

## check: vet + race tests (what CI runs)
check: vet test-race

## tidy: tidy go.mod / go.sum
tidy:
	go mod tidy

## clean: remove build artifacts
clean:
	rm -rf bin coverage.out

## help: list available targets
help:
	@awk 'BEGIN {FS = ":.*?## "} /^## / { sub(/^## /, "", $$0); split($$0, a, ": "); printf "  %-12s %s\n", a[1], a[2] }' $(MAKEFILE_LIST)
