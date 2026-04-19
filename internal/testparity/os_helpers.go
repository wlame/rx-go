package testparity

import "os"

// osCreate is a tiny indirection over os.Create. Separate file so
// test-only utilities stay out of fixtures.go's main surface.
func osCreate(path string) (*os.File, error) { return os.Create(path) }
