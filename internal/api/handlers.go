package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
)

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

	IdempotencyKey string `json:"idempotency_key"`
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

	job, created, err := a.opts.Store.Create(r.Context(), store.NewJob{
		Type:       req.Type,
		Payload:    req.Payload,
		Queue:      req.Queue,
		Priority:   req.Priority,
		Delay:      time.Duration(req.DelaySeconds) * time.Second,
		MaxRetries: req.MaxRetries,

		// The header wins over the body when both are set, because a proxy or
		// a client library adds the header and the body is the application's.
		IdempotencyKey: firstNonEmpty(r.Header.Get("Idempotency-Key"), req.IdempotencyKey),
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

	// 200 for a submission that stored nothing, 201 for one that did.
	//
	// A client retrying because it did not see the first answer gets the
	// first job back and a status that says so. Answering 201 to both would
	// tell it that it had just created something, which is the belief the key
	// exists to correct.
	code := http.StatusOK
	if created {
		code = http.StatusCreated
		a.opts.Metrics.JobCreated()
		a.log.Info("job accepted", "job", job.ID, "type", job.Type, "queue", job.Queue)
	} else {
		a.log.Info("job already submitted under this key", "job", job.ID, "key", job.IdempotencyKey)
	}

	w.Header().Set("Location", "/v1/jobs/"+job.ID)
	a.send(w, code, map[string]any{
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

// listJobs handles GET /v1/jobs.
//
// Filters are query parameters and paging is a cursor. The answer carries the
// cursor for the next page rather than making the caller know that it is the
// identifier of the last row, which keeps the shape of the cursor a detail
// that can change.
func (a *API) listJobs(w http.ResponseWriter, r *http.Request) {
	limit, err := readLimit(r, 50, 1000)
	if err != nil {
		a.fail(w, http.StatusBadRequest, err.Error())
		return
	}

	query := r.URL.Query()
	filter := store.Filter{
		Queue:  query.Get("queue"),
		Type:   query.Get("type"),
		Limit:  limit,
		Before: query.Get("before"),
		Worker: query.Get("worker"),
	}

	// order=soonest gives the order the queue works in, so the first page
	// holds what runs next rather than what was submitted last.
	switch wanted := query.Get("order"); wanted {
	case "", "newest":
		filter.Order = store.Newest
	case "soonest":
		filter.Order = store.Soonest
	default:
		a.fail(w, http.StatusBadRequest, fmt.Sprintf(
			"%q is not an order. It must be newest or soonest.", wanted))
		return
	}

	// due=now is the spelling for the question people actually ask, and the
	// handler resolves it. The store must not read a clock, and a caller
	// should not have to send a timestamp to ask what is ready.
	if wanted := query.Get("due"); wanted != "" {
		if wanted == "now" {
			filter.DueBy = a.opts.Now()
		} else {
			moment, err := time.Parse(time.RFC3339, wanted)
			if err != nil {
				a.fail(w, http.StatusBadRequest, fmt.Sprintf(
					"%q is not a moment. Use now, or a time in RFC 3339 such as 2026-01-02T15:04:05Z.", wanted))
				return
			}
			filter.DueBy = moment
		}
	}

	if wanted := query.Get("status"); wanted != "" {
		status, err := jobs.ParseStatus(wanted)
		if err != nil {
			// The valid ones are listed, because a caller who guessed wrong
			// once will guess wrong again without them.
			a.fail(w, http.StatusBadRequest, fmt.Sprintf(
				"%q is not a status. It must be one of %s", wanted, strings.Join(statusNames(), ", ")))
			return
		}
		filter.Status = status
	}

	found, err := a.opts.Store.List(r.Context(), filter)
	if err != nil {
		// A cursor naming no job is the caller's to fix, and 400 rather than
		// 404: the route exists, and answering 404 to a listing suggests it
		// does not.
		if errors.Is(err, store.ErrNotFound) {
			a.fail(w, http.StatusBadRequest,
				"the before cursor names no job, so the page cannot be placed. Ask again without it.")
			return
		}
		a.failWith(w, err, "cannot list the jobs")
		return
	}

	// An empty slice and not a nil one. JSON renders nil as null, and a
	// client that walks the answer then has to test for it.
	if found == nil {
		found = []*store.Job{}
	}

	answer := map[string]any{"jobs": found}

	// A cursor only when the page was full. A short page is the end, and
	// handing back a cursor there makes every caller ask one more time to
	// find that out.
	if len(found) == limit {
		answer["next_cursor"] = found[len(found)-1].ID
	}

	a.send(w, http.StatusOK, answer)
}

// statusNames lists the statuses for an error message.
func statusNames() []string {
	all := jobs.All()
	names := make([]string, len(all))
	for i, status := range all {
		names[i] = status.String()
	}
	return names
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

// firstNonEmpty gives the first value that is set.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
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
