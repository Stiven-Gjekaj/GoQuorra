package api

import (
	"context"
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

	// After names jobs this one waits for. Every one has to already exist,
	// which is what makes a cycle impossible.
	After []string `json:"after"`
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

	if !a.mayWriteTo(w, r, req.Queue) {
		return
	}

	job, created, err := a.opts.Store.Create(r.Context(), store.NewJob{
		Type:       req.Type,
		Payload:    req.Payload,
		Queue:      req.Queue,
		Priority:   req.Priority,
		Delay:      time.Duration(req.DelaySeconds) * time.Second,
		MaxRetries: req.MaxRetries,

		After: req.After,

		// The header wins over the body when both are set, because a proxy or
		// a client library adds the header and the body is the application's.
		IdempotencyKey: firstNonEmpty(r.Header.Get("Idempotency-Key"), req.IdempotencyKey),
	})
	if err != nil {
		// A job the store refuses is the client's mistake, and the store says
		// which field. Answering 500 to it, as the old handler did, sends the
		// client looking for a fault on this side.
		// A job that waits for one that is not there is the caller's mistake,
		// and 400 rather than 404: the route exists, and the job the caller
		// asked to create is not the thing that is missing.
		if errors.Is(err, store.ErrNotFound) || isClientMistake(err) {
			a.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		a.logOf(r.Context()).Error("cannot store a job", "error", err)
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
		a.logOf(r.Context()).Info("job accepted", "job", job.ID, "type", job.Type, "queue", job.Queue)
	} else {
		a.logOf(r.Context()).Info("job already submitted under this key", "job", job.ID, "key", job.IdempotencyKey)
	}

	w.Header().Set("Location", "/v1/jobs/"+job.ID)
	answer := map[string]any{
		"id":     job.ID,
		"status": job.Status,
		"queue":  job.Queue,
		"run_at": job.RunAt,
	}
	// Only when there is one. A caller that named no parent gets exactly the
	// answer it got before this field existed.
	if len(job.After) > 0 {
		answer["after"] = job.After
	}
	a.send(w, code, answer)
}

// createMany handles POST /v1/jobs/bulk.
//
// One request for many jobs. A producer with a thousand rows to queue
// otherwise makes a thousand requests, and the round trips cost more than the
// work does.
//
// Each job is stored on its own and the answer says what happened to each.
// One transaction for the batch was the alternative and is wrong here: jobs
// are independent, and one bad payload would lose the nine hundred and
// ninety nine good ones.
func (a *API) createMany(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, a.opts.MaxBodyBytes)

	var req struct {
		Jobs []createRequest `json:"jobs"`
	}
	decoder := json.NewDecoder(body)
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

	if len(req.Jobs) == 0 {
		a.fail(w, http.StatusBadRequest, "the request names no jobs")
		return
	}
	if len(req.Jobs) > bulkSubmitMost {
		a.fail(w, http.StatusBadRequest, fmt.Sprintf(
			"the request holds %d jobs, and one request may carry %d", len(req.Jobs), bulkSubmitMost))
		return
	}

	results := make([]map[string]any, 0, len(req.Jobs))
	created := 0
	failed := 0

	for i, one := range req.Jobs {
		if refusal := refusedQueue(r, one.Queue); refusal != "" {
			results = append(results, map[string]any{"index": i, "error": refusal})
			failed++
			continue
		}

		job, made, err := a.opts.Store.Create(r.Context(), store.NewJob{
			Type:           one.Type,
			Payload:        one.Payload,
			Queue:          one.Queue,
			Priority:       one.Priority,
			Delay:          time.Duration(one.DelaySeconds) * time.Second,
			MaxRetries:     one.MaxRetries,
			After:          one.After,
			IdempotencyKey: one.IdempotencyKey,
		})
		if err != nil {
			// A job the store refuses is reported beside the others rather
			// than ending the request. The caller learns which of its rows
			// is wrong, which is the answer it needs to fix them.
			if !errors.Is(err, store.ErrNotFound) && !isClientMistake(err) {
				a.logOf(r.Context()).Error("cannot store a job", "error", err)
				a.fail(w, http.StatusInternalServerError, "cannot store the jobs")
				return
			}
			results = append(results, map[string]any{"index": i, "error": err.Error()})
			failed++
			continue
		}

		if made {
			created++
			a.opts.Metrics.JobCreated()
		}
		results = append(results, map[string]any{
			"index":   i,
			"id":      job.ID,
			"status":  job.Status,
			"queue":   job.Queue,
			"run_at":  job.RunAt,
			"created": made,
		})
	}

	a.logOf(r.Context()).Info("jobs accepted in one request", "created", created, "refused", failed)

	// 200 and not 201, whatever the counts.
	//
	// The request succeeded: it was read, and every job in it was answered
	// for. 201 would claim the whole thing was created, which is not true
	// when one row was refused, and 400 would claim none of it was, which is
	// not true when nine hundred were stored.
	a.send(w, http.StatusOK, map[string]any{
		"created": created,
		"refused": failed,
		"results": results,
	})
}

