package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
)

// equalKeys compares two keys in constant time.
//
// A plain != returns as soon as two bytes differ, so the time it takes says
// how much of the key was right. That is enough to recover a key one byte at
// a time over a network, and the fix costs nothing.
func equalKeys(given, want string) bool {
	// Both are hashed to a fixed length first. ConstantTimeCompare returns 0
	// immediately when the lengths differ, so comparing the raw strings still
	// leaks the length of the key.
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare(fold(given), fold(want)) == 1
}

// createRequest is the body of POST /v1/jobs.
type createRequest struct {
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload"`
	Queue    string          `json:"queue"`
	Priority int             `json:"priority"`

	DelaySeconds int `json:"delay_seconds"`

	// A pointer, because zero is a real answer meaning do not retry. The old
	// handler used a plain int, so asking for no retries silently gave three.
	MaxRetries *int `json:"max_retries"`
}

func (a *API) createJob(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, a.opts.MaxBodyBytes)

	var req createRequest
	decoder := json.NewDecoder(body)

	// A field the server does not know is refused rather than ignored. A
	// client sending "maxRetries" instead of "max_retries" otherwise gets a
	// job with the default and no hint that its setting went nowhere.
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			a.fail(w, http.StatusRequestEntityTooLarge,
				"the request body is larger than "+strconv.FormatInt(a.opts.MaxBodyBytes, 10)+" bytes")
			return
		}
		a.fail(w, http.StatusBadRequest, "the request body is not the JSON this endpoint expects: "+err.Error())
		return
	}

	// One JSON value and not a stream of them. Without this, a body holding
	// two objects is accepted and the second is discarded in silence.
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		a.fail(w, http.StatusBadRequest, "the request body holds more than one JSON value")
		return
	}

	job, err := a.opts.Store.Create(r.Context(), store.NewJob{
		Type:       req.Type,
		Payload:    req.Payload,
		Queue:      req.Queue,
		Priority:   req.Priority,
		Delay:      time.Duration(req.DelaySeconds) * time.Second,
		MaxRetries: req.MaxRetries,
	})
	if err != nil {
		// A job the store refuses is the client's mistake, and the store says
		// which field. Answering 500 to it, as the old handler did, sends the
		// client looking for a fault on this side.
		if errors.Is(err, store.ErrNotFound) || isClientMistake(err) {
			a.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		a.log.Error("cannot store a job", "error", err)
		a.fail(w, http.StatusInternalServerError, "cannot store the job")
		return
	}

	a.opts.Metrics.JobCreated()
	a.log.Info("job accepted", "job", job.ID, "type", job.Type, "queue", job.Queue)

	w.Header().Set("Location", "/v1/jobs/"+job.ID)
	a.send(w, http.StatusCreated, map[string]any{
		"id":     job.ID,
		"status": job.Status,
		"queue":  job.Queue,
		"run_at": job.RunAt,
	})
}

func (a *API) getJob(w http.ResponseWriter, r *http.Request) {
	job, err := a.opts.Store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failWith(w, err, "cannot read the job")
		return
	}
	a.send(w, http.StatusOK, job)
}

// cancelJob handles POST /v1/jobs/{id}/cancel.
//
// A POST to a verb under the job, rather than a PATCH of its status. The
// status is not a field a client may set: there is no request that legally
// moves a job to succeeded, and an endpoint shaped like a field invites one.
func (a *API) cancelJob(w http.ResponseWriter, r *http.Request) {
	job, err := a.opts.Store.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failWith(w, err, "cannot cancel the job")
		return
	}

	a.opts.Metrics.JobCancelled()
	a.log.Info("job cancelled", "job", job.ID, "type", job.Type, "queue", job.Queue)
	a.send(w, http.StatusOK, job)
}

// reviveJob handles POST /v1/jobs/{id}/revive.
func (a *API) reviveJob(w http.ResponseWriter, r *http.Request) {
	job, err := a.opts.Store.Revive(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failWith(w, err, "cannot revive the job")
		return
	}

	a.opts.Metrics.JobRevived()
	a.log.Info("job revived", "job", job.ID, "type", job.Type, "queue", job.Queue)
	a.send(w, http.StatusOK, job)
}

func (a *API) recentJobs(w http.ResponseWriter, r *http.Request) {
	limit, err := readLimit(r, 50, 1000)
	if err != nil {
		a.fail(w, http.StatusBadRequest, err.Error())
		return
	}

	found, err := a.opts.Store.Recent(r.Context(), limit)
	if err != nil {
		a.failWith(w, err, "cannot read the recent jobs")
		return
	}

	// An empty slice and not a nil one. JSON renders nil as null, and a
	// client that walks the answer then has to test for it.
	if found == nil {
		found = []*store.Job{}
	}
	a.send(w, http.StatusOK, map[string]any{"jobs": found})
}

func (a *API) queueStats(w http.ResponseWriter, r *http.Request) {
	stats, err := a.opts.Store.QueueStats(r.Context())
	if err != nil {
		a.failWith(w, err, "cannot count the queues")
		return
	}

	if stats == nil {
		stats = []store.QueueStat{}
	}
	a.send(w, http.StatusOK, map[string]any{"queues": stats})
}

func (a *API) alive(w http.ResponseWriter, _ *http.Request) {
	a.send(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ready answers whether the server can do its work.
//
// It reaches the store, which is the difference that matters. A process that
// is running but cannot see its database must not be sent traffic, and a
// health check that only proves the process is running says it should be.
func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 2*time.Second)
	defer cancel()

	if _, err := a.opts.Store.QueueStats(ctx); err != nil {
		a.log.Warn("the readiness check failed", "error", err)
		a.send(w, http.StatusServiceUnavailable, map[string]string{
			"status": "the store cannot be reached",
		})
		return
	}
	a.send(w, http.StatusOK, map[string]string{"status": "ready"})
}

func readLimit(r *http.Request, fallback, most int) (int, error) {
	text := r.URL.Query().Get("limit")
	if text == "" {
		return fallback, nil
	}

	limit, err := strconv.Atoi(text)
	if err != nil {
		return 0, errors.New("limit is not a whole number")
	}
	if limit < 1 || limit > most {
		return 0, errors.New("limit must be between 1 and " + strconv.Itoa(most))
	}
	return limit, nil
}

// isClientMistake reports whether the store refused a job because of what was
// in it, rather than because something went wrong underneath.
func isClientMistake(err error) bool {
	var validation interface{ Error() string }
	if !errors.As(err, &validation) {
		return false
	}
	// The store's validation errors all begin with this prefix, and no error
	// from the database does.
	return len(err.Error()) > 7 && err.Error()[:7] == "store: "
}
