.PHONY: build test race bench vet fmt run smoke clean

BINARY ?= vmflow
CONFIG ?= ./examples/config.yaml
GOCACHE ?= /tmp/vmflow-gocache
GO_FILES := $(shell git ls-files '*.go' 2>/dev/null || find . -name '*.go' -not -path './.git/*' -not -path './.agents/*' -not -path './.claude/*' -not -path './.codex/*' -not -path './.lingma/*')
GO_FILES := $(wildcard $(GO_FILES))

build:
	GOCACHE=$(GOCACHE) go build -buildvcs=false -trimpath -o $(BINARY) ./cmd/vmflow

test:
	GOCACHE=$(GOCACHE) go test ./...

race:
	GOCACHE=$(GOCACHE) go test -race ./engine ./metrics ./precheck ./controlapi ./config .

bench:
	GOCACHE=$(GOCACHE) go test ./engine ./metrics ./precheck -run '^$$' -bench . -benchmem

vet:
	GOCACHE=$(GOCACHE) go vet ./...

fmt:
	gofmt -w $(GO_FILES)

run:
	GOCACHE=$(GOCACHE) GOFLAGS=-buildvcs=false go run ./cmd/vmflow run -config $(CONFIG)

smoke:
	GOCACHE=$(GOCACHE) GOFLAGS=-buildvcs=false go run ./cmd/vmflow version
	GOCACHE=$(GOCACHE) GOFLAGS=-buildvcs=false go run ./cmd/vmflow version -json
	GOCACHE=$(GOCACHE) GOFLAGS=-buildvcs=false go run ./examples/embedding

clean:
	rm -f vmflow relayd relayctl relaytui relay
	rm -rf bin dist
