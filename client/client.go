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
}

// Job is a job as the server holds it.
type Job struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload"`
	Queue    string          `json:"queue"`
	Priority int             `json:"priority"`

	// Status is pending, leased, succeeded, dead or cancelled.
	Status string `json:"status"`

	Attempts   int    `json:"attempts"`
	MaxRetries int    `json:"max_retries"`
	LastError  string `json:"last_error,omitempty"`

	// Result is what the handler produced, if it produced anything.
	Result json.RawMessage `json:"result,omitempty"`

	IdempotencyKey string `json:"idempotency_key,omitempty"`

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
	RunAt  time.Time `json:"run_at"`
}

// Submit stores a job.
//
// The returned job carries the identifier, the queue and the time it may run.
// It does not carry the payload back, because the server does not send it: ask
// for the job with Get when the whole of it is wanted.
func (c *Client) Submit(ctx context.Context, n NewJob) (*Job, error) {
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

	var answer submitted
	if err := c.call(ctx, http.MethodPost, "/v1/jobs", body, &answer); err != nil {
		return nil, err
	}

	return &Job{
		ID:     answer.ID,
		Type:   n.Type,
		Status: answer.Status,
		Queue:  answer.Queue,
		RunAt:  answer.RunAt,
	}, nil
}

// Get reads one job.
func (c *Client) Get(ctx context.Context, id string) (*Job, error) {
	var job Job
	if err := c.call(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(id), nil, &job); err != nil {
		return nil, err
	}
	return &job, nil
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
	for name, value := range map[string]string{
		"queue": f.Queue, "status": f.Status, "type": f.Type, "before": f.Cursor,
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
		return statusError(response.StatusCode, raw)
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("quorra: the answer is not the JSON this package expects: %w", err)
	}
	return nil
}

// statusError turns a refusal into an error a caller can test.
func statusError(code int, raw []byte) error {
	message := strings.TrimSpace(string(raw))

	// The server explains itself in the body, so the sentence it wrote is
	// worth more than the status line.
	var answer struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &answer); err == nil && answer.Error != "" {
		message = answer.Error
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
