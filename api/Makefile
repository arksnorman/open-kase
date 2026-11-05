# open-kase API. Requires Go on PATH.
BINARY   := open-kase
PKG      := github.com/arksnorman/open-kase/api/package/product
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILT_AT ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).BuiltAt=$(BUILT_AT)

.PHONY: all fmt fmt-check vet test test-integration build run clean tidy

all: fmt-check vet test build

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

test:
	go test ./...

# Runs MongoDB-backed tests; needs a single-node replica set, e.g.
# a local mongod started with --replSet. Each test uses its own throwaway database.
test-integration:
	OPEN_KASE_TEST_MONGO_URI="$${OPEN_KASE_TEST_MONGO_URI:-mongodb://127.0.0.1:27017/?replicaSet=rs0}" go test -count=1 ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) .

run: build
	./bin/$(BINARY) serve

tidy:
	go mod tidy

clean:
	rm -rf bin
