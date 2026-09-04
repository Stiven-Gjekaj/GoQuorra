package client_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Stiven-Gjekaj/GoQuorra/client"
)

// A producer manages a schedule without dropping to raw HTTP.
//
// The six routes existed and this package reached none of them, so anybody
// wanting a schedule from Go wrote the requests by hand and kept their own
// copy of the shapes.
func TestAProducerManagesAScheduleThroughThisPackage(t *testing.T) {
	c := connect(t)
	ctx := t.Context()

	made, err := c.CreateSchedule(ctx, client.NewSchedule{
		Name:     "nightly-report",
		Cron:     "0 3 * * *",
		Timezone: "Europe/Rome",
		CatchUp:  client.CatchUpSkip,
		Type:     "report",
		Payload:  json.RawMessage(`{"for":"yesterday"}`),
		Queue:    "reports",
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if made.Name != "nightly-report" || made.Timezone != "Europe/Rome" {
		t.Errorf("the schedule came back as %+v", made)
	}
	if made.NextFiringAt == nil {
		t.Error("an enabled schedule does not say when it fires next")
	}

	listed, err := c.Schedules(ctx)
	if err != nil {
		t.Fatalf("Schedules: %v", err)
	}
	if len(listed) != 1 || listed[0].Name != "nightly-report" {
		t.Fatalf("the listing holds %d schedules", len(listed))
	}

	one, err := c.Schedule(ctx, "nightly-report")
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if string(one.Payload) != `{"for":"yesterday"}` {
		t.Errorf("the payload came back as %s", one.Payload)
	}

	off, err := c.DisableSchedule(ctx, "nightly-report")
	if err != nil {
		t.Fatalf("DisableSchedule: %v", err)
	}
	if off.Enabled {
		t.Error("the schedule is still switched on")
	}
	// A schedule that is switched off produces nothing, so there is no next
	// firing to give.
	if off.NextFiringAt != nil {
		t.Errorf("a schedule that is off says it fires at %s", off.NextFiringAt)
	}

	on, err := c.EnableSchedule(ctx, "nightly-report")
	if err != nil {
		t.Fatalf("EnableSchedule: %v", err)
	}
	if !on.Enabled {
		t.Error("the schedule is still switched off")
	}

	if err := c.DeleteSchedule(ctx, "nightly-report"); err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
	if _, err := c.Schedule(ctx, "nightly-report"); !errors.Is(err, client.ErrNotFound) {
		t.Errorf("reading a removed schedule gave %v, want ErrNotFound", err)
	}
}

// A name that is taken has its own error.
//
// The server answers 409 to this and to a job in the wrong state, and a
// caller that cannot tell them apart cannot decide whether to rename and
// retry or to give up.
func TestAScheduleNameThatIsTakenHasItsOwnError(t *testing.T) {
	c := connect(t)
	ctx := t.Context()

	first := client.NewSchedule{
		Name: "nightly", Cron: "0 3 * * *", CatchUp: client.CatchUpSkip, Type: "report",
	}
	if _, err := c.CreateSchedule(ctx, first); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	second := first
	second.Cron = "0 4 * * *"
	_, err := c.CreateSchedule(ctx, second)
	if !errors.Is(err, client.ErrNameTaken) {
		t.Fatalf("a name that is taken gave %v, want ErrNameTaken", err)
	}

	// And the first one is untouched, which is the point of refusing.
	kept, err := c.Schedule(ctx, "nightly")
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if kept.Cron != "0 3 * * *" {
		t.Errorf("the rule is now %q", kept.Cron)
	}
}

// A schedule with no catch up policy is refused by the server.
//
// This package does not choose one either. There is no answer that is right
// for every schedule: a nightly report that missed three days wants one run,
// and a billing run wants all three.
func TestAScheduleWithNoCatchUpPolicyIsRefused(t *testing.T) {
	c := connect(t)

	_, err := c.CreateSchedule(t.Context(), client.NewSchedule{
		Name: "no-policy", Cron: "0 3 * * *", Type: "report",
	})
	if err == nil {
		t.Fatal("a schedule with no catch up policy was stored")
	}
	if errors.Is(err, client.ErrNameTaken) {
		t.Errorf("it was refused as a name that is taken: %v", err)
	}
}

// A name holding a slash reaches the schedule it names.
//
// The server accepts one, and the routes put the name in the path, so a name
// put in raw reaches a different route or none at all.
func TestANameHoldingASlashReachesItsSchedule(t *testing.T) {
	c := connect(t)
	ctx := t.Context()

	if _, err := c.CreateSchedule(ctx, client.NewSchedule{
		Name: "team/nightly", Cron: "0 3 * * *", CatchUp: client.CatchUpSkip, Type: "report",
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	one, err := c.Schedule(ctx, "team/nightly")
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if one.Name != "team/nightly" {
		t.Errorf("the schedule came back as %q", one.Name)
	}
	if err := c.DeleteSchedule(ctx, "team/nightly"); err != nil {
		t.Errorf("DeleteSchedule: %v", err)
	}
}
