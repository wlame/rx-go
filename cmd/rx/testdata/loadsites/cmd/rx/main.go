// Package main is a fixture: it builds the signatures command, which
// loads only inside its closure.
package main

import "example.com/rx/internal/clicommand"

func main() { _ = clicommand.NewSignaturesCommand() }
