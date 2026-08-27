package store

import (
	"encoding/json"
	"math/rand/v2"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
)

// DefaultQueue is where a job goes when the caller names no queue.
const DefaultQueue = "default"

// MostAfter bounds how many jobs one job may wait for.
//
// Every one of them is read when the job is submitted, and the list is read
// again whenever one of them ends. An unbounded list is an unbounded amount
// of work on a path a caller controls, and sixty four is far past what any
// real chain of work needs.
const MostAfter = 64

// defaultJitter draws the number the backoff needs.
//
// math/rand/v2 is safe to call from several goroutines, which matters because
// every worker reporting a failure lands here at once. It is not a
// cryptographic source and does not need to be: this decides when a retry
// happens, not who may read it.
func defaultJitter() float64 { return rand.Float64() }

// Prepare turns a NewJob into the row that goes into storage.
//
// Both stores call this, so both fill in the same defaults. The version
// before the rebuild applied its defaults twice, once in the HTTP handler and
// once in the store, and the two disagreed about what an absent max_retries
// meant. It also wrote those defaults back into the caller's own struct,
// which changed a value the caller still held.
func (o Options) Prepare(n NewJob, id string, now time.Time) *Job {
	queue := n.Queue
	if queue == "" {
		queue = DefaultQueue
	}

	maxRetries := o.Policy.MaxRetries
	if n.MaxRetries != nil {
		maxRetries = *n.MaxRetries
	}

	payload := n.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}

	return &Job{
		ID:             id,
		IdempotencyKey: n.IdempotencyKey,
		ScheduleID:     n.ScheduleID,
		Type:           n.Type,
		Payload:        payload,
		Queue:          queue,
		Priority:       n.Priority,
		Status:         jobs.Pending,
		Attempts:       0,
		MaxRetries:     maxRetries,
		RunAt:          now.Add(n.Delay),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}