// bulkSubmitMost bounds how many jobs one request may carry.
//
// The body size is bounded too, so this is the second of two limits. It
// matters because a body of a thousand tiny jobs fits easily and is a
// thousand round trips to the database.
const bulkSubmitMost = 1000

// whoami handles GET /v1/whoami.
//
// It answers the name and the scope of the key that asked, and nothing about
// any other key. A caller holding a secret out of a configuration file has no
// other way to find out which of several it holds, or whether it may write,
// short of trying something that changes a job and reading the refusal.
//
// It needs only read, because a key that cannot change anything still has to
// be able to ask what it is.
func (a *API) whoami(w http.ResponseWriter, r *http.Request) {
	caller := callerOf(r.Context())

	// An empty list and not a missing field, because the two mean different
	// things and a client that walks the answer should not have to test for
	// null. Empty means every queue.
	queues := caller.Queues()
	if queues == nil {
		queues = []string{}
	}

	a.send(w, http.StatusOK, map[string]any{
		"name":   caller.Name,
		"scope":  caller.Scope.String(),
		"queues": queues,
	})
}

// heldByCaller reports whether the key that made a request may act on a
// queue, and answers the request when it may not.
//
// One helper and not a check written out at each route. Every one of them
// asks the same question, and the answer a caller gets has to be the same
// wherever it comes from.
//
// 404 and not 403. A key limited to its own queues learns nothing about what
// is in another one, not even that a job is there. The caller already holds
// the identifier it asked about, so the only thing 403 would add is the fact
// that the job exists.
func (a *API) heldByCaller(w http.ResponseWriter, r *http.Request, queue string) bool {
	if callerOf(r.Context()).MayUse(queue) {
		return true
	}
	a.fail(w, http.StatusNotFound, "no job carries that identifier")
	return false
}

// heldSchedule is the same rule for a schedule, and answers 404 for the same
// reason.
//
// A schedule is named by whoever made it, so a caller guessing names would
// learn from a 403 which of its guesses are real. It also names a queue, and
// a caller that cannot reach that queue has no business reading the payload
// the schedule carries into it.
func (a *API) heldSchedule(w http.ResponseWriter, r *http.Request, queue string) bool {
	if callerOf(r.Context()).MayUse(queue) {
		return true
	}
	a.fail(w, http.StatusNotFound, "no schedule carries that name")
	return false
}

// mayWriteTo reports whether the caller may put work in a queue, and answers
// the request when it may not.
//
// 403 and not the 404 the read side gives. The caller named this queue, so
// there is nothing to hide: it already knows the name it asked for, and being
// told that the key does not hold it is the only useful answer.
//
// The empty name is resolved first. A key limited to queues refuses the empty
// string on purpose, so a route that forgot to resolve would refuse a
// submission to the default queue rather than allow one anywhere.
func (a *API) mayWriteTo(w http.ResponseWriter, r *http.Request, queue string) bool {
	if refusal := refusedQueue(r, queue); refusal != "" {
		a.fail(w, http.StatusForbidden, refusal)
		return false
	}
	return true
}

// refusedQueue gives the sentence to answer with when the caller may not put
// work in a queue, and an empty string when it may.
//
// Split from mayWriteTo because the bulk submit reports a bad row beside the
// others rather than ending the request, and one row naming the wrong queue
// is a bad row and not a bad request.
func refusedQueue(r *http.Request, queue string) string {
	if queue == "" {
		queue = store.DefaultQueue
	}

	caller := callerOf(r.Context())
	if caller.MayUse(queue) {
		return ""
	}
	return fmt.Sprintf("the key %q may act on %s, and this names %s",
		caller.Name, strings.Join(caller.Queues(), ", "), queue)
}

// queuesOfCaller gives the queues a filter has to be narrowed to.
//
// An empty answer means every queue, which is what a key that names none
// holds.
func queuesOfCaller(r *http.Request) []string {
	return callerOf(r.Context()).Queues()
}

func (a *API) getJob(w http.ResponseWriter, r *http.Request) {
	job, err := a.opts.Store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failWith(r.Context(), w, err, "cannot read the job")
		return
	}
	if !a.heldByCaller(w, r, job.Queue) {
		return
	}
	a.send(w, http.StatusOK, job)
}

