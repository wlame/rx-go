// Package other is a fixture: it loads through an import alias.
package other

import sig "example.com/rx/internal/signatures"

var catalogue = sig.Load
