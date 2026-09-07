//go:build !linux && !darwin

package index

import "os"

// fileOwner has no uid to resolve on a platform whose FileInfo does not
// carry one, so the index records no owner rather than a guess.
func fileOwner(_ os.FileInfo) *string { return nil }
