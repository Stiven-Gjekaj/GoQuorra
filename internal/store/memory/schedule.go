package memory

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/google/uuid"
)

// CreateSchedule stores a repeat schedule.
func (s *Store) CreateSchedule(ctx context.Context, n store.NewSchedule) (*store.Schedule, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := n.Validate(); err != nil {
		return nil, err
	}

	made := s.opts.PrepareSchedule(n, uuid.NewString(), s.opts.Now())

	s.mu.Lock()
	defer s.mu.Unlock()

	// A name that is taken is refused rather than replacing what is there. A
	// schedule is something somebody refers to by name in a change request,
	// and quietly replacing one is how a rule nobody agreed to starts
	// producing jobs.
	if _, taken := s.schedules[made.Name]; taken {
		return nil, store.NameTaken("a schedule named %q already exists", made.Name)
	}
	s.schedules[made.Name] = made

	return cloneSchedule(made), nil
}

// Schedules lists the schedules, by name.
func (s *Store) Schedules(ctx context.Context, enabledOnly bool, limit int) ([]*store.Schedule, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*store.Schedule
	for _, one := range s.schedules {
		if enabledOnly && !one.Enabled {
			continue
		}
		out = append(out, cloneSchedule(one))
	}

	// By name, so that a deployment with more schedules than one tick reads
	// works through them in an order that does not change between ticks.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Schedule reads one by name.
func (s *Store) Schedule(ctx context.Context, name string) (*store.Schedule, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	one, found := s.schedules[strings.TrimSpace(name)]
	if !found {
		return nil, store.ErrNotFound
	}
	return cloneSchedule(one), nil
}

// SetScheduleEnabled switches a schedule on or off.
func (s *Store) SetScheduleEnabled(ctx context.Context, name string, enabled bool) (*store.Schedule, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	one, found := s.schedules[strings.TrimSpace(name)]
	if !found {
		return nil, store.ErrNotFound
	}
	one.Enabled = enabled
	one.UpdatedAt = s.opts.Now()

	return cloneSchedule(one), nil
}

// DeleteSchedule removes a schedule and leaves the jobs it produced.
func (s *Store) DeleteSchedule(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	name = strings.TrimSpace(name)
	if _, found := s.schedules[name]; !found {
		return store.ErrNotFound
	}
	delete(s.schedules, name)
	return nil
}

// MarkScheduleFired records the window a schedule has produced jobs up to.
func (s *Store) MarkScheduleFired(ctx context.Context, id string, window time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, one := range s.schedules {
		if one.ID != id {
			continue
		}
		// Never backwards. Two servers running the loop can mark the same
		// schedule, and the later window is the true one: moving it back
		// would catch windows up that have already been caught.
		at := window.UTC()
		if one.LastFiredAt != nil && !at.After(*one.LastFiredAt) {
			return nil
		}
		one.LastFiredAt = &at
		one.UpdatedAt = s.opts.Now()
		return nil
	}
	return store.ErrNotFound
}

// cloneSchedule returns a copy that shares nothing with the stored one.
func cloneSchedule(one *store.Schedule) *store.Schedule {
	out := *one
	if one.Payload != nil {
		out.Payload = append(json.RawMessage(nil), one.Payload...)
	}
	if one.MaxRetries != nil {
		retries := *one.MaxRetries
		out.MaxRetries = &retries
	}
	if one.LastFiredAt != nil {
		at := *one.LastFiredAt
		out.LastFiredAt = &at
	}
	return &out
}
