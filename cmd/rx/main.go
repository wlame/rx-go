package main

import "github.com/wlame/rx/internal/cli"

// version is set at build time via -ldflags:
//
//	go build -ldflags="-X main.version=v2.2.1" -o rx ./cmd/rx
var version = "dev"

func main() {
	cli.SetVersion(version)
	cli.Execute()
}
