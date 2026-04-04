.PHONY: build test lint cover clean compare bench

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
	@echo "TODO: comparison tests"

bench:
	@echo "TODO: benchmarks"
