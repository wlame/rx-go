# rx — ripgrep-powered log/file search tool.
#
# Targets:
#   make build      — build the `rx` binary into dist/ (default target)
#   make build-all  — cross-compile static binaries for linux/darwin
#   make test       — run unit tests with -race and coverage
#   make cover      — write coverage.out + coverage.html
#   make lint       — run golangci-lint
#   make fmt        — run gofmt -s -w
#   make vet        — run go vet
#   make ci         — fmt-check + vet + lint + test (what CI runs)
#   make clean      — remove dist/, coverage artifacts

VERSION ?= 0.1.0-dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build build-all test cover lint fmt vet ci clean

build:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o dist/rx ./cmd/rx

# Cross-compile for the three distribution targets from spec §11.1.
# Each leg produces dist/rx-<os>-<arch>. CGO disabled for fully static binaries.
build-all:
	GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o dist/rx-linux-amd64  ./cmd/rx
	GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o dist/rx-linux-arm64  ./cmd/rx
	GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o dist/rx-darwin-arm64 ./cmd/rx

test:
	go test -race -cover ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	go tool cover -func=coverage.out | tail -20

lint:
	golangci-lint run ./...

fmt:
	gofmt -s -w .

vet:
	go vet ./...

# Fail if any file is unformatted.
fmt-check:
	@if [ -n "$$(gofmt -s -l .)" ]; then \
		echo "These files need formatting:"; \
		gofmt -s -l .; \
		exit 1; \
	fi

ci: fmt-check vet lint test

clean:
	rm -rf dist/ coverage.out coverage.html
