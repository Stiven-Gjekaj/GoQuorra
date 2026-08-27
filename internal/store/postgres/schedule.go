package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// scheduleColumns is the one list every read of a schedule uses.
const scheduleColumns = `id, name, cron, timezone, catch_up, job_type, payload,
	queue, priority, max_retries, enabled, last_fired_at, created_at, updated_at`

// CreateSchedule stores a repeat schedule.
func (s *Store) CreateSchedule(ctx context.Context, n store.NewSchedule) (*store.Schedule, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}

	made := s.opts.PrepareSchedule(n, uuid.NewString(), s.opts.Now())

	row := s.pool.QueryRow(ctx, `
		INSERT INTO schedules (id, name, cron, timezone, catch_up, job_type, payload,
		                       queue, priority, max_retries, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)
		ON CONFLICT (name) DO NOTHING
		RETURNING `+scheduleColumns,
		made.ID, made.Name, made.Cron, made.Timezone, string(made.CatchUp),
		made.Type, []byte(made.Payload), made.Queue, made.Priority,
		made.MaxRetries, made.Enabled, made.CreatedAt,
	)

	stored, err := scanSchedule(row)
	if errors.Is(err, pgx.ErrNoRows) {
		// DO NOTHING returns no row, which here can only mean the name was
		// already taken. Refused rather than replacing what is there: a
		// schedule is something somebody refers to by name in a change
		// request.
		return nil, fmt.Errorf("store: a schedule named %q already exists", made.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot store the schedule: %w", err)
	}
	return stored, nil
}

// Schedules lists the schedules, by name.
func (s *Store) Schedules(ctx context.Context, enabledOnly bool, limit int) ([]*store.Schedule, error) {
	if limit <= 0 {
		return nil, nil
	}

	where := "TRUE"
	if enabledOnly {
		// Written so that the partial index on enabled answers it.
		where = "enabled"
	}

	rows, err := s.pool.Query(ctx,
		`SELECT `+scheduleColumns+` FROM schedules WHERE `+where+` ORDER BY name LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot list the schedules: %w", err)
	}
	defer rows.Close()

	var out []*store.Schedule
	for rows.Next() {
		one, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: cannot read a schedule: %w", err)
		}
		out = append(out, one)
	}
	return out, rows.Err()
}

// Schedule reads one by name.
func (s *Store) Schedule(ctx context.Context, name string) (*store.Schedule, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+scheduleColumns+` FROM schedules WHERE name = $1`, strings.TrimSpace(name))

	one, err := scanSchedule(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot read the schedule: %w", err)
	}
	return one, nil
}

// SetScheduleEnabled switches a schedule on or off.
func (s *Store) SetScheduleEnabled(ctx context.Context, name string, enabled bool) (*store.Schedule, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE schedules SET enabled = $1, updated_at = $2 WHERE name = $3
		RETURNING `+scheduleColumns,
		enabled, s.opts.Now(), strings.TrimSpace(name),
	)

	one, err := scanSchedule(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot switch the schedule: %w", err)
	}
	return one, nil
}

// DeleteSchedule removes a schedule and leaves the jobs it produced.
func (s *Store) DeleteSchedule(ctx context.Context, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM schedules WHERE name = $1`, strings.TrimSpace(name))
	if err != nil {
		return fmt.Errorf("postgres: cannot remove the schedule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// MarkScheduleFired records the window a schedule has produced jobs up to.
func (s *Store) MarkScheduleFired(ctx context.Context, id string, window time.Time) error {
	if _, err := parseID(id); err != nil {
		return store.ErrNotFound
	}

	// Never backwards. Two servers running the producing loop can mark the
	// same schedule, and the later window is the true one: moving it back
	// would catch windows up that have already been caught.
	//
	// In the WHERE clause and not in a read before it, so the check and the
	// write are one statement and the two servers cannot land between them.
	tag, err := s.pool.Exec(ctx, `
		UPDATE schedules SET last_fired_at = $1, updated_at = $2
		WHERE id = $3 AND (last_fired_at IS NULL OR last_fired_at < $1)`,
		window.UTC(), s.opts.Now(), id,
	)
	if err != nil {
		return fmt.Errorf("postgres: cannot mark the schedule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either the schedule is gone or the window is not newer. The second
		// is the ordinary case and is not a failure, so this only reports a
		// missing schedule after looking.
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT TRUE FROM schedules WHERE id = $1`, id).Scan(&exists); err != nil {
			return store.ErrNotFound
		}
	}
	return nil
}

// scanSchedule builds a schedule from one row.
func scanSchedule(r row) (*store.Schedule, error) {
	var (
		one     store.Schedule
		catchUp string
		payload []byte
		fired   *time.Time
	)

	err := r.Scan(
		&one.ID, &one.Name, &one.Cron, &one.Timezone, &catchUp, &one.Type, &payload,
		&one.Queue, &one.Priority, &one.MaxRetries, &one.Enabled, &fired,
		&one.CreatedAt, &one.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	parsed, err := jobs.ParseCatchUp(catchUp)
	if err != nil {
		return nil, fmt.Errorf("the table holds %w", err)
	}
	one.CatchUp = parsed
	one.Payload = json.RawMessage(payload)
	if fired != nil {
		at := fired.UTC()
		one.LastFiredAt = &at
	}
	one.CreatedAt = one.CreatedAt.UTC()
	one.UpdatedAt = one.UpdatedAt.UTC()

	return &one, nil
}
