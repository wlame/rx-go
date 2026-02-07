.PHONY: all build test clean install run-server fmt lint deps help
.PHONY: build-linux-amd64 build-linux-arm64 build-darwin-amd64 build-darwin-arm64 build-windows-amd64
.PHONY: build-all dist clean-dist

# Build variables
BINARY_NAME=rx
BUILD_DIR=bin
DIST_DIR=dist
VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-ldflags "-X main.Version=$(VERSION) -s -w"

all: deps fmt test build

# Build the binary (for current platform)
build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/rx

# Cross-compilation targets
build-linux-amd64:
	@echo "Building for Linux (amd64)..."
	@mkdir -p $(DIST_DIR)
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/rx

build-linux-arm64:
	@echo "Building for Linux (arm64)..."
	@mkdir -p $(DIST_DIR)
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-linux-arm64 ./cmd/rx

build-darwin-amd64:
	@echo "Building for macOS (amd64/Intel)..."
	@mkdir -p $(DIST_DIR)
	GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-darwin-amd64 ./cmd/rx

build-darwin-arm64:
	@echo "Building for macOS (arm64/Apple Silicon)..."
	@mkdir -p $(DIST_DIR)
	GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-darwin-arm64 ./cmd/rx

build-windows-amd64:
	@echo "Building for Windows (amd64)..."
	@mkdir -p $(DIST_DIR)
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o $(DIST_DIR)/$(BINARY_NAME)-windows-amd64.exe ./cmd/rx

# Build for all platforms
build-all: build-linux-amd64 build-linux-arm64 build-darwin-amd64 build-darwin-arm64 build-windows-amd64
	@echo "All platform binaries built in $(DIST_DIR)/"
	@ls -lh $(DIST_DIR)/

# Create distribution archives
dist: build-all
	@echo "Creating distribution archives..."
	@cd $(DIST_DIR) && tar -czf $(BINARY_NAME)-$(VERSION)-linux-amd64.tar.gz $(BINARY_NAME)-linux-amd64
	@cd $(DIST_DIR) && tar -czf $(BINARY_NAME)-$(VERSION)-linux-arm64.tar.gz $(BINARY_NAME)-linux-arm64
	@cd $(DIST_DIR) && tar -czf $(BINARY_NAME)-$(VERSION)-darwin-amd64.tar.gz $(BINARY_NAME)-darwin-amd64
	@cd $(DIST_DIR) && tar -czf $(BINARY_NAME)-$(VERSION)-darwin-arm64.tar.gz $(BINARY_NAME)-darwin-arm64
	@cd $(DIST_DIR) && tar -czf $(BINARY_NAME)-$(VERSION)-windows-amd64.tar.gz $(BINARY_NAME)-windows-amd64.exe
	@if command -v zip >/dev/null 2>&1; then \
		cd $(DIST_DIR) && zip -q $(BINARY_NAME)-$(VERSION)-windows-amd64.zip $(BINARY_NAME)-windows-amd64.exe; \
		echo "Windows .zip archive created"; \
	else \
		echo "Note: zip not available, using .tar.gz for Windows"; \
	fi
	@echo "Distribution archives created:"
	@ls -lh $(DIST_DIR)/*.tar.gz $(DIST_DIR)/*.zip 2>/dev/null || ls -lh $(DIST_DIR)/*.tar.gz

# Run tests
test:
	@echo "Running tests..."
	go test -v -race -coverprofile=coverage.out ./...

# Run tests with coverage report
test-coverage: test
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

# Run integration tests
test-integration:
	@echo "Running integration tests..."
	go test -v -tags=integration ./test/integration/...

# Install dependencies
deps:
	@echo "Installing dependencies..."
	go mod download
	go mod tidy

# Format code
fmt:
	@echo "Formatting code..."
	go fmt ./...
	gofmt -s -w .

# Lint code
lint:
	@echo "Linting code..."
	@which golangci-lint > /dev/null || (echo "Installing golangci-lint..." && go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)
	golangci-lint run ./...

# Clean build artifacts
clean:
	@echo "Cleaning..."
	rm -rf $(BUILD_DIR)
	rm -f coverage.out coverage.html
	go clean

# Clean distribution artifacts
clean-dist:
	@echo "Cleaning distribution files..."
	rm -rf $(DIST_DIR)

# Install binary to PATH
install: build
	@echo "Installing to /usr/local/bin/$(BINARY_NAME)..."
	sudo cp $(BUILD_DIR)/$(BINARY_NAME) /usr/local/bin/

# Run development server
run-server: build
	@echo "Starting server on port 8000..."
	./$(BUILD_DIR)/$(BINARY_NAME) serve --port=8000

# Run with debug logging
run-debug: build
	RX_DEBUG=1 RX_LOG_LEVEL=DEBUG ./$(BUILD_DIR)/$(BINARY_NAME)

# Generate mocks (if needed)
generate:
	@echo "Generating code..."
	go generate ./...

# Show help
help:
	@echo "Available targets:"
	@echo ""
	@echo "Building:"
	@echo "  make build                - Build binary for current platform"
	@echo "  make build-linux-amd64    - Build for Linux (amd64)"
	@echo "  make build-linux-arm64    - Build for Linux (arm64)"
	@echo "  make build-darwin-amd64   - Build for macOS (Intel)"
	@echo "  make build-darwin-arm64   - Build for macOS (Apple Silicon)"
	@echo "  make build-windows-amd64  - Build for Windows (amd64)"
	@echo "  make build-all            - Build for all platforms"
	@echo "  make dist                 - Build all platforms and create archives"
	@echo ""
	@echo "Testing:"
	@echo "  make test                 - Run tests"
	@echo "  make test-coverage        - Run tests with coverage report"
	@echo "  make test-integration     - Run integration tests"
	@echo ""
	@echo "Development:"
	@echo "  make deps                 - Install dependencies"
	@echo "  make fmt                  - Format code"
	@echo "  make lint                 - Lint code"
	@echo "  make run-server           - Run development server"
	@echo "  make run-debug            - Run with debug logging"
	@echo ""
	@echo "Maintenance:"
	@echo "  make clean                - Clean build artifacts"
	@echo "  make clean-dist           - Clean distribution artifacts"
	@echo "  make install              - Install binary to /usr/local/bin"
	@echo ""
	@echo "Convenience:"
	@echo "  make all                  - Run deps, fmt, test, and build"
