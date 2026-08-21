package api

import (
	_ "embed"
	"net/http"
)

// logoSource is the mark, compiled into the binary.
//
// The file lives beside this code rather than in docs/ because go:embed
// cannot reach a parent directory and does not follow a symbolic link. The
// choice was one file in a slightly odd place against two copies that drift,
// and this project has a rule about the second of those. The README points at
// this path.
//
//go:embed logo.svg
var logoSource []byte

// logo serves the mark for the dashboard.
//
// The page loads it with an img tag, so script inside an SVG cannot run from
// it whatever the file holds. It is compiled in rather than read from disk,
// so there is no path for a caller to reach any other file through this.
func (a *API) logo(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// An hour. The file changes only when the binary does, and a stale mark
	// for an hour after a deployment costs nothing.
	w.Header().Set("Cache-Control", "public, max-age=3600")

	if _, err := w.Write(logoSource); err != nil {
		a.log.Error("cannot write the logo", "error", err)
	}
}
