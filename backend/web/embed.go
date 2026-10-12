// Package web holds the web UI files embedded into the binary.
//
// Embedding cannot reach outside the module, so the sources in ../ui are copied
// into dist/ before a build (`make ui`, the Containerfile, ship's ui sync step).
// Only dist/.gitkeep is tracked: it keeps the directory embeddable on a fresh
// clone, where FS holds nothing but that file.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// FS returns the UI files rooted at dist/.
func FS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err) // dist is a literal embedded directory; Sub cannot fail
	}
	return sub
}
