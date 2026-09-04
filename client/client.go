// Package client submits work to a GoQuorra server.
//
// It is the other half of the worker package. A producer imports this one and
// a consumer imports that one, and neither has to know that the two talk over
// different protocols.
//
//	c, err := client.New(client.Config{
//		Server: "http://localhost:8080",
//		APIKey: os.Getenv("QUORRA_API_KEY"),
//	})
//	if err != nil {
//		return err
//	}
//	job, err := c.Submit(ctx, client.NewJob{
//		Type:    "email_send",
//		Payload: mail,
//		Key:     "welcome-" + user.ID,
//	})
//
// Nothing here holds a type from inside this repository. The shapes below are
// the JSON the API speaks, so a caller can depend on this package without
// depending on how the server stores anything.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Errors a caller is expected to tell apart.
var (
	// ErrNotFound means no job carries that identifier.
	ErrNotFound = errors.New("quorra: no such job")

	// ErrWrongState means the job exists and the action does not apply to it
	// in the state it is in. Cancelling a job that has already finished is
	// the common one. A caller that waits and asks again may succeed, which
	// is what makes this different from a refusal.
	ErrWrongState = errors.New("quorra: the job is in the wrong state")

	// ErrUnauthorized means the API key was missing or wrong.
	ErrUnauthorized = errors.New("quorra: the API key was refused")

	// ErrNameTaken means a schedule already carries the name asked for.
	//
	// The server answers 409 to this and to a job in the wrong state, so this
	// package tells them apart by which call was made rather than by reading
	// the sentence. CreateSchedule is the only call that can mean this one.
	ErrNameTaken = errors.New("quorra: the name is taken")
)

// Config sets a client up.
type Config struct {
	// Server is the address of the HTTP API.
	Server string

	// APIKey guards every route this package uses.
	APIKey string

	// HTTPClient is used for every request. Leave it nil for one with a
	// thirty second timeout.
	//
	// A timeout matters more than it looks. Without one a submission against
	// a server that has stopped answering blocks the goroutine that made it
	// for as long as the process lives.
	HTTPClient *http.Client
}

// Client talks to a GoQuorra server.
type Client struct {
	server string
	key    string
	http   *http.Client
}

// New builds a client.
func New(cfg Config) (*Client, error) {
	if cfg.Server == "" {
		cfg.Server = "http://localhost:8080"
	}
	if cfg.APIKey == "" {
		return nil, errors.New("quorra: an API key is required")
	}
	if _, err := url.Parse(cfg.Server); err != nil {
		return nil, fmt.Errorf("quorra: the server address is not usable: %w", err)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}

	return &Client{
		server: strings.TrimRight(cfg.Server, "/"),
		key:    cfg.APIKey,
		http:   cfg.HTTPClient,
	}, nil
}

// NewJob is a job to submit.
type NewJob struct {
	// Type decides which handler runs it. Required.
	Type string

	// Payload is marshalled to JSON and handed to the handler untouched.
	Payload any

	// Queue holds the job. Empty means "default".
	Queue string

	// Priority runs a job before others waiting in the same queue. Higher
	// first.
	Priority int

	// Delay holds the job back until it has passed.
	Delay time.Duration

	// MaxRetries counts the retries after the first attempt. A nil value
	// takes the server's default, and a pointer to zero means do not retry.
	MaxRetries *int

	// Key makes the submission safe to repeat. Sending the same key twice
	// gives back the job that already exists rather than making a second one.
	//
	// Use it whenever a submission can be retried, which is whenever the
	// network can drop an answer.
	Key string

	// After names jobs this one waits for. It runs when every one of them has
	// succeeded, and is cancelled when any one of them cannot.
	//
	// Every identifier has to name a job that already exists, which is what
	// makes a cycle impossible: a job cannot be created before itself. A job
	// naming one that is not there is refused with ErrNotFound.
	After []string
}

