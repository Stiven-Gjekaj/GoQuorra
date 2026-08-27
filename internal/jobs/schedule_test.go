package jobs

import (
	"testing"
	"time"
)

// A schedule that has never fired starts from now.
//
// Reading the zero time as a missed window would make the first tick of a new
// schedule catch up from the year one.
func TestANewScheduleDoesNotCatchUpFromTheYearOne(t *testing.T) {
	c := mustCron(t, "0 * * * *")
	now := moment("2026-03-01 12:30")

	for _, policy := range CatchUps() {
		at, mark, dropped := Firings(c, policy, time.Time{}, now)
		if len(at) != 0 {
			t.Errorf("%s: a new schedule fired %d times at once", policy, len(at))
		}
		if dropped != 0 {
			t.Errorf("%s: a new schedule dropped %d windows", policy, dropped)
		}
		if !mark.Equal(now) {
			t.Errorf("%s: a new schedule is marked at %s, want now", policy, mark)
		}
	}
}

// Nothing is due between two firings.
func TestNothingIsDueBeforeTheNextWindow(t *testing.T) {
	c := mustCron(t, "0 * * * *")
	last := moment("2026-03-01 12:00")

	at, mark, _ := Firings(c, CatchUpSkip, last, moment("2026-03-01 12:30"))
	if len(at) != 0 {
		t.Errorf("a schedule fired %d times half way through its window", len(at))
	}
	if !mark.Equal(last) {
		t.Errorf("the mark moved to %s with nothing due", mark)
	}
}

// The weekend outage, which is the case the policy exists for.
//
// Hourly, last fired on Friday at noon, and the server comes back on Monday
// at noon. Seventy two windows were missed.
func TestTheWeekendOutage(t *testing.T) {
	c := mustCron(t, "0 * * * *")
	last := moment("2026-03-06 12:00")
	now := moment("2026-03-09 12:00")

	// skip: one firing, at the most recent missed window and not at now, so
	// a handler keyed on a window still gets a window.
	at, mark, dropped := Firings(c, CatchUpSkip, last, now)
	if len(at) != 1 {
		t.Fatalf("skip fired %d times, want 1", len(at))
	}
	if !at[0].Equal(now) {
		t.Errorf("skip fired at %s, want the most recent missed window", at[0])
	}
	if dropped != 71 {
		t.Errorf("skip dropped %d windows, want 71", dropped)
	}
	if !mark.Equal(now) {
		t.Errorf("skip marked %s, want the last window reached", mark)
	}

	// none: no firing at all, and the mark still moves. A policy that fired
	// nothing and left the mark where it was would catch the same windows up
	// on every tick for ever.
	at, mark, dropped = Firings(c, CatchUpNone, last, now)
	if len(at) != 0 {
		t.Errorf("none fired %d times", len(at))
	}
	if dropped != 72 {
		t.Errorf("none dropped %d windows, want 72", dropped)
	}
	if !mark.Equal(now) {
		t.Errorf("none marked %s, and the same windows come up again on the next tick", mark)
	}

	// all: every window, oldest first.
	at, _, dropped = Firings(c, CatchUpAll, last, now)
	if len(at) != 72 {
		t.Fatalf("all fired %d times, want 72", len(at))
	}
	if dropped != 0 {
		t.Errorf("all dropped %d windows with room for them", dropped)
	}
	if !at[0].Equal(moment("2026-03-06 13:00")) {
		t.Errorf("all started at %s, want the oldest missed window", at[0])
	}
	if !at[len(at)-1].Equal(now) {
		t.Errorf("all ended at %s, want the newest", at[len(at)-1])
	}
}

// A firing carries the window it belongs to, and not the moment the loop
// happened to notice.
//
// A handler keyed on a window needs the window. Firing at "now" would hand it
// whatever minute the background loop woke up on, which is not a window at
// all, and two servers running the loop a second apart would produce two
// different answers for the same missed hour.
func TestAFiringCarriesItsWindowAndNotTheMomentItWasNoticed(t *testing.T) {
	c := mustCron(t, "0 * * * *")
	last := moment("2026-03-01 09:00")

	// Half past, so the window and the moment of asking are different.
	now := moment("2026-03-01 11:30")

	at, mark, _ := Firings(c, CatchUpSkip, last, now)
	if len(at) != 1 {
		t.Fatalf("skip fired %d times, want 1", len(at))
	}
	if !at[0].Equal(moment("2026-03-01 11:00")) {
		t.Errorf("skip fired at %s, want the eleven o'clock window", at[0])
	}
	if !mark.Equal(moment("2026-03-01 11:00")) {
		t.Errorf("skip marked %s, want the window it fired for", mark)
	}

	// all is the same rule: every firing is a window.
	at, _, _ = Firings(c, CatchUpAll, last, now)
	if len(at) != 2 {
		t.Fatalf("all fired %d times, want 2", len(at))
	}
	for i, one := range at {
		if one.Minute() != 0 {
			t.Errorf("firing %d is at %s, which is not a window of this schedule", i, one)
		}
	}
}

