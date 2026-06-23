// Package web embeds the dashboard's static assets so a single worker binary
// can serve its own UI without external files.
//
// Three files are bundled:
//   - index.html : the dashboard shell
//   - styles.css : presentation
//   - app.js     : behaviour (talks to /api/* and the events WS)
//
// The HTTP layer (internal/api) consumes this FS via http.FS / FS.ReadFile.
package web

import "embed"

//go:embed index.html styles.css app.js
var FS embed.FS
