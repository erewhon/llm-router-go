set shell := ["bash", "-c"]
set positional-arguments

# default: list recipes
default:
    @just --list

# version stamped into binaries via -ldflags. Evaluated once at parse time.
version := `git describe --tags --always --dirty 2>/dev/null || echo dev`
ldflags := "-X main.version=" + version

# build all binaries into ./bin/
build: build-node-agent build-tool-proxy build-router

build-node-agent:
    go build -ldflags='{{ ldflags }}' -o bin/node-agent ./cmd/node-agent

build-tool-proxy:
    go build -ldflags='{{ ldflags }}' -o bin/tool-proxy ./cmd/tool-proxy

build-router:
    go build -ldflags='{{ ldflags }}' -o bin/router ./cmd/router

# cross-compile arm64 binaries for the Sparks (archimedes, hypatia)
build-arm64:
    GOOS=linux GOARCH=arm64 go build -ldflags='{{ ldflags }}' -o bin/arm64/node-agent ./cmd/node-agent
    GOOS=linux GOARCH=arm64 go build -ldflags='{{ ldflags }}' -o bin/arm64/tool-proxy ./cmd/tool-proxy
    GOOS=linux GOARCH=arm64 go build -ldflags='{{ ldflags }}' -o bin/arm64/router    ./cmd/router

# run tests
test:
    go test ./...

# run tests with race detector and coverage
test-race:
    go test -race -coverprofile=coverage.txt ./...

# format (uses goimports if installed, else gofmt)
fmt:
    @if command -v goimports >/dev/null; then \
        goimports -w -local github.com/erewhon/llm-router-go . ; \
    else \
        gofmt -w . ; \
    fi

# lint (requires golangci-lint)
lint:
    golangci-lint run ./...

# tidy go.mod
tidy:
    go mod tidy

# clean build artifacts
clean:
    rm -rf bin/ coverage.txt coverage.html

# print version that would be embedded into a build
print-version:
    @echo {{ version }}

# deploy node-agent to a host: cross-compile + rsync + install (does NOT start)
deploy-node-agent host:
    deploy/scripts/deploy-node-agent.sh {{ host }}

# deploy and start node-agent on a host
deploy-node-agent-start host:
    deploy/scripts/deploy-node-agent.sh {{ host }} --start
