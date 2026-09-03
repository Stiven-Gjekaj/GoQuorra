package jobs

import (
	"testing"
	"time"
)

func mustCron(t *testing.T, text string) Cron {
	t.Helper()
	c, err := ParseCron(text)
	if err != nil {
		t.Fatalf("ParseCron(%q): %v", text, err)
	}
	return c
}

func moment(text string) time.Time {
	at, err := time.Parse("2006-01-02 15:04", text)
	if err != nil {
		panic(err)
	}
	return at
}

// The shapes a column can take.
func TestACronColumnTakesEveryShapeItShould(t *testing.T) {
	cases := []struct {
		schedule string
		at       string
		want     bool
	}{
		// Every minute.
		{"* * * * *", "2026-03-01 00:00", true},
		{"* * * * *", "2026-03-01 13:47", true},

		// A single value.
		{"0 9 * * *", "2026-03-01 09:00", true},
		{"0 9 * * *", "2026-03-01 09:01", false},
		{"0 9 * * *", "2026-03-01 10:00", false},

		// A list.
		{"0 9,17 * * *", "2026-03-01 09:00", true},
		{"0 9,17 * * *", "2026-03-01 17:00", true},
		{"0 9,17 * * *", "2026-03-01 13:00", false},

		// A range.
		{"0 9-11 * * *", "2026-03-01 09:00", true},
		{"0 9-11 * * *", "2026-03-01 11:00", true},
		{"0 9-11 * * *", "2026-03-01 12:00", false},

		// A step over a star.
		{"*/15 * * * *", "2026-03-01 00:00", true},
		{"*/15 * * * *", "2026-03-01 00:15", true},
		{"*/15 * * * *", "2026-03-01 00:20", false},

		// A step over a range.
		{"0 8-18/2 * * *", "2026-03-01 08:00", true},
		{"0 8-18/2 * * *", "2026-03-01 10:00", true},
		{"0 8-18/2 * * *", "2026-03-01 09:00", false},

		// A step over a bare value, which every cron reads as "from here to
		// the end of the column" and nothing says.
		{"0 9/6 * * *", "2026-03-01 09:00", true},
		{"0 9/6 * * *", "2026-03-01 15:00", true},
		{"0 9/6 * * *", "2026-03-01 21:00", true},
		{"0 9/6 * * *", "2026-03-01 03:00", false},

		// A month.
		{"0 0 1 1 *", "2026-01-01 00:00", true},
		{"0 0 1 1 *", "2026-02-01 00:00", false},
	}

	for _, c := range cases {
		if got := mustCron(t, c.schedule).Matches(moment(c.at)); got != c.want {
			t.Errorf("%q.Matches(%s) = %v, want %v", c.schedule, c.at, got, c.want)
		}
	}
}

// The day rule, which is the one part of cron that surprises everybody.
//
// When both day columns name specific days, a moment matches if it is either
// of them and not both. A reader who has not met this will not believe it, so
// it gets its own table.
func TestTheTwoDayColumnsAreAnOrAndNotAnAnd(t *testing.T) {
	// 2026-03-01 is a Sunday, so 2026-03-02 is a Monday.
	if moment("2026-03-02 00:00").Weekday() != time.Monday {
		t.Fatal("the dates this table is built on are wrong")
	}

	cases := []struct {
		name     string
		schedule string
		at       string
		want     bool
	}{
		// Both stars: every day.
		{"both stars, any day", "0 0 * * *", "2026-03-04 00:00", true},

		// Day of month only.
		{"the first, on the first", "0 0 1 * *", "2026-03-01 00:00", true},
		{"the first, on the second", "0 0 1 * *", "2026-03-02 00:00", false},

		// Day of week only.
		{"Mondays, on a Monday", "0 0 * * 1", "2026-03-02 00:00", true},
		{"Mondays, on a Tuesday", "0 0 * * 1", "2026-03-03 00:00", false},

		// Both named: either one fires it.
		{"the first or a Monday, on the first", "0 0 1 * 1", "2026-03-01 00:00", true},
		{"the first or a Monday, on a Monday", "0 0 1 * 1", "2026-03-02 00:00", true},
		{"the first or a Monday, on a Tuesday", "0 0 1 * 1", "2026-03-03 00:00", false},

		// And the same schedule read as an AND would say no to both of the
		// first two, which is the fault this table exists to catch.
	}

	for _, c := range cases {
		if got := mustCron(t, c.schedule).Matches(moment(c.at)); got != c.want {
			t.Errorf("%s: %q.Matches(%s) = %v, want %v", c.name, c.schedule, c.at, got, c.want)
		}
	}
}

