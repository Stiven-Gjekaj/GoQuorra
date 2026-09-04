// Package api serves the REST interface and the dashboard.
//
// The router is net/http. Go 1.22 gave ServeMux method and wildcard patterns,
// so "POST /v1/jobs" and "GET /v1/jobs/{id}" are routes it understands, and
// the chi dependency the old version carried for exactly that is no longer
// paying for itself.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/auth"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/reqid"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
)

// Options configure the API.
type Options struct {
	Store   store.Store
	Metrics *metrics.Metrics
	Log     *slog.Logger

	// Keys guard every route under /v1. The set names each caller, so an
	// action can be attributed to one.
	Keys *auth.Set

	// MaxBodyBytes caps a request body. Without a cap, one client sending an
	// endless body holds a connection and a goroutine for as long as it
	// likes, and the memory it costs is charged to this process.
	MaxBodyBytes int64

	// DashboardEnabled serves the monitoring page. It is separate from the
	// API key because the page is public: it asks the reader for a key and
	// keeps it in the browser.
	DashboardEnabled bool

	// Now reads the clock. Leave it nil for time.Now.
	//
	// It is here because one route resolves "due=now" into a moment before it
	// asks the store, and the store is not allowed to read a clock. A test
	// that states the moment can then ask what the queue looked like then,
	// rather than arranging for the answer to be true at the instant it runs.
	Now func() time.Time
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
	if opts.Now == nil {
		opts.Now = time.Now
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

		// Only with the dashboard, because the dashboard is the only thing
		// that asks for it. A deployment that turns the page off serves
		// nothing it does not use.
		mux.HandleFunc("GET /logo.svg", a.logo)
	}

	// Guarded, and each route says the scope it needs.
	//
	// The scope is named at the route and not inside the handler, so the
	// whole policy is one column of this list. A handler that forgot to check
	// would be a route that anybody could call, and reading the list is how
	// somebody notices.
	mux.Handle("POST /v1/jobs", a.guard(auth.Write, http.HandlerFunc(a.createJob)))
	mux.Handle("POST /v1/jobs/bulk", a.guard(auth.Write, http.HandlerFunc(a.createMany)))
	mux.Handle("GET /v1/jobs", a.guard(auth.Read, http.HandlerFunc(a.listJobs)))
	mux.Handle("GET /v1/jobs/{id}", a.guard(auth.Read, http.HandlerFunc(a.getJob)))
	mux.Handle("GET /v1/jobs/{id}/attempts", a.guard(auth.Read, http.HandlerFunc(a.jobAttempts)))
	mux.Handle("POST /v1/jobs/cancel", a.guard(auth.Write, http.HandlerFunc(a.cancelMatching)))
	mux.Handle("POST /v1/jobs/revive", a.guard(auth.Write, http.HandlerFunc(a.reviveMatching)))
	mux.Handle("POST /v1/jobs/{id}/cancel", a.guard(auth.Write, http.HandlerFunc(a.cancelJob)))
	mux.Handle("POST /v1/jobs/{id}/revive", a.guard(auth.Write, http.HandlerFunc(a.reviveJob)))
	mux.Handle("GET /v1/queues", a.guard(auth.Read, http.HandlerFunc(a.queueStats)))
	mux.Handle("GET /v1/workers", a.guard(auth.Read, http.HandlerFunc(a.workers)))

	mux.Handle("POST /v1/schedules", a.guard(auth.Change, http.HandlerFunc(a.createSchedule)))
	mux.Handle("GET /v1/schedules", a.guard(auth.Read, http.HandlerFunc(a.listSchedules)))
	mux.Handle("GET /v1/schedules/{name}", a.guard(auth.Read, http.HandlerFunc(a.getSchedule)))
	mux.Handle("DELETE /v1/schedules/{name}", a.guard(auth.Change, http.HandlerFunc(a.deleteSchedule)))
	mux.Handle("POST /v1/schedules/{name}/enable", a.guard(auth.Change, http.HandlerFunc(a.enableSchedule)))
	mux.Handle("POST /v1/schedules/{name}/disable", a.guard(auth.Change, http.HandlerFunc(a.disableSchedule)))
	mux.Handle("GET /v1/whoami", a.guard(auth.Read, http.HandlerFunc(a.whoami)))

	return a.observe(a.routingErrors(mux))
}

// routingErrors turns what ServeMux writes for a route it does not know into
// JSON.
//
// ServeMux answers an unknown path with "404 page not found" and a method it
// does not allow with "Method Not Allowed", both as text/plain. Every other
// answer this API gives is JSON with an error field, so a client that decodes
// every answer reports a parse failure and not the reason it was refused.
//
// The two are told apart by the content type, and not by the status. This
// API's own answers set application/json before the status line and
// ServeMux's do not, so a text/plain 404 or 405 is one nothing in this
// package wrote. Telling them apart by status alone would rewrite the 404
// that a job which is not there answers.
func (a *API) routingErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&routingWriter{ResponseWriter: w}, r)
	})
}

// routingWriter replaces the body of a routing refusal on its way out.
type routingWriter struct {
	http.ResponseWriter

	// replaced says the status line has gone with a body of this wrapper's
	// own, so whatever the mux writes next is dropped.
	replaced bool
}

