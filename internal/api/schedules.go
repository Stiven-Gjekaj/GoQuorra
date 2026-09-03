package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
)

// scheduleRequest is the body of POST /v1/schedules.
type scheduleRequest struct {
	Name     string `json:"name"`
	Cron     string `json:"cron"`
	Timezone string `json:"timezone"`

	// CatchUp has no default here. The record called this the part everybody
	// forgets and then argues about, so a caller says what it wants rather
	// than discovering what it got.
	CatchUp string `json:"catch_up"`

	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload"`
	Queue      string          `json:"queue"`
	Priority   int             `json:"priority"`
	MaxRetries *int            `json:"max_retries"`

	// Enabled is a pointer, because false is a real answer meaning "store it
	// switched off" and not an absent one.
	Enabled *bool `json:"enabled"`
}

func (a *API) createSchedule(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, a.opts.MaxBodyBytes)

	var req scheduleRequest
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		a.fail(w, http.StatusBadRequest, "the request body is not the JSON this endpoint expects: "+err.Error())
		return
	}

	if strings.TrimSpace(req.CatchUp) == "" {
		a.fail(w, http.StatusBadRequest,
			"catch_up is required, and it must be skip, all or none. It says what happens to the windows a "+
				"schedule missed while the server was down, and there is no answer that is right for every schedule.")
		return
	}
	policy, err := jobs.ParseCatchUp(req.CatchUp)
	if err != nil {
		a.fail(w, http.StatusBadRequest, err.Error())
		return
	}

	// A schedule produces jobs into a queue, so it is a way of writing to
	// one and answers the same rule.
	if !a.mayWriteTo(w, r, req.Queue) {
		return
	}

	made, err := a.opts.Store.CreateSchedule(r.Context(), store.NewSchedule{
		Name:       req.Name,
		Cron:       req.Cron,
		Timezone:   req.Timezone,
		CatchUp:    policy,
		Type:       req.Type,
		Payload:    req.Payload,
		Queue:      req.Queue,
		Priority:   req.Priority,
		MaxRetries: req.MaxRetries,
		Enabled:    req.Enabled,
	})
	if err != nil {
		// Everything the store refuses here is the caller's to fix: a rule
		// that is not a rule, a zone that is not a zone, or a name that is
		// taken. None of them is a fault underneath.
		if isClientMistake(err) || strings.Contains(err.Error(), "already exists") {
			a.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		a.failWith(r.Context(), w, err, "cannot store the schedule")
		return
	}

	caller := callerOf(r.Context())
	a.logOf(r.Context()).Info("schedule stored", "schedule", made.Name, "cron", made.Cron,
		"timezone", made.Timezone, "catch_up", made.CatchUp, "by", caller.Name)

	w.Header().Set("Location", "/v1/schedules/"+made.Name)
	a.send(w, http.StatusCreated, a.withNextFiring(made))
}

func (a *API) listSchedules(w http.ResponseWriter, r *http.Request) {
	limit, err := readLimit(r, 100, store.MostSchedules)
	if err != nil {
		a.fail(w, http.StatusBadRequest, err.Error())
		return
	}

	found, err := a.opts.Store.Schedules(r.Context(), false, limit)
	if err != nil {
		a.failWith(r.Context(), w, err, "cannot list the schedules")
		return
	}

	rows := make([]map[string]any, 0, len(found))
	for _, one := range found {
		rows = append(rows, a.withNextFiring(one))
	}
	a.send(w, http.StatusOK, map[string]any{"schedules": rows})
}

func (a *API) getSchedule(w http.ResponseWriter, r *http.Request) {
	one, err := a.opts.Store.Schedule(r.Context(), r.PathValue("name"))
	if err != nil {
		a.failWith(r.Context(), w, err, "cannot read the schedule")
		return
	}
	a.send(w, http.StatusOK, a.withNextFiring(one))
}

func (a *API) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := a.opts.Store.DeleteSchedule(r.Context(), name); err != nil {
		a.failWith(r.Context(), w, err, "cannot remove the schedule")
		return
	}

	caller := callerOf(r.Context())
	a.logOf(r.Context()).Info("schedule removed", "schedule", name, "by", caller.Name)

	// The jobs it produced stay, and the answer says so. A caller who
	// expected them to go should find that out here rather than a week later.
	a.send(w, http.StatusOK, map[string]any{
		"removed": name,
		"note":    "the jobs this schedule produced are kept, because they are work that happened",
	})
}

func (a *API) enableSchedule(w http.ResponseWriter, r *http.Request)  { a.switchSchedule(w, r, true) }
func (a *API) disableSchedule(w http.ResponseWriter, r *http.Request) { a.switchSchedule(w, r, false) }

func (a *API) switchSchedule(w http.ResponseWriter, r *http.Request, on bool) {
	one, err := a.opts.Store.SetScheduleEnabled(r.Context(), r.PathValue("name"), on)
	if err != nil {
		a.failWith(r.Context(), w, err, "cannot switch the schedule")
		return
	}

	caller := callerOf(r.Context())
	a.logOf(r.Context()).Info("schedule switched", "schedule", one.Name, "enabled", on, "by", caller.Name)
	a.send(w, http.StatusOK, a.withNextFiring(one))
}

// withNextFiring renders a schedule and says when it fires next.
//
// Worked out on the server and not left to the caller. The caller would need
// a cron parser and the server's clock, and a dashboard that computed it in
// a browser would answer in whatever zone the reader's machine is set to.
//
// It is absent for a schedule that is switched off, and for one whose rule
// names a day that never comes: "0 0 30 2 *" is the thirtieth of February,
// and the honest answer to when it fires next is nothing at all.
func (a *API) withNextFiring(one *store.Schedule) map[string]any {
	row := map[string]any{
		"id":       one.ID,
		"name":     one.Name,
		"cron":     one.Cron,
		"timezone": one.Timezone,
		"catch_up": one.CatchUp,
		"type":     one.Type,
		"payload":  one.Payload,
		"queue":    one.Queue,
		"priority": one.Priority,
		"enabled":  one.Enabled,

		"created_at": one.CreatedAt,
		"updated_at": one.UpdatedAt,
	}
	if one.MaxRetries != nil {
		row["max_retries"] = *one.MaxRetries
	}
	if one.LastFiredAt != nil {
		row["last_fired_at"] = *one.LastFiredAt
	}
	if !one.Enabled {
		return row
	}

	rule, err := one.Rule()
	if err != nil {
		row["error"] = err.Error()
		return row
	}
	place, err := one.Location()
	if err != nil {
		row["error"] = err.Error()
		return row
	}

	if next, found := rule.Next(a.opts.Now().In(place)); found {
		row["next_firing_at"] = next.UTC()
	}
	return row
}
