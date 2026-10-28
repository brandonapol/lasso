// Package web holds the browser view's static files (ADR 0001 §1): plain
// HTML, CSS and JavaScript with no build step, embedded into the daemon.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// FS is the web view's files, rooted at the static directory (index.html,
// app.css, app.js).
func FS() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err) // the directory is embedded above
	}
	return sub
}