func (w *routingWriter) WriteHeader(status int) {
	routing := status == http.StatusNotFound || status == http.StatusMethodNotAllowed
	if !routing || strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		w.ResponseWriter.WriteHeader(status)
		return
	}

	w.replaced = true
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.ResponseWriter.WriteHeader(status)

	// The Allow header ServeMux sets for a 405 is written before the status
	// line, so it is already out and a caller still learns which methods the
	// path takes.
	message := "no route answers that path"
	if status == http.StatusMethodNotAllowed {
		message = "that path does not answer that method, and the Allow header says which it does"
	}
	_ = json.NewEncoder(w.ResponseWriter).Encode(map[string]string{"error": message})
}

func (w *routingWriter) Write(b []byte) (int, error) {
	if w.replaced {
		// What the mux was about to write. Reported as written, because a
		// short write is an error to the caller and nothing went wrong here.
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
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
//
// It also gives the request its identifier, before anything else runs. A
// request refused by the guard has one, and so does a request that panics:
// those are the two a caller is most likely to ask about.
func (a *API) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &recorder{ResponseWriter: w}

		id := reqid.Of(r.Header.Get(reqid.Header))
		r = r.WithContext(reqid.Into(r.Context(), id))

		// On the answer before the handler runs, so that it is there whatever
		// the answer turns out to be. A header written after the status line
		// has gone is a header nobody receives.
		rec.Header().Set(reqid.Header, id)

		defer func() {
			if panicked := recover(); panicked != nil {
				// One bad request must not end the process and take every
				// other request in flight with it.
				a.logOf(r.Context()).Error("a request panicked",
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
			took := time.Since(started)
			a.opts.Metrics.HTTPRequest(route, r.Method, statusText(rec.status), took)

			// A line for every answer that refused, and none for the rest.
			//
			// Without this the identifier on a refusal finds nothing: the
			// only line the server wrote for a 404 was no line at all, and a
			// caller told to quote an identifier had nothing to quote it
			// against. That was found by reading a refusal out of quorractl
			// and searching the log of the server for what it printed.
			//
			// Refusals only, because a queue answering normally is the
			// normal case and a line for each would bury the ones that
			// matter. A queue refusing constantly is itself worth seeing.
			if rec.status >= 400 {
				level := slog.LevelInfo
				if rec.status >= 500 {
					level = slog.LevelWarn
				}
				a.logOf(r.Context()).Log(r.Context(), level, "request refused",
					"method", r.Method, "route", route, "code", rec.status, "took", took)
			}
		}()

		next.ServeHTTP(rec, r)
	})
}

// guard checks the API key.
func (a *API) guard(needs auth.Scope, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The header only.
		//
		// The old server also read the key from the query string, and its
		// dashboard put the key in every URL it fetched. A query string is
		// written to the access log of every proxy in front of this server,
		// kept in browser history, and sent in the Referer header to any
		// address a page links to. A key that has been in one is a key that
		// has to be replaced.
		key, found := a.opts.Keys.Lookup(r.Header.Get("X-API-Key"))
		if !found {
			a.fail(w, http.StatusUnauthorized, "the X-API-Key header is missing or wrong")
			return
		}

		// 403 and not 401. The key is real and the server knows whose it is;
		// what is missing is permission, and answering 401 would send a
		// caller to check a key that is working correctly.
		if !key.Scope.Allows(needs) {
			a.fail(w, http.StatusForbidden, fmt.Sprintf(
				"the key %q may %s, and this needs %s", key.Name, key.Scope, needs))
			return
		}

		// The caller travels on the request from here, so a handler does not
		// have to look the key up again and cannot look up a different one.
		next.ServeHTTP(w, r.WithContext(withCaller(r.Context(), key)))
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
func (a *API) failWith(ctx context.Context, w http.ResponseWriter, err error, what string) {
	a.failMissing(ctx, w, err, what, "no job carries that identifier")
}

// failMissing is failWith for a route about something that is not a job.
//
// The sentinel for a missing row is shared, so every route that used failWith
// answered "no job carries that identifier". On the schedule routes that
// named the wrong thing entirely: a person who asked for a schedule by name
// and got back a sentence about a job identifier looks for a job.
func (a *API) failMissing(ctx context.Context, w http.ResponseWriter, err error, what, missing string) {
	if errors.Is(err, store.ErrNotFound) {
		a.fail(w, http.StatusNotFound, missing)
		return
	}

	// 409 and not 400. The request is well formed and would be correct
	// against the same job in another state, so a client that retries after
	// the job moves is behaving sensibly. 400 tells it never to try again.
	if errors.Is(err, store.ErrWrongState) {
		a.fail(w, http.StatusConflict, err.Error())
		return
	}

	a.logOf(ctx).Error(what, "error", err)
	a.fail(w, http.StatusInternalServerError, what)
}

// logOf gives a log that names the request every line came from.
//
// The identifier is on the answer as well, so a caller with a failure can
// quote the one string that finds every line about it.
func (a *API) logOf(ctx context.Context) *slog.Logger {
	if id := reqid.From(ctx); id != "" {
		return a.log.With("request", id)
	}
	return a.log
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
