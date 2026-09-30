package signatures

import _ "embed" // for the //go:embed directive below

// templateFile is the commented example file `rx signatures template`
// prints: one signature of each kind, every field explained.
//
// Go note: //go:embed copies template.yml into the binary at build
// time as the bytes of this variable.
//
//go:embed template.yml
var templateFile []byte

// Template returns a commented signature file with one signature of
// each kind. It loads without errors, so a user can start from it.
func Template() []byte {
	out := make([]byte, len(templateFile))
	copy(out, templateFile)
	return out
}
