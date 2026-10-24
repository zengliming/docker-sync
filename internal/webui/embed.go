// Package webui embeds the static frontend (HTML/CSS/JS) into the server
// binary so that the container needs no extra files.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed web
var assets embed.FS

// FS returns the embedded frontend files rooted at web/.
func FS() fs.FS {
	sub, err := fs.Sub(assets, "web")
	if err != nil {
		panic(err)
	}
	return sub
}