// Job is a job as the server holds it.
type Job struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload"`
	Queue    string          `json:"queue"`
	Priority int             `json:"priority"`

	// Status is blocked, pending, leased, succeeded, dead or cancelled.
	//
	// blocked means the job waits for the jobs in After. It is not pending,
	// because pending is a claim that the queue will hand the job out.
	Status string `json:"status"`

	// After names the jobs this one waits for, and is empty for a job that
	// waits for nothing.
	After []string `json:"after,omitempty"`

	Attempts   int    `json:"attempts"`
	MaxRetries int    `json:"max_retries"`
	LastError  string `json:"last_error,omitempty"`

	// Result is what the handler produced, if it produced anything.
	Result json.RawMessage `json:"result,omitempty"`

	IdempotencyKey string `json:"idempotency_key,omitempty"`

	// LeasedAt is when the worker now holding this job took it, and is nil
	// when nobody holds it. It says how long a run has been going, which is
	// what somebody looking for a job that is stuck wants to know.
	LeasedAt *time.Time `json:"leased_at,omitempty"`

	// ActedBy and ActedAt name the key that last cancelled or revived the
	// job, and when. Both are absent on a job nobody has acted on, and only
	// cancel and revive set them: the queue leasing, retrying or burying a
	// job is not a person acting.
	ActedBy string     `json:"acted_by,omitempty"`
	ActedAt *time.Time `json:"acted_at,omitempty"`

	RunAt     time.Time `json:"run_at"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Finished reports whether the job will move again.
func (j Job) Finished() bool {
	switch j.Status {
	case "succeeded", "dead", "cancelled":
		return true
	default:
		return false
	}
}

// Waiting reports whether the job is held back by the jobs it follows.
//
// It is not finished and it is not going to run yet, so a caller polling for
// a result has to tell it apart from pending: a job that stays blocked is
// waiting on somebody else's work, not on a worker.
func (j Job) Waiting() bool { return j.Status == "blocked" }

// Decode reads the result into a value.
func (j Job) Decode(into any) error {
	if len(j.Result) == 0 {
		return errors.New("quorra: the job carries no result")
	}
	return json.Unmarshal(j.Result, into)
}

// submitted is the short answer the server gives to a submission.
type submitted struct {
	ID     string    `json:"id"`
	Status string    `json:"status"`
	Queue  string    `json:"queue"`
	After  []string  `json:"after"`
	RunAt  time.Time `json:"run_at"`
}

// Submit stores a job.
//
// The returned job carries the identifier, the queue and the time it may run.
// It does not carry the payload back, because the server does not send it: ask
// for the job with Get when the whole of it is wanted.
func (c *Client) Submit(ctx context.Context, n NewJob) (*Job, error) {

	body, err := submission(n)
	if err != nil {
		return nil, err
	}

	var answer submitted
	if err := c.call(ctx, http.MethodPost, "/v1/jobs", body, &answer); err != nil {
		return nil, err
	}

	return &Job{
		ID:     answer.ID,
		Type:   n.Type,
		Status: answer.Status,
		Queue:  answer.Queue,
		After:  answer.After,
		RunAt:  answer.RunAt,
	}, nil
}

// submission turns a NewJob into the body the API takes.
//
// One place, used by both the single and the bulk paths, so that a field
// added to one is in the other. Written twice, the two would drift on the
// first field added to either.
func submission(n NewJob) (map[string]any, error) {
	if n.Type == "" {
		return nil, errors.New("quorra: a job needs a type")
	}
	if n.Delay < 0 {
		return nil, fmt.Errorf("quorra: the delay is %s, which puts the job in the past", n.Delay)
	}

	body := map[string]any{
		"type":          n.Type,
		"priority":      n.Priority,
		"delay_seconds": int(n.Delay.Seconds()),
	}
	if n.Payload != nil {
		encoded, err := json.Marshal(n.Payload)
		if err != nil {
			return nil, fmt.Errorf("quorra: the payload is not JSON: %w", err)
		}
		body["payload"] = json.RawMessage(encoded)
	}
	if n.Queue != "" {
		body["queue"] = n.Queue
	}
	if n.MaxRetries != nil {
		body["max_retries"] = *n.MaxRetries
	}
	if n.Key != "" {
		body["idempotency_key"] = n.Key
	}
	if len(n.After) > 0 {
		body["after"] = n.After
	}
	return body, nil
}

// Submitted is what happened to one job in a bulk submission.
type Submitted struct {
	// Index is the position of the job in the list that was sent, so a
	// caller can match a failure to the row that caused it.
	Index int `json:"index"`

	ID     string `json:"id"`
	Status string `json:"status"`
	Queue  string `json:"queue"`

	// Created is false for a job that an idempotency key already claimed.
	Created bool `json:"created"`

	// Error is what the server refused this job for, and is empty for one it
	// stored.
	Error string `json:"error"`
}

// SubmitMany stores many jobs in one request.
//
// A producer with a thousand rows to queue otherwise makes a thousand
// requests, and the round trips cost more than the work does.
//
// Each job is answered for on its own. A failure in the list is not an error
// from this call: jobs are independent, and one bad payload does not lose the
// nine hundred and ninety nine beside it. Read Error on each result.
func (c *Client) SubmitMany(ctx context.Context, jobs []NewJob) ([]Submitted, error) {
	if len(jobs) == 0 {
		return nil, errors.New("quorra: SubmitMany was given no jobs")
	}

	bodies := make([]map[string]any, 0, len(jobs))
	for _, n := range jobs {
		body, err := submission(n)
		if err != nil {
			return nil, err
		}
		bodies = append(bodies, body)
	}

	var answer struct {
		Results []Submitted `json:"results"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/jobs/bulk", map[string]any{"jobs": bodies}, &answer); err != nil {
		return nil, err
	}
	return answer.Results, nil
}

