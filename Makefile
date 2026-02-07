.PHONY: all build test clean install run-server fmt lint deps help

# Build variables
BINARY_NAME=rx
BUILD_DIR=bin
VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-ldflags "-X main.Version=$(VERSION)"

all: deps fmt test build

# Build the binary
build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/rx

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
	@echo "  make build           - Build the binary"
	@echo "  make test            - Run tests"
	@echo "  make test-coverage   - Run tests with coverage report"
	@echo "  make test-integration - Run integration tests"
	@echo "  make deps            - Install dependencies"
	@echo "  make fmt             - Format code"
	@echo "  make lint            - Lint code"
	@echo "  make clean           - Clean build artifacts"
	@echo "  make install         - Install binary to /usr/local/bin"
	@echo "  make run-server      - Run development server"
	@echo "  make run-debug       - Run with debug logging"
	@echo "  make all             - Run deps, fmt, test, and build"
