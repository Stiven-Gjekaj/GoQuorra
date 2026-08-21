// Package api serves the REST interface and the dashboard.
//
// The router is net/http. Go 1.22 gave ServeMux method and wildcard patterns,
// so "POST /v1/jobs" and "GET /v1/jobs/{id}" are routes it understands, and
// the chi dependency the old version carried for exactly that is no longer
// paying for itself.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
)

// Options configure the API.
type Options struct {
	Store   store.Store
	Metrics *metrics.Metrics
	Log     *slog.Logger

	// APIKey guards every route under /v1.
	APIKey string

	// MaxBodyBytes caps a request body. Without a cap, one client sending an
	// endless body holds a connection and a goroutine for as long as it
	// likes, and the memory it costs is charged to this process.
	MaxBodyBytes int64

	// DashboardEnabled serves the monitoring page. It is separate from the
	// API key because the page is public: it asks the reader for a key and
	// keeps it in the browser.
	DashboardEnabled bool
}

// API answers HTTP requests.
type API struct {
	opts Options
	log  *slog.Logger
}

// New builds the API.
func New(opts Options) *API {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 1 << 20
	}
	return &API{opts: opts, log: opts.Log}
}

// Handler builds the router.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public. A health check that needed a key would have to carry one in
	// every load balancer and every container definition.
	mux.HandleFunc("GET /healthz", a.alive)
	mux.HandleFunc("GET /readyz", a.ready)
	mux.Handle("GET /metrics", a.opts.Metrics.Handler())

	if a.opts.DashboardEnabled {
		mux.HandleFunc("GET /{$}", a.dashboard)
		mux.HandleFunc("GET /dashboard", a.dashboard)
	}

	// Guarded.
	mux.Handle("POST /v1/jobs", a.guard(http.HandlerFunc(a.createJob)))
	mux.Handle("GET /v1/jobs", a.guard(http.HandlerFunc(a.recentJobs)))
	mux.Handle("GET /v1/jobs/{id}", a.guard(http.HandlerFunc(a.getJob)))
	mux.Handle("GET /v1/queues", a.guard(http.HandlerFunc(a.queueStats)))

	return a.observe(mux)
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// recorder remembers the status so that the metrics and the log can report it.
type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// observe times a request, records it, and turns a panic into a 500.
func (a *API) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &recorder{ResponseWriter: w}

		defer func() {
			if panicked := recover(); panicked != nil {
				// One bad request must not end the process and take every
				// other request in flight with it.
				a.log.Error("a request panicked",
					"method", r.Method, "path", r.URL.Path, "panic", panicked)
				if rec.status == 0 {
					http.Error(rec, `{"error":"internal error"}`, http.StatusInternalServerError)
				}
			}

			if rec.status == 0 {
				rec.status = http.StatusOK
			}

			// The pattern and not the path. Labelling by path puts one time
			// series on every job identifier ever requested, which is how a
			// metrics store falls over.
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			a.opts.Metrics.HTTPRequest(route, r.Method, statusText(rec.status), time.Since(started))
		}()

		next.ServeHTTP(rec, r)
	})
}

// guard checks the API key.
func (a *API) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The header only.
		//
		// The old server also read the key from the query string, and its
		// dashboard put the key in every URL it fetched. A query string is
		// written to the access log of every proxy in front of this server,
		// kept in browser history, and sent in the Referer header to any
		// address a page links to. A key that has been in one is a key that
		// has to be replaced.
		if !equalKeys(r.Header.Get("X-API-Key"), a.opts.APIKey) {
			a.fail(w, http.StatusUnauthorized, "the X-API-Key header is missing or wrong")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Answers
// ---------------------------------------------------------------------------

func (a *API) send(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line has gone already, so this cannot be turned into an
		// error for the client. It is still worth a log line, because a
		// client that received half an answer will report something that
		// makes no sense on its own.
		a.log.Error("cannot write the answer", "error", err)
	}
}

func (a *API) fail(w http.ResponseWriter, status int, message string) {
	a.send(w, status, map[string]string{"error": message})
}

// failWith maps a store error onto a status.
//
// The old handler answered 404 to every failure from the store, so a database
// that had fallen over was reported to the client as a missing job, and the
// logs of the client and of the server told different stories.
func (a *API) failWith(w http.ResponseWriter, err error, what string) {
	if errors.Is(err, store.ErrNotFound) {
		a.fail(w, http.StatusNotFound, "no job carries that identifier")
		return
	}

	a.log.Error(what, "error", err)
	a.fail(w, http.StatusInternalServerError, what)
}

func statusText(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	default:
		return "2xx"
	}
}
