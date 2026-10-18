// Package assets holds the files the API documentation pages of `rx
// serve` load: the Swagger UI stylesheet and bundle behind /docs and
// the ReDoc bundle behind /redoc. They are embedded in the binary so
// the pages render on a network with no route to the internet, where
// /docs is often the only API reference at hand; before, both pages
// loaded them from CDNs and rendered blank offline.
//
// Each file is one release of its project, fetched once from the
// pinned URL in Files and committed as it was received. Its SHA-256
// is recorded twice, in Files and in SHA256SUMS beside the files, and a
// test checks the embedded bytes against both, so an upgrade is a
// deliberate change of the table, never a silent one. The licenses are
// beside the files too (LICENSE-swagger-ui, Apache-2.0;
// LICENSE-redoc, MIT).
package assets

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
)

// File describes one embedded asset: its name in FS, the release it
// came from, where it was fetched and the SHA-256 of its bytes.
type File struct {
	Name        string
	Version     string
	Source      string
	ContentType string
	SHA256      string
}

// Files lists every embedded asset. The order is the order of
// SHA256SUMS.
var Files = []File{
	{
		Name:        "swagger-ui.css",
		Version:     "swagger-ui-dist 5.31.1",
		Source:      "https://unpkg.com/swagger-ui-dist@5.31.1/swagger-ui.css",
		ContentType: "text/css; charset=utf-8",
		SHA256:      "9e576f8322132c76949da47f9bb53c891373c303ea3a761ff548ffc9de50a98c",
	},
	{
		Name:        "swagger-ui-bundle.js",
		Version:     "swagger-ui-dist 5.31.1",
		Source:      "https://unpkg.com/swagger-ui-dist@5.31.1/swagger-ui-bundle.js",
		ContentType: "application/javascript; charset=utf-8",
		SHA256:      "7945c6eb2f92db4ffbbd50ecee96d0900290d3ec8144adf06d8e135bbd82a4ed",
	},
	{
		Name:        "redoc.standalone.js",
		Version:     "ReDoc v2.5.0",
		Source:      "https://cdn.redoc.ly/redoc/v2.5.0/bundles/redoc.standalone.js",
		ContentType: "application/javascript; charset=utf-8",
		SHA256:      "0ec05be285ac885a330289b02f470e1bdbd2b6b3223a9fa213f24bf805a851d1",
	},
}

// FS holds the files of Files. go:embed reads them at build time, so the
// binary carries them and needs no directory beside it.
//
//go:embed swagger-ui.css swagger-ui-bundle.js redoc.standalone.js
var FS embed.FS

// Read returns the bytes of the asset named name, or an error when
// Files does not list it.
func Read(name string) ([]byte, error) {
	if _, ok := Lookup(name); !ok {
		return nil, fmt.Errorf("assets: %q is not an embedded asset", name)
	}
	return FS.ReadFile(name)
}

// Lookup returns the File named name.
func Lookup(name string) (File, bool) {
	for _, f := range Files {
		if f.Name == name {
			return f, true
		}
	}
	return File{}, false
}

// Digest returns the lowercase hex SHA-256 of data, the form Files and
// SHA256SUMS record.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