// jobAttempts handles GET /v1/jobs/{id}/attempts.
//
// A route under the job rather than a field on it. The history of a job that
// has retried for a day is longer than the job, and every listing carries
// jobs, so folding it into the job would make every page of every list carry
// it for nothing.
//
// A job that has not finished a run answers 200 with an empty list. Only a
// job that is not there is 404.
func (a *API) jobAttempts(w http.ResponseWriter, r *http.Request) {
	// The job first, so that a key which may not see it is answered before
	// its history is read.
	job, err := a.opts.Store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failWith(r.Context(), w, err, "cannot read what the job did")
		return
	}
	if !a.heldByCaller(w, r, job.Queue) {
		return
	}

	found, err := a.opts.Store.Attempts(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failWith(r.Context(), w, err, "cannot read what the job did")
		return
	}

	// An empty slice and not a nil one. JSON renders nil as null, and a
	// client that walks the answer then has to test for it.
	if found == nil {
		found = []store.Attempt{}
	}
	a.send(w, http.StatusOK, map[string]any{"attempts": found})
}

// cancelJob handles POST /v1/jobs/{id}/cancel.
//
// A POST to a verb under the job, rather than a PATCH of its status. The
// status is not a field a client may set: there is no request that legally
// moves a job to succeeded, and an endpoint shaped like a field invites one.
//
// The name of the key that asked goes to the store and to the log line. The
// counter says how many jobs were cancelled, and on a queue that two teams
// share the next question is always which team.
func (a *API) cancelJob(w http.ResponseWriter, r *http.Request) {
	caller := callerOf(r.Context())

	// Read before acting, so that a key which may not see the job cannot
	// stop it either.
	before, err := a.opts.Store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failWith(r.Context(), w, err, "cannot cancel the job")
		return
	}
	if !a.heldByCaller(w, r, before.Queue) {
		return
	}

	job, err := a.opts.Store.Cancel(r.Context(), r.PathValue("id"), caller.Name)
	if err != nil {
		a.failWith(r.Context(), w, err, "cannot cancel the job")
		return
	}

	a.opts.Metrics.JobCancelled(caller.Name)
	a.logOf(r.Context()).Info("job cancelled", "job", job.ID, "type", job.Type, "queue", job.Queue, "by", caller.Name)
	a.send(w, http.StatusOK, job)
}

// reviveJob handles POST /v1/jobs/{id}/revive.
func (a *API) reviveJob(w http.ResponseWriter, r *http.Request) {
	caller := callerOf(r.Context())

	before, err := a.opts.Store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failWith(r.Context(), w, err, "cannot revive the job")
		return
	}
	if !a.heldByCaller(w, r, before.Queue) {
		return
	}

	job, err := a.opts.Store.Revive(r.Context(), r.PathValue("id"), caller.Name)
	if err != nil {
		a.failWith(r.Context(), w, err, "cannot revive the job")
		return
	}

	a.opts.Metrics.JobRevived(caller.Name)
	a.logOf(r.Context()).Info("job revived", "job", job.ID, "type", job.Type, "queue", job.Queue, "by", caller.Name)
	a.send(w, http.StatusOK, job)
}

// bulkRequest is the body of POST /v1/jobs/cancel and POST /v1/jobs/revive.
//
// The same fields the listing takes, so an operator narrows a listing until
// it shows what they mean and then sends the same narrowing here. A bulk
// action whose filter did not match the listing would be one nobody could
// check before running.
type bulkRequest struct {
	Queue  string `json:"queue"`
	Status string `json:"status"`
	Type   string `json:"type"`
	Worker string `json:"worker"`

	// Limit is required and has no default. A default would make the most
	// dangerous request in the API the shortest one to write.
	Limit int `json:"limit"`
}

// cancelMatching handles POST /v1/jobs/cancel.
func (a *API) cancelMatching(w http.ResponseWriter, r *http.Request) {
	a.bulk(w, r, "cancelled", a.opts.Store.CancelMatching, a.opts.Metrics.JobCancelled)
}

// reviveMatching handles POST /v1/jobs/revive.
func (a *API) reviveMatching(w http.ResponseWriter, r *http.Request) {
	a.bulk(w, r, "revived", a.opts.Store.ReviveMatching, a.opts.Metrics.JobRevived)
}

