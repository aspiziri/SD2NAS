// Package web holds the built web UI (web/dist, written by `npm run build`
// in this directory) for embedding in the sd2nas binary.
package web

import (
	"embed"
	"io/fs"
)

// dist always contains dist/.placeholder (committed), so this package
// compiles on a fresh checkout before the UI has ever been built.
//
//go:embed all:dist
var dist embed.FS

// Dist returns the built UI rooted at web/dist. Without a build it holds
// only .placeholder and has no index.html.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // "dist" is a valid literal path, so fs.Sub cannot fail
	}
	return sub
}