// Get reads one job.
func (c *Client) Get(ctx context.Context, id string) (*Job, error) {
	var job Job
	if err := c.call(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(id), nil, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

// Attempt is one finished run of a job.
type Attempt struct {
	JobID string `json:"job_id"`

	// Number counts from one. Reviving a job sets its count back to zero, so
	// a job that was buried and revived holds two runs numbered 1, and the
	// order of the list is what says which came first.
	Number int `json:"attempt"`

	// Worker is the identifier the worker gave when it took the lease, and is
	// empty for a lease that ran out with no worker named.
	Worker string `json:"worker,omitempty"`

	// Outcome is done, failed, expired or refused.
	Outcome string `json:"outcome"`

	// Error is what went wrong, and is empty for a run that finished.
	Error string `json:"error,omitempty"`

	// StartedAt is nil when the store does not know when the run began, which
	// happens for a job leased by a build older than the history itself.
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time  `json:"finished_at"`
}

// Took gives how long the run took, and whether that is known.
func (a Attempt) Took() (time.Duration, bool) {
	if a.StartedAt == nil {
		return 0, false
	}
	return a.FinishedAt.Sub(*a.StartedAt), true
}

// Attempts lists what a job did, oldest run first.
//
// An empty list means the job has not finished a run. A job that is not there
// gives ErrNotFound, which is the difference between "nothing has happened
// yet" and "there is nothing to ask about".
func (c *Client) Attempts(ctx context.Context, id string) ([]Attempt, error) {
	var answer struct {
		Attempts []Attempt `json:"attempts"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(id)+"/attempts", nil, &answer); err != nil {
		return nil, err
	}
	return answer.Attempts, nil
}

// Worker is one worker, asking about one queue.
type Worker struct {
	ID    string `json:"id"`
	Queue string `json:"queue"`

	FirstSeenAt time.Time `json:"first_seen_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`

	// IdleSeconds is how long ago the worker last asked for work, worked out
	// by the server. It is not computed here, because a caller a second out
	// of step with the server's clock reads a fleet that is fine as one that
	// stopped.
	IdleSeconds float64 `json:"idle_seconds"`
}

// Idle gives how long ago the worker last asked for work.
func (w Worker) Idle() time.Duration {
	return time.Duration(w.IdleSeconds * float64(time.Second))
}

// Workers lists the workers the queue has heard from, most recently first.
//
// An empty list means nothing is out there. That is worth checking before
// waiting on a job: a queue with a thousand waiting jobs and no worker looks
// exactly like a queue that is busy.
func (c *Client) Workers(ctx context.Context) ([]Worker, error) {
	var answer struct {
		Workers []Worker `json:"workers"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/workers", nil, &answer); err != nil {
		return nil, err
	}
	return answer.Workers, nil
}

// QueueCount is how many jobs one queue holds in one status.
//
// One row per pair, and not a queue with a map of statuses. That is the shape
// the API answers with, and this package holds the JSON the API speaks.
type QueueCount struct {
	Queue  string `json:"queue"`
	Status string `json:"status"`
	Count  int    `json:"count"`
}

// Queues counts the jobs, by queue and by status.
//
// A producer uses this to decide whether to submit more. Reading it through
// List means paging every job to count them, which is the wrong tool and gets
// slower as the queue does the thing worth watching for.
//
// A key limited to queues is answered about its own and nothing else, so a
// queue missing from the answer is one that is empty or one this key cannot
// reach. Whoami says which.
func (c *Client) Queues(ctx context.Context) ([]QueueCount, error) {
	var answer struct {
		Queues []QueueCount `json:"queues"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/queues", nil, &answer); err != nil {
		return nil, err
	}
	return answer.Queues, nil
}

// Waiting gives how many jobs in a queue have not started.
//
// Pending and blocked together, because both are work that has not run. A
// caller watching one number wants that one: a queue of a thousand blocked
// jobs is not idle, and reading pending alone says it is.
func Waiting(counts []QueueCount, queue string) int {
	total := 0
	for _, one := range counts {
		if one.Queue != queue {
			continue
		}
		if one.Status == "pending" || one.Status == "blocked" {
			total += one.Count
		}
	}
	return total
}

// Identity is what a key is and what it may do.
type Identity struct {
	// Name is what the server records against an action this key takes.
	Name string `json:"name"`

	// Scope is "read" or "write". A read key may ask questions and change
	// nothing.
	Scope string `json:"scope"`

	// Queues names the queues this key may act on. Empty means every queue.
	//
	// A key limited to queues reads an empty listing for one it cannot reach,
	// which looks exactly like an empty queue. A producer that finds nothing
	// where it expected work can ask this and learn that it was never
	// looking at the queue it meant.
	Queues []string `json:"queues"`
}

// MayUse reports whether this key may act on a queue.
//
// A key naming no queue holds every one, which is what a deployment that has
// not divided anything gets.
func (i Identity) MayUse(queue string) bool {
	if len(i.Queues) == 0 {
		return true
	}
	for _, held := range i.Queues {
		if held == queue {
			return true
		}
	}
	return false
}

// CanWrite reports whether this key may change a job.
//
// Any scope that carries write and not only the one that is exactly "write".
// A key holding everything answers "all", and a key that also leases answers
// "write+worker", and both may submit. Testing for the one word turned the
// most privileged key there is into one this package said could not write.
func (i Identity) CanWrite() bool {
	for _, part := range strings.Split(i.Scope, "+") {
		if part == "write" || part == "all" {
			return true
		}
	}
	return false
}

// Whoami says which key this client holds.
//
// A key read out of an environment variable gives no hint of which key it is,
// and one that may only read looks the same as one that may write until
// something is refused. A producer that starts up can ask once and refuse to
// run, rather than failing on the first submission an hour later.
func (c *Client) Whoami(ctx context.Context) (*Identity, error) {
	var who Identity
	if err := c.call(ctx, http.MethodGet, "/v1/whoami", nil, &who); err != nil {
		return nil, err
	}
	return &who, nil
}

// Cancel stops a job that has not finished.
func (c *Client) Cancel(ctx context.Context, id string) (*Job, error) {
	return c.act(ctx, id, "cancel")
}

// Revive puts a dead or cancelled job back in the queue with a fresh set of
// attempts.
func (c *Client) Revive(ctx context.Context, id string) (*Job, error) {
	return c.act(ctx, id, "revive")
}

// Many names the jobs a bulk action applies to.
//
// The same fields Filter takes, so a caller narrows a listing until it shows
// what they mean and then sends the same narrowing here.
type Many struct {
	Queue  string
	Status string
	Type   string
	Worker string

	// Limit is required and bounds how many jobs the action moves. There is
	// no default: a default would make the most dangerous call in this
	// package the shortest one to write.
	Limit int
}

// CancelMatching stops every job that Many names, and reports how many.
//
// A job it names that has already finished is skipped rather than refused. A
// bulk action against a moving queue will always race something.
func (c *Client) CancelMatching(ctx context.Context, m Many) (int, error) {
	return c.actOnMany(ctx, "cancel", m)
}

// ReviveMatching puts back every job that Many names, and reports how many.
//
// The reason this exists. Recovering a dead letter queue after fixing what
// broke is otherwise a loop that leaves the queue half done if it stops.
func (c *Client) ReviveMatching(ctx context.Context, m Many) (int, error) {
	return c.actOnMany(ctx, "revive", m)
}

func (c *Client) actOnMany(ctx context.Context, verb string, m Many) (int, error) {
	if m.Limit < 1 {
		return 0, errors.New("quorra: a bulk action needs a limit, which bounds how many jobs it moves")
	}

	body := map[string]any{"limit": m.Limit}
	for name, value := range map[string]string{
		"queue": m.Queue, "status": m.Status, "type": m.Type, "worker": m.Worker,
	} {
		if value != "" {
			body[name] = value
		}
	}

	var answer struct {
		Moved int `json:"moved"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/jobs/"+verb, body, &answer); err != nil {
		return 0, err
	}
	return answer.Moved, nil
}

func (c *Client) act(ctx context.Context, id, verb string) (*Job, error) {
	var job Job
	if err := c.call(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(id)+"/"+verb, nil, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

// Filter narrows a listing. A zero Filter asks for the newest jobs.
type Filter struct {
	Queue  string
	Status string
	Type   string
	Limit  int

	// Cursor continues a previous page. Take it from Page.Cursor.
	Cursor string

	// Worker keeps only the jobs that worker is holding right now. A finished
	// job belongs to nobody, whoever ran it.
	Worker string

	// DueBy keeps only the jobs that run at or before this moment. The zero
	// value keeps every job. Use Ready for the common case.
	DueBy time.Time

	// Ready keeps only the jobs the queue would hand out now: pending, and
	// due. It separates what is waiting for a worker from what is waiting out
	// a backoff.
	//
	// It overrides Status, because a job that is ready is pending by
	// definition and any other status would give an empty page.
	//
	// The server reads its own clock for this, which is the point: the two
	// machines do not have to agree on the time for the answer to be right.
	Ready bool

	// Soonest gives the job that runs first, first, which is the order the
	// queue itself works in. The default is the newest job first.
	Soonest bool
}

// Page is one page of a listing.
type Page struct {
	Jobs []Job `json:"jobs"`

	// Cursor asks for the page after this one. It is empty at the end.
	Cursor string `json:"next_cursor"`
}

// List reads one page of jobs, newest first.
func (c *Client) List(ctx context.Context, f Filter) (*Page, error) {
	query := url.Values{}
	if f.Limit > 0 {
		query.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.Ready {
		// The server resolves this against its own clock. Sending a moment
		// from here would make the answer depend on the two machines
		// agreeing about the time.
		query.Set("due", "now")

		// And pending, because a job the queue would hand out now is both.
		// A finished job keeps the run_at of its last attempt, so due alone
		// matches every job that has ever run.
		// The literal and not a constant from inside this repository. This
		// package deliberately holds no type from in here, so a caller
		// depends on it without depending on how the server stores anything.
		query.Set("status", "pending")
	} else if !f.DueBy.IsZero() {
		query.Set("due", f.DueBy.UTC().Format(time.RFC3339))
	}
	if f.Soonest {
		query.Set("order", "soonest")
	}
	for name, value := range map[string]string{
		"queue": f.Queue, "status": f.Status, "type": f.Type,
		"before": f.Cursor, "worker": f.Worker,
	} {
		if value != "" {
			query.Set(name, value)
		}
	}

	path := "/v1/jobs"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}

	var page Page
	if err := c.call(ctx, http.MethodGet, path, nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// Each walks every job a filter matches, following the pages.
//
// It stops when the walk function returns an error, and gives that error back.
// A caller wanting one page asks List instead.
func (c *Client) Each(ctx context.Context, f Filter, walk func(Job) error) error {
	if f.Limit <= 0 {
		f.Limit = 100
	}

	for {
		page, err := c.List(ctx, f)
		if err != nil {
			return err
		}
		for _, job := range page.Jobs {
			if err := walk(job); err != nil {
				return err
			}
		}
		if page.Cursor == "" {
			return nil
		}
		f.Cursor = page.Cursor
	}
}

// call sends one request and reads the answer.
func (c *Client) call(ctx context.Context, method, path string, body, into any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, c.server+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("X-API-Key", c.key)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("quorra: cannot reach %s: %w", c.server, err)
	}
	defer func() { _ = response.Body.Close() }()

	// Bounded. A server answering with something enormous should not be able
	// to exhaust the memory of every client that asked it a question.
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("quorra: cannot read the answer: %w", err)
	}

	if response.StatusCode >= 400 {
		return statusError(response.StatusCode, raw, response.Header.Get(requestHeader))
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("quorra: the answer is not the JSON this package expects: %w", err)
	}
	return nil
}

// requestHeader is where the server puts the identifier of the request.
//
// Written out rather than taken from internal/reqid. This package holds
// nothing from inside the repository on purpose, so that a caller depends on
// the JSON the API speaks and not on how the server is built, and one header
// name is a small price for keeping that true.
const requestHeader = "X-Request-Id"

// mostRequestID is the longest identifier that goes in an error message.
//
// The same bound the server puts on what a caller may send, so a value this
// package refuses is one the server would have refused as well.
const mostRequestID = 64

// statusError turns a refusal into an error a caller can test.
//
// The identifier of the request is put in the message. It is the one string
// that finds every line the server wrote while it was refusing, so a caller
// asking somebody to look has something to quote, and the caller does not
// have to know that such a thing exists to end up holding it.
func statusError(code int, raw []byte, request string) error {
	message := strings.TrimSpace(string(raw))

	// The server explains itself in the body, so the sentence it wrote is
	// worth more than the status line.
	var answer struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &answer); err == nil && answer.Error != "" {
		message = answer.Error
	}

	// Left out rather than trimmed when it is longer than the server would
	// ever send. An error message is read by a person, and a page of
	// identifier buries the sentence that says what went wrong.
	if request != "" && len(request) <= mostRequestID {
		message += " (request " + request + ")"
	}

	switch code {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, message)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrWrongState, message)
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: %s", ErrUnauthorized, message)
	default:
		return fmt.Errorf("quorra: the server answered %d: %s", code, message)
	}
}