// bulk runs one bulk action.
//
// A POST to a verb under the collection, matching the verb under one job. The
// two paths differ in what they name and in nothing else.
func (a *API) bulk(
	w http.ResponseWriter,
	r *http.Request,
	done string,
	act func(context.Context, store.Filter, string) (int, error),
	count func(string),
) {
	body := http.MaxBytesReader(w, r.Body, a.opts.MaxBodyBytes)

	var req bulkRequest
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		a.fail(w, http.StatusBadRequest, "the request body is not the JSON this endpoint expects: "+err.Error())
		return
	}

	// Required, and named as required. A caller that forgets it is told what
	// to add rather than being given a default nobody chose for an action
	// that moves every job it can find.
	if req.Limit < 1 || req.Limit > bulkMost {
		a.fail(w, http.StatusBadRequest, fmt.Sprintf(
			"limit is required, and must be between 1 and %d. It bounds how many jobs this moves.", bulkMost))
		return
	}

	filter := store.Filter{
		Queue:  req.Queue,
		Queues: queuesOfCaller(r),
		Type:   req.Type,
		Worker: req.Worker,
		Limit:  req.Limit,
	}
	if req.Status != "" {
		status, err := jobs.ParseStatus(req.Status)
		if err != nil {
			a.fail(w, http.StatusBadRequest, fmt.Sprintf(
				"%q is not a status. It must be one of %s", req.Status, strings.Join(statusNames(), ", ")))
			return
		}
		filter.Status = status
	}

	caller := callerOf(r.Context())
	moved, err := act(r.Context(), filter, caller.Name)
	if err != nil {
		a.failWith(r.Context(), w, err, "cannot act on the jobs")
		return
	}

	// One count for each job, not one for the request. The counters answer
	// how many jobs an operator stopped, and a batch of four hundred that
	// counted as one would make the number useless the day it matters.
	for i := 0; i < moved; i++ {
		count(caller.Name)
	}
	a.logOf(r.Context()).Info("jobs "+done+" in one request", "count", moved, "by", caller.Name)

	a.send(w, http.StatusOK, map[string]any{"moved": moved})
}

// bulkMost bounds one bulk request.
//
// The store bounds nothing on its own: it does what the filter says. This is
// where the bound belongs, because it is a limit on what one request may do
// and not on what the queue can hold.
const bulkMost = 10000

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
		Queues: queuesOfCaller(r),
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
		a.failWith(r.Context(), w, err, "cannot list the jobs")
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
		a.failWith(r.Context(), w, err, "cannot count the queues")
		return
	}

	// Counted for every queue and shown for the caller's. A count of a queue
	// somebody cannot read is a fact about that queue.
	caller := callerOf(r.Context())
	kept := make([]store.QueueStat, 0, len(stats))
	for _, one := range stats {
		if caller.MayUse(one.Queue) {
			kept = append(kept, one)
		}
	}
	stats = kept
	a.send(w, http.StatusOK, map[string]any{"queues": stats})
}

// workers handles GET /v1/workers.
//
// It answers whether anything is out there. Every other question this API
// answers is about the jobs, and a queue with a thousand waiting jobs and no
// worker looks exactly like a queue that is simply busy.
//
// Each row carries how long the worker has been quiet, worked out here rather
// than left to the caller. The caller would need the server's clock to do it,
// and a caller a second out of step reads a fleet that is fine as one that
// stopped.
func (a *API) workers(w http.ResponseWriter, r *http.Request) {
	seen, err := a.opts.Store.Workers(r.Context())
	if err != nil {
		a.failWith(r.Context(), w, err, "cannot list the workers")
		return
	}

	now := a.opts.Now()
	caller := callerOf(r.Context())
	rows := make([]map[string]any, 0, len(seen))
	for _, one := range seen {
		// A worker is recorded per queue, so a key limited to its own queues
		// sees the workers on those and not the size of somebody's fleet.
		if !caller.MayUse(one.Queue) {
			continue
		}
		rows = append(rows, map[string]any{
			"id":            one.ID,
			"queue":         one.Queue,
			"first_seen_at": one.FirstSeenAt,
			"last_seen_at":  one.LastSeenAt,
			"idle_seconds":  one.Idle(now).Seconds(),
		})
	}
	a.send(w, http.StatusOK, map[string]any{"workers": rows})
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
		a.logOf(r.Context()).Warn("the readiness check failed", "error", err)
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
//
// One sentinel and nothing else. This function used to read the first seven
// characters of the message, which every sentinel in the store package
// carries as well, so it could not tell a refused job from a job that is not
// there. It also meant that rewording any message in that package moved a
// status code, with nothing anywhere failing.
func isClientMistake(err error) bool {
	return errors.Is(err, store.ErrInvalid)
}
