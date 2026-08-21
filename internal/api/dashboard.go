package api

import (
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"html/template"
	"net/http"
)

//go:embed dashboard.html
var dashboardSource string

// dashboardPage is parsed once. html/template escapes what it inserts
// according to where it is being inserted, which is why the page is a
// template rather than a string with the nonce pasted into it.
var dashboardPage = template.Must(template.New("dashboard").Parse(dashboardSource))

// dashboard serves the monitoring page.
//
// The page carries no API key. It asks the reader for one and keeps it in the
// browser for that tab. The old page had the key printed into its source, so
// anybody who could open the dashboard could read the key that guarded the
// whole API, and the page then sent it in the query string of every request.
func (a *API) dashboard(w http.ResponseWriter, _ *http.Request) {
	nonce, err := newNonce()
	if err != nil {
		a.log.Error("cannot build a nonce for the dashboard", "error", err)
		a.fail(w, http.StatusInternalServerError, "cannot serve the dashboard")
		return
	}

	// A policy that names this page's own script and nothing else.
	//
	// It is the second line of defence and not the first: the page builds
	// every row with textContent, so there is nothing for a policy to stop.
	// It is here because the first line is one careless innerHTML away from
	// being gone, and this is what remains standing on that day.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; "+
			"script-src 'nonce-"+nonce+"'; "+
			"style-src 'nonce-"+nonce+"'; "+
			// The page carries its own mark, and default-src none blocks an
			// image as readily as a script. Leaving this out gives a broken
			// picture and a console message that nobody watching the queue
			// would ever look for.
			"img-src 'self'; "+
			"connect-src 'self'; "+
			"base-uri 'none'; "+
			"form-action 'none'; "+
			"frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if err := dashboardPage.Execute(w, struct{ Nonce string }{nonce}); err != nil {
		a.log.Error("cannot write the dashboard", "error", err)
	}
}

func newNonce() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(raw), nil
}
