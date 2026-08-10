// Package web embeds the built dashboard SPA. web/dist is committed, so
// `go build` never needs Node; `make web` (dev-only tooling) rebuilds it.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built SPA rooted at its index.html.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic("web: dist subtree missing from embed: " + err.Error())
	}
	return sub
}
