// Package web embeds the read-only dashboard: static HTML, CSS and vanilla
// JavaScript, with no build step. The files carry no data; the page fetches
// everything it shows from the authenticated /v1 API in the browser.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// Static returns the dashboard files rooted at the static directory, so
// "index.html" and "app.js" are top-level names.
func Static() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		// fs.Sub only fails on an invalid name, and "static" is a constant.
		panic(err)
	}
	return sub
}
