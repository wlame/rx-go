.PHONY: build test lint cover clean compare bench integration build-all release

# Version is read from git tag, or falls back to the VERSION env var, or "dev".
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o rx ./cmd/rx

test:
	go test -race ./...

lint:
	go vet ./...

cover:
	go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out

clean:
	rm -f rx coverage.out
	rm -f *.test
	rm -rf dist/

compare:
	@echo "No playground directory available for Python comparison. Run 'make integration' for Go-only tests."

bench:
	go test -bench=. -benchmem ./internal/engine/...

integration:
	go test -race -count=1 -v ./internal/engine/ -run TestIntegration

# Cross-compilation targets for release builds.
# Produces static binaries for linux and darwin on amd64 and arm64.
build-all: clean
	@mkdir -p dist
	GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o dist/rx-linux-amd64   ./cmd/rx
	GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o dist/rx-linux-arm64   ./cmd/rx
	GOOS=darwin  GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o dist/rx-darwin-amd64  ./cmd/rx
	GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o dist/rx-darwin-arm64  ./cmd/rx
	@echo "Built binaries in dist/:"
	@ls -lh dist/

# Build all targets and prepare for release.
release: build-all
	@echo ""
	@echo "Release $(VERSION) ready in dist/"
	@for f in dist/rx-*; do sha256sum "$$f"; done
