package clicommand

// runSamples reaches Load through a helper of the allowed file.
func runSamples() {
	runCheckLater()
}

// newSamples names helper, which reaches Load only inside a closure.
func newSamples() { _ = helper() }