// Next gives the first firing strictly after a moment.
func TestNextFindsTheFiringAfterAMoment(t *testing.T) {
	cases := []struct {
		schedule string
		after    string
		want     string
	}{
		{"* * * * *", "2026-03-01 00:00", "2026-03-01 00:01"},
		{"0 9 * * *", "2026-03-01 00:00", "2026-03-01 09:00"},

		// Strictly after, so asking again with the answer moves on rather
		// than standing still.
		{"0 9 * * *", "2026-03-01 09:00", "2026-03-02 09:00"},

		{"*/15 * * * *", "2026-03-01 00:07", "2026-03-01 00:15"},
		{"0 0 1 * *", "2026-03-15 12:00", "2026-04-01 00:00"},

		// A whole year away, which is the case that walks a year one minute
		// at a time without the month skip.
		{"0 0 1 1 *", "2026-01-01 00:00", "2027-01-01 00:00"},

		// A weekday rule across a month boundary.
		{"0 0 * * 1", "2026-03-26 00:00", "2026-03-30 00:00"},
	}

	for _, c := range cases {
		got, found := mustCron(t, c.schedule).Next(moment(c.after))
		if !found {
			t.Errorf("%q.Next(%s) found nothing", c.schedule, c.after)
			continue
		}
		if !got.Equal(moment(c.want)) {
			t.Errorf("%q.Next(%s) = %s, want %s", c.schedule, c.after, got.Format("2006-01-02 15:04"), c.want)
		}
	}
}

// The seconds of the moment asked about are not part of the question.
func TestNextIgnoresWhatIsSmallerThanAMinute(t *testing.T) {
	c := mustCron(t, "* * * * *")
	at := time.Date(2026, 3, 1, 12, 0, 30, 500, time.UTC)

	got, found := c.Next(at)
	if !found {
		t.Fatal("Next found nothing")
	}
	want := time.Date(2026, 3, 1, 12, 1, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("Next = %s, want %s", got, want)
	}
}

// A schedule that names a day that does not come answers no rather than
// walking for ever.
//
// The thirtieth of February. A background loop that walked looking for it
// would stop doing its other work.
func TestNextGivesUpOnADayThatNeverComes(t *testing.T) {
	c := mustCron(t, "0 0 30 2 *")
	if got, found := c.Next(moment("2026-01-01 00:00")); found {
		t.Errorf("the thirtieth of February was found at %s", got)
	}

	// The twenty ninth does come, in a leap year, so the answer is not simply
	// always no.
	leap := mustCron(t, "0 0 29 2 *")
	got, found := leap.Next(moment("2026-01-01 00:00"))
	if !found {
		t.Fatal("the twenty ninth of February was not found")
	}
	if !got.Equal(moment("2028-02-29 00:00")) {
		t.Errorf("the next twenty ninth of February is %s, want 2028-02-29", got.Format("2006-01-02 15:04"))
	}
}

// A schedule the parser cannot read is refused, and the message names the
// field.
func TestASchedulePartTheParserCannotReadIsRefused(t *testing.T) {
	bad := map[string]string{
		"too few fields":              "0 9 * *",
		"too many fields":             "0 0 9 * * *",
		"a minute past the end":       "60 * * * *",
		"an hour past the end":        "* 24 * * *",
		"a day of zero":               "0 0 0 * *",
		"a month of thirteen":         "0 0 * 13 *",
		"a weekday of seven":          "0 0 * * 7",
		"a range that counts down":    "0 17-9 * * *",
		"a step of zero":              "*/0 * * * *",
		"a step that is not a number": "*/x * * * *",
		"a word":                      "0 0 * * MON",
		"a shorthand":                 "@daily",
		"an empty field":              "0  * * *",
	}

	for name, text := range bad {
		if _, err := ParseCron(text); err == nil {
			t.Errorf("%s: ParseCron(%q) was accepted", name, text)
		}
	}
}

// A schedule survives being written down and read back.
func TestAScheduleSurvivesBeingWrittenDownAndReadBack(t *testing.T) {
	for _, text := range []string{"0 9 * * *", "*/15 * * * *", "0 0 1 * 1", "0 8-18/2 * * 1-5"} {
		first := mustCron(t, text)
		second := mustCron(t, first.String())

		if first.String() != second.String() {
			t.Errorf("%q came back as %q", text, second.String())
		}
		// And the two agree about every minute of a day, which is what
		// matters rather than the text.
		at := moment("2026-03-02 00:00")
		for i := 0; i < 24*60; i++ {
			if first.Matches(at) != second.Matches(at) {
				t.Fatalf("%q and its round trip disagree at %s", text, at)
			}
			at = at.Add(time.Minute)
		}
	}
}

