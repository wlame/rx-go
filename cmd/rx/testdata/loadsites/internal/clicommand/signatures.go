// Package clicommand is a fixture: the allowed file loads signatures in
// a command's closure and in a helper.
package clicommand

import "example.com/rx/internal/signatures"

// NewSignaturesCommand loads only inside its closure.
func NewSignaturesCommand() func() {
	return func() { _ = signatures.Load(signatures.LoadOptions{}) }
}

// runCheck loads directly.
func runCheck() { _ = signatures.Load(signatures.LoadOptions{}) }

// runCheckLater reaches Load through runCheck.
func runCheckLater() { runCheck() }

// helper names runCheck inside a closure only.
func helper() func() { return func() { runCheckLater() } }