// A catch up is bounded, and it keeps the newest.
//
// A schedule that has not fired for a year is nine thousand windows, and
// enqueueing nine thousand jobs because somebody restored an old backup is
// not recovery. The recent windows are the ones still worth doing.
func TestACatchUpIsBoundedAndKeepsTheNewest(t *testing.T) {
	c := mustCron(t, "0 * * * *")
	last := moment("2026-01-01 00:00")
	now := moment("2026-03-01 00:00")

	at, mark, dropped := Firings(c, CatchUpAll, last, now)
	if len(at) != MostCaughtUp {
		t.Fatalf("all fired %d times, and the bound is %d", len(at), MostCaughtUp)
	}

	// The count is the real number of windows that did not run, and not the
	// number that happened not to fit. An earlier version stopped walking at
	// the bound and reported one missed window for an outage of a hundred
	// and forty, which is a number nobody could act on.
	//
	// January and February 2026 are 31 and 28 days, so 59 days of hours.
	windows := 59 * 24
	if want := windows - MostCaughtUp; dropped != want {
		t.Errorf("the outage dropped %d windows, want %d", dropped, want)
	}

	// The newest kept, so the last one is the most recent window reached.
	if !at[len(at)-1].Equal(mark) {
		t.Errorf("the last firing is %s and the mark is %s", at[len(at)-1], mark)
	}

	// And they are in order, oldest first.
	for i := 1; i < len(at); i++ {
		if !at[i].After(at[i-1]) {
			t.Fatalf("firing %d is at %s, which is not after %s", i, at[i], at[i-1])
		}
	}
}

// The count of what did not run is the real number under every policy.
//
// It is what an operator reads to find out how much work was lost, so a
// number bounded by what happened to fit in memory would be worse than none.
func TestTheCountOfMissedWindowsIsTheRealNumber(t *testing.T) {
	c := mustCron(t, "0 * * * *")
	last := moment("2026-01-01 00:00")
	now := moment("2026-03-01 00:00")
	windows := 59 * 24

	cases := map[CatchUp]struct{ fired, dropped int }{
		CatchUpAll:  {MostCaughtUp, windows - MostCaughtUp},
		CatchUpSkip: {1, windows - 1},
		CatchUpNone: {0, windows},
	}
	for policy, want := range cases {
		at, _, dropped := Firings(c, policy, last, now)
		if len(at) != want.fired {
			t.Errorf("%s fired %d times, want %d", policy, len(at), want.fired)
		}
		if dropped != want.dropped {
			t.Errorf("%s dropped %d windows, want %d", policy, dropped, want.dropped)
		}
		// Every window is accounted for, whichever policy ran.
		if len(at)+dropped != windows {
			t.Errorf("%s accounts for %d of %d windows", policy, len(at)+dropped, windows)
		}
	}
}

// A policy the package does not know behaves as skip rather than as nothing.
//
// A row written by a newer build should keep the schedule running, and the
// safe reading of an unknown policy is the one almost every schedule means.
func TestAPolicyThePackageDoesNotKnowBehavesAsSkip(t *testing.T) {
	c := mustCron(t, "0 * * * *")
	last := moment("2026-03-01 00:00")
	now := moment("2026-03-01 05:00")

	at, _, _ := Firings(c, CatchUp("whatever-a-newer-build-wrote"), last, now)
	if len(at) != 1 {
		t.Errorf("an unknown policy fired %d times, want the one that skip gives", len(at))
	}

	if _, err := ParseCatchUp("whatever"); err == nil {
		t.Error("an unknown policy was accepted where it is read")
	}
	for _, policy := range CatchUps() {
		if got, err := ParseCatchUp(string(policy)); err != nil || got != policy {
			t.Errorf("ParseCatchUp(%q) = %q, %v", policy, got, err)
		}
	}
}

// A schedule that never fires again moves nothing and answers nothing.
func TestAScheduleThatNeverFiresAgainIsLeftAlone(t *testing.T) {
	c := mustCron(t, "0 0 30 2 *")
	last := moment("2026-03-01 00:00")

	at, mark, dropped := Firings(c, CatchUpAll, last, moment("2027-03-01 00:00"))
	if len(at) != 0 || dropped != 0 {
		t.Errorf("a schedule that never fires produced %d firings and dropped %d", len(at), dropped)
	}
	if !mark.Equal(last) {
		t.Errorf("the mark moved to %s for a schedule that never fires", mark)
	}
}