// A schedule fires once on the day the clock goes back.
//
// The walk used to add an hour to an instant and read the wall clock off it.
// A reading that happens twice was then two firings, an hour apart, and the
// idempotency key does not stop them because they are different windows. A
// daily invoice run billed twice.
//
// The instants are pinned in UTC and not only on the clock, because both
// answers read 02:00 and only one of them is right.
func TestAScheduleFiresOnceOnTheDayTheClockGoesBack(t *testing.T) {
	berlin := place(t, "Europe/Berlin")
	rule := mustCron(t, "0 2 * * *")

	// 25 October 2026: 03:00 CEST becomes 02:00 CET, so 02:00 happens twice.
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, berlin)

	want := []string{
		"2026-10-25 00:00", // 02:00 CEST, the first of the two readings
		"2026-10-26 01:00", // 02:00 CET, the day after
		"2026-10-27 01:00",
	}
	for _, expected := range want {
		next, found := rule.Next(at)
		if !found {
			t.Fatalf("no firing after %s", at)
		}
		if got := next.UTC().Format("2006-01-02 15:04"); got != expected {
			t.Errorf("fired at %s UTC, want %s", got, expected)
		}
		if next.Hour() != 2 {
			t.Errorf("the clock reads %02d:00 and the schedule says 02:00", next.Hour())
		}
		at = next
	}
}

// A schedule fires on the day the clock goes forward, at the first moment
// that exists.
//
// 02:00 does not happen on that day. The walk used to step over the whole
// day, so a daily schedule did not run, and nothing said so. A day missing
// once a year is found in the ledger and not in the log.
func TestAScheduleFiresOnTheDayTheClockGoesForward(t *testing.T) {
	berlin := place(t, "Europe/Berlin")
	rule := mustCron(t, "0 2 * * *")

	// 29 March 2026: 02:00 CET becomes 03:00 CEST.
	next, found := rule.Next(time.Date(2026, 3, 28, 12, 0, 0, 0, berlin))
	if !found {
		t.Fatal("no firing")
	}

	if got := next.Format("2006-01-02 15:04"); got != "2026-03-29 03:00" {
		t.Errorf("fired at %s, want the first moment after the gap", got)
	}
	if got := next.UTC().Format("2006-01-02 15:04"); got != "2026-03-29 01:00" {
		t.Errorf("fired at %s UTC, want 2026-03-29 01:00", got)
	}
}

// The same two days in a zone that changes its clock in the other order.
//
// A test written only against Europe would pass against code that assumed the
// clock goes forward in March and back in October. Auckland does the reverse.
func TestTheClockChangesBothWaysBelowTheEquator(t *testing.T) {
	auckland := place(t, "Pacific/Auckland")
	rule := mustCron(t, "30 2 * * *")

	// 5 April 2026: 03:00 NZDT becomes 02:00 NZST, so 02:30 happens twice.
	//
	// Walked twice and not once. One call lands on the first reading whether
	// or not the second is suppressed, so a single call cannot tell a fixed
	// walk from a broken one.
	at := time.Date(2026, 4, 4, 12, 0, 0, 0, auckland)
	for _, expected := range []string{"2026-04-04 13:30", "2026-04-05 14:30"} {
		back, found := rule.Next(at)
		if !found {
			t.Fatal("no firing in April")
		}
		if got := back.UTC().Format("2006-01-02 15:04"); got != expected {
			t.Errorf("fired at %s UTC, want %s", got, expected)
		}
		at = back
	}

	// 27 September 2026: 02:00 NZST becomes 03:00 NZDT, so 02:30 is missing.
	forward, found := rule.Next(time.Date(2026, 9, 26, 12, 0, 0, 0, auckland))
	if !found {
		t.Fatal("no firing in September")
	}
	if got := forward.Format("2006-01-02 15:04"); got != "2026-09-27 03:30" {
		t.Errorf("fired at %s, want the first moment after the gap", got)
	}
}

// place loads a zone, and fails the test rather than skipping when the
// machine has no zone database.
//
// A skip here would be a test that reports success having checked nothing,
// which is the failure this project was rebuilt to stop.
func place(t *testing.T, name string) *time.Location {
	t.Helper()

	loaded, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("cannot load %s, so the rules about the clock changing cannot be checked: %v", name, err)
	}
	return loaded
}
