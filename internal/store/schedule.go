package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
)

// Schedule is a rule that produces jobs.
//
// It is not a job and never becomes one. It holds the rule, the zone the rule
// is read in, what to do about the windows it missed, and the shape of the
// job each firing submits.
type Schedule struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	// Cron is the five field rule, as it was written.
	Cron string `json:"cron"`

	// Timezone is an IANA name. Empty means UTC.
	Timezone string `json:"timezone"`

	// CatchUp is skip, all or none.
	CatchUp jobs.CatchUp `json:"catch_up"`

	// The job each firing submits.
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	Queue      string          `json:"queue"`
	Priority   int             `json:"priority"`
	MaxRetries *int            `json:"max_retries,omitempty"`

	// Enabled says whether the producing loop looks at this schedule. A
	// schedule that is off keeps its history and produces nothing.
	Enabled bool `json:"enabled"`

	// LastFiredAt is the window this schedule last produced a job for, and
	// not the moment the loop noticed. It is nil for one that has never
	// fired.
	LastFiredAt *time.Time `json:"last_fired_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NewSchedule is a schedule to store.
type NewSchedule struct {
	Name     string
	Cron     string
	Timezone string
	CatchUp  jobs.CatchUp

	Type       string
	Payload    json.RawMessage
	Queue      string
	Priority   int
	MaxRetries *int

	// Enabled defaults to true through Prepare. A schedule submitted switched
	// off is a real thing to want, and it is asked for rather than assumed.
	Enabled *bool
}

// Validate refuses a schedule that cannot work.
//
// Everything here is checked before a row is written, and every check is one
// the database could not make: a cron rule and a zone name are both text to
// PostgreSQL.
func (n NewSchedule) Validate() error {
	if strings.TrimSpace(n.Name) == "" {
		return Invalid("a schedule needs a name")
	}
	if len(n.Name) > 255 {
		return Invalid("the name is %d characters, and the column holds 255", len(n.Name))
	}
	if _, err := jobs.ParseCron(n.Cron); err != nil {
		return Invalid("%s", err)
	}
	if _, err := n.Location(); err != nil {
		return err
	}
	if !n.CatchUp.Valid() {
		return Invalid(
			"%q is not a catch up policy, and it must be skip, all or none", n.CatchUp)
	}

	// The job a firing submits has to be one a caller could have submitted.
	// A schedule that could ask for something a caller could not would be a
	// second way to make a job, and the two would drift.
	return NewJob{
		Type:       n.Type,
		Payload:    n.Payload,
		Queue:      n.Queue,
		Priority:   n.Priority,
		MaxRetries: n.MaxRetries,
	}.Validate()
}

// Location reads the time zone this schedule is written in.
//
// An IANA name and never an offset. "Every day at nine" moves twice a year,
// and a queue that stored UTC alone would get it wrong both times.
//
// The lookup can fail on a machine with no zone database, which is every
// container built FROM scratch. The error says so, because "unknown time
// zone Europe/Berlin" sends a reader to check the spelling of a name that is
// spelled correctly.
func (n NewSchedule) Location() (*time.Location, error) {
	name := strings.TrimSpace(n.Timezone)
	if name == "" {
		return time.UTC, nil
	}

	place, err := time.LoadLocation(name)
	if err != nil {
		return nil, Invalid(
			"cannot read the time zone %q: %s. A container built with no zone database has only UTC, "+
				"and importing time/tzdata puts one in the binary.", name, err)
	}
	return place, nil
}

// Location reads the time zone a stored schedule is written in.
func (s Schedule) Location() (*time.Location, error) {
	return NewSchedule{Timezone: s.Timezone}.Location()
}

// Rule reads the cron rule of a stored schedule.
func (s Schedule) Rule() (jobs.Cron, error) {
	c, err := jobs.ParseCron(s.Cron)
	if err != nil {
		return jobs.Cron{}, fmt.Errorf("store: the schedule %q holds %w", s.Name, err)
	}
	return c, nil
}

// Due says which jobs this schedule should produce, and what to record.
//
// The whole decision is jobs.Firings, which is pure and table tested. This
// puts the schedule's own zone around it: the rule is read in the zone it was
// written in, and the answers come back in UTC because that is what the rest
// of the store speaks.
func (s Schedule) Due(now time.Time) (at []time.Time, mark time.Time, dropped int, err error) {
	rule, err := s.Rule()
	if err != nil {
		return nil, time.Time{}, 0, err
	}
	place, err := s.Location()
	if err != nil {
		return nil, time.Time{}, 0, err
	}

	last := time.Time{}
	if s.LastFiredAt != nil {
		last = s.LastFiredAt.In(place)
	}

	windows, marked, dropped := jobs.Firings(rule, s.CatchUp, last, now.In(place))
	for _, one := range windows {
		at = append(at, one.UTC())
	}
	return at, marked.UTC(), dropped, nil
}

// FiringKey is the idempotency key for one firing.
//
// The schedule and the window it belongs to, and nothing else. Two servers
// running the producing loop at once then submit one job, and a loop that
// runs twice over the same window submits one job. That is the whole
// concurrency story for schedules, and it is the one the queue already has.
func FiringKey(scheduleID string, window time.Time) string {
	return "schedule:" + scheduleID + ":" + window.UTC().Format(time.RFC3339)
}

// PrepareSchedule fills in the defaults for a new schedule.
func (o Options) PrepareSchedule(n NewSchedule, id string, now time.Time) *Schedule {
	queue := n.Queue
	if queue == "" {
		queue = DefaultQueue
	}
	zone := strings.TrimSpace(n.Timezone)
	if zone == "" {
		zone = "UTC"
	}
	payload := n.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	enabled := true
	if n.Enabled != nil {
		enabled = *n.Enabled
	}

	return &Schedule{
		ID:         id,
		Name:       strings.TrimSpace(n.Name),
		Cron:       n.Cron,
		Timezone:   zone,
		CatchUp:    n.CatchUp,
		Type:       n.Type,
		Payload:    payload,
		Queue:      queue,
		Priority:   n.Priority,
		MaxRetries: n.MaxRetries,
		Enabled:    enabled,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

// MostSchedules bounds how many the producing loop reads in one tick.
//
// A deployment with more schedules than this has them worked through over
// several ticks, oldest name first, rather than in one query that holds a
// connection while it walks them all.
const MostSchedules = 1000
