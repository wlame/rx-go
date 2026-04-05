.PHONY: build test lint cover clean compare bench integration

build:
	go build -o rx ./cmd/rx

test:
	go test -race ./...

lint:
	go vet ./...

cover:
	go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out

clean:
	rm -f rx coverage.out
	rm -f *.test

compare:
	@echo "No playground directory available for Python comparison. Run 'make integration' for Go-only tests."

bench:
	go test -bench=. -benchmem ./internal/engine/...

integration:
	go test -race -count=1 -v ./internal/engine/ -run TestIntegration
