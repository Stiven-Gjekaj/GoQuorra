package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Schedule is a rule that produces jobs on its own.
//
// The shape is the JSON the API answers with. Nothing here holds a type from
// inside the repository, so a caller depends on the protocol and not on how
// the server stores a schedule.
type Schedule struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	// Cron is the five field rule: minute, hour, day of month, month, day of
	// week.
	Cron string `json:"cron"`

	// Timezone is the zone the rule is read in. A rule is about a clock on a
	// wall, and the wall clock changes twice a year in most places.
	Timezone string `json:"timezone"`

	// CatchUp is "skip", "all" or "none". It says what happens to the
	// windows a schedule missed while the server was down.
	CatchUp string `json:"catch_up"`

	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload"`
	Queue      string          `json:"queue"`
	Priority   int             `json:"priority"`
	MaxRetries int             `json:"max_retries"`

	Enabled bool `json:"enabled"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// LastFiredAt is the window this schedule has produced jobs up to, and
	// is absent until it has produced any.
	LastFiredAt *time.Time `json:"last_fired_at,omitempty"`

	// NextFiringAt is when it fires next, worked out by the server.
	//
	// Absent for a schedule that is switched off, and for one whose rule
	// names a day that never comes: "0 0 30 2 *" is the thirtieth of
	// February. It is worked out on the server because a caller doing it
	// here would answer in whatever zone this machine is set to.
	NextFiringAt *time.Time `json:"next_firing_at,omitempty"`
}

// NewSchedule asks for a schedule.
type NewSchedule struct {
	// Name is how everything else refers to this schedule. A name that is
	// taken is refused rather than replacing what is there.
	Name string `json:"name"`

	// Cron is the five field rule.
	Cron string `json:"cron"`

	// Timezone is a name from the zone database, such as "Europe/Rome". An
	// empty one means UTC.
	Timezone string `json:"timezone,omitempty"`

	// CatchUp is required, and the server refuses a request without it.
	//
	// There is no answer that is right for every schedule, so this package
	// does not choose one either. A nightly report that missed three days
	// wants one run and not three, and a billing run wants all three.
	CatchUp string `json:"catch_up"`

	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	Queue      string          `json:"queue,omitempty"`
	Priority   int             `json:"priority,omitempty"`
	MaxRetries *int            `json:"max_retries,omitempty"`

	// Enabled is a pointer because false is a real answer, meaning store it
	// switched off, and not an absent one.
	Enabled *bool `json:"enabled,omitempty"`
}

// The three answers to what happens to a window a schedule missed.
const (
	// CatchUpSkip runs one job for every window that was missed, at most
	// once per window, and is what most schedules want.
	CatchUpSkip = "skip"

	// CatchUpAll runs every missed window.
	CatchUpAll = "all"

	// CatchUpNone runs nothing for a missed window and starts again at the
	// next one.
	CatchUpNone = "none"
)

// CreateSchedule stores a schedule.
//
// A name that is taken is refused with ErrNameTaken rather than replacing
// what is there. A schedule is something somebody refers to by name in a
// change request, and quietly replacing one is how a rule nobody agreed to
// starts producing jobs.
func (c *Client) CreateSchedule(ctx context.Context, n NewSchedule) (*Schedule, error) {
	var made Schedule
	err := c.call(ctx, http.MethodPost, "/v1/schedules", n, &made)
	if err != nil {
		// The server answers 409 to a name that is taken and to a job in the
		// wrong state, and the shared reader turns both into ErrWrongState.
		// This is the one call where 409 can only mean the name, so it is
		// named here rather than by reading the sentence the server wrote.
		if errors.Is(err, ErrWrongState) {
			return nil, fmt.Errorf("%w: %s", ErrNameTaken, err)
		}
		return nil, err
	}
	return &made, nil
}

// Schedules lists the schedules, by name.
//
// A key limited to queues is answered about its own and nothing else.
func (c *Client) Schedules(ctx context.Context) ([]Schedule, error) {
	var answer struct {
		Schedules []Schedule `json:"schedules"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/schedules", nil, &answer); err != nil {
		return nil, err
	}
	return answer.Schedules, nil
}

// Schedule reads one by name.
//
// A name this key may not reach answers ErrNotFound, the same as a name that
// is not there. That is the server's answer and this package passes it on.
func (c *Client) Schedule(ctx context.Context, name string) (*Schedule, error) {
	var one Schedule
	if err := c.call(ctx, http.MethodGet, schedulePath(name), nil, &one); err != nil {
		return nil, err
	}
	return &one, nil
}

// EnableSchedule switches a schedule on.
func (c *Client) EnableSchedule(ctx context.Context, name string) (*Schedule, error) {
	return c.switchSchedule(ctx, name, "enable")
}

// DisableSchedule switches a schedule off.
//
// Switching off rather than removing, because removing takes the rule and the
// reason it existed with it.
func (c *Client) DisableSchedule(ctx context.Context, name string) (*Schedule, error) {
	return c.switchSchedule(ctx, name, "disable")
}

func (c *Client) switchSchedule(ctx context.Context, name, verb string) (*Schedule, error) {
	var one Schedule
	if err := c.call(ctx, http.MethodPost, schedulePath(name)+"/"+verb, nil, &one); err != nil {
		return nil, err
	}
	return &one, nil
}

// DeleteSchedule removes a schedule.
//
// The jobs it produced stay. They are work that happened, and a schedule
// going away does not unmake them.
func (c *Client) DeleteSchedule(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodDelete, schedulePath(name), nil, nil)
}

// schedulePath escapes a name into the path.
//
// A name is chosen by whoever writes it and the server accepts a slash in
// one, so a name put in raw would reach a different route or none at all.
func schedulePath(name string) string {
	return "/v1/schedules/" + url.PathEscape(name)
}
