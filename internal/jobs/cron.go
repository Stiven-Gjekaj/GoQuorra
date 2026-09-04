package jobs

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a five field schedule: minute, hour, day of month, month, day of
// week.
//
// Written here rather than taken from a library. This project has five direct
// dependencies and a rule about adding a sixth, and the answer to "why does
// the standard library not do this" is that it does not have a cron parser at
// all. What it does have is enough to write one: the whole of this file is
// arithmetic on time.Time, and every rule in it is table tested with nothing
// installed.
//
// Five fields and not six. A seconds field is offered by some libraries, and
// a queue that hands a job to a worker over a network cannot honour a second.
// Offering the field would say it could.
//
// Numbers only. Not JAN, not MON, and not @daily. Every one of those is a
// second spelling of something already expressible, and a second spelling is
// a second thing to get wrong.
type Cron struct {
	minute  field
	hour    field
	day     field
	month   field
	weekday field

	// text is what was parsed, kept so that a schedule can be written back
	// out exactly as somebody wrote it.
	text string
}

// field is the set of values one column matches.
//
// A bitmask and not a list. Asking whether a moment matches is then one AND
// per column, which matters because finding the next firing walks forward a
// minute at a time and asks up to a few hundred thousand times.
type field struct {
	bits uint64

	// star records that the column was written as "*" rather than as every
	// value spelled out. The day of month and the day of week columns behave
	// differently when one of them is a star, and nothing else can tell.
	star bool
}

func (f field) has(value int) bool { return f.bits&(1<<uint(value)) != 0 }

// ParseCron reads a five field schedule.
func ParseCron(text string) (Cron, error) {
	parts := strings.Fields(strings.TrimSpace(text))
	if len(parts) != 5 {
		return Cron{}, fmt.Errorf(
			"%q has %d fields, and a schedule has five: minute hour day-of-month month day-of-week",
			text, len(parts))
	}

	columns := []struct {
		name     string
		from, to int
	}{
		{"minute", 0, 59},
		{"hour", 0, 23},
		{"day of month", 1, 31},
		{"month", 1, 12},
		{"day of week", 0, 6},
	}

	var built [5]field
	for i, column := range columns {
		one, err := parseField(parts[i], column.from, column.to)
		if err != nil {
			return Cron{}, fmt.Errorf("the %s field of %q: %w", column.name, text, err)
		}
		built[i] = one
	}

	return Cron{
		minute:  built[0],
		hour:    built[1],
		day:     built[2],
		month:   built[3],
		weekday: built[4],
		text:    strings.Join(parts, " "),
	}, nil
}

// String gives the schedule back as it was written.
func (c Cron) String() string { return c.text }

// parseField reads one column: a star, a list, a range, or a step.
func parseField(text string, from, to int) (field, error) {
	if text == "" {
		return field{}, fmt.Errorf("the field is empty")
	}

	out := field{star: text == "*" || strings.HasPrefix(text, "*/")}

	for _, piece := range strings.Split(text, ",") {
		low, high, step, err := parsePiece(piece, from, to)
		if err != nil {
			return field{}, err
		}
		for value := low; value <= high; value += step {
			out.bits |= 1 << uint(value)
		}
	}

	if out.bits == 0 {
		return field{}, fmt.Errorf("%q matches nothing", text)
	}
	return out, nil
}

// parsePiece reads one comma separated piece of a column.
func parsePiece(piece string, from, to int) (low, high, step int, err error) {
	step = 1

	// A step, written value/step or */step.
	if slash := strings.Index(piece, "/"); slash >= 0 {
		step, err = strconv.Atoi(piece[slash+1:])
		if err != nil || step < 1 {
			return 0, 0, 0, fmt.Errorf("%q has a step that is not a whole number above zero", piece)
		}
		piece = piece[:slash]

		// A step over a bare value means from that value to the end of the
		// column, which is what every cron does and what nothing says.
		if piece != "*" && !strings.Contains(piece, "-") {
			value, err := strconv.Atoi(piece)
			if err != nil {
				return 0, 0, 0, fmt.Errorf("%q is not a number", piece)
			}
			if value < from || value > to {
				return 0, 0, 0, fmt.Errorf("%d is outside %d to %d", value, from, to)
			}
			return value, to, step, nil
		}
	}

	if piece == "*" {
		return from, to, step, nil
	}

	if dash := strings.Index(piece, "-"); dash > 0 {
		low, err = strconv.Atoi(piece[:dash])
		if err != nil {
			return 0, 0, 0, fmt.Errorf("%q is not a range of numbers", piece)
		}
		high, err = strconv.Atoi(piece[dash+1:])
		if err != nil {
			return 0, 0, 0, fmt.Errorf("%q is not a range of numbers", piece)
		}
		if low > high {
			return 0, 0, 0, fmt.Errorf("the range %q counts down, and a schedule reads one way", piece)
		}
		if low < from || high > to {
			return 0, 0, 0, fmt.Errorf("the range %q is outside %d to %d", piece, from, to)
		}
		return low, high, step, nil
	}

	value, err := strconv.Atoi(piece)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%q is not a number", piece)
	}
	if value < from || value > to {
		return 0, 0, 0, fmt.Errorf("%d is outside %d to %d", value, from, to)
	}
	return value, value, step, nil
}

// Matches reports whether a moment is one this schedule fires at.
//
// The moment is taken in whatever location it carries. A schedule holds its
// own zone, and the caller applies it before asking.
func (c Cron) Matches(at time.Time) bool {
	if !c.minute.has(at.Minute()) || !c.hour.has(at.Hour()) || !c.month.has(int(at.Month())) {
		return false
	}

	// The day rule, which is the one part of cron that surprises everybody.
	//
	// When both day columns name specific days, a moment matches if it is
	// either of them, and not both. "0 0 1 * 1" is the first of the month and
	// every Monday, not the first of the month when it falls on a Monday.
	// When one of them is a star, only the other one decides.
	//
	// This is what every cron does, and it is written down here because it is
	// the rule a reader will not believe without being told.
	day := c.day.has(at.Day())
	weekday := c.weekday.has(int(at.Weekday()))

	switch {
	case c.day.star && c.weekday.star:
		return true
	case c.day.star:
		return weekday
	case c.weekday.star:
		return day
	default:
		return day || weekday
	}
}

// Next gives the first moment after a time that this schedule fires at.
//
// Strictly after, so that asking again with the answer moves forward rather
// than standing still.
//
// It answers false when there is none inside five years. A schedule can name
// a day that does not come: "0 0 30 2 *" is the thirtieth of February, and
// the honest answer to when it next fires is never. Walking for ever looking
// for it is how a background loop stops doing its other work.
func (c Cron) Next(after time.Time) (time.Time, bool) {
	place := after.Location()

	// The walk is over wall clock readings, and not over instants.
	//
	// A cron rule is a rule about what a clock on a wall says. Adding an hour
	// to an instant and then reading the wall off it gives the same answer in
	// a zone that never changes, and a wrong answer in a zone that does.
	//
	// Measured before this was written, "0 2 * * *" in Europe/Berlin: the
	// walk answered 25 October 2026 02:00 twice, an hour apart, because that
	// reading happens twice on the day the clock goes back. It also stepped
	// from 28 March to 30 March, because 02:00 does not happen at all on the
	// day the clock goes forward.
	//
	// Reading first and converting once at the end makes both of those fall
	// out of the representation rather than out of a special case. Every
	// reading is visited once, so a schedule fires once on the day the clock
	// goes back. A reading that does not exist converts to the first instant
	// after the gap, so a schedule fires once on the other day as well.
	//
	// A UTC time carries the reading. It is not a moment in UTC. It is five
	// numbers, and time.Time is the type in the standard library that holds
	// those five numbers and knows how many days April has.
	local := after.In(place)
	civil := time.Date(
		local.Year(), local.Month(), local.Day(),
		local.Hour(), local.Minute(), 0, 0, time.UTC,
	).Add(time.Minute)
	limit := civil.AddDate(5, 0, 0)

	for civil.Before(limit) {
		// A whole month that cannot match is skipped as a month rather than
		// as forty thousand minutes.
		//
		// Measured again after the walk moved to wall clock readings,
		// because this is the only line here that is an optimisation rather
		// than a rule, and one of those has to earn its place.
		//
		// "0 0 29 2 *" asked in March: 7.5us with this line and 15.4ms
		// without it. "0 0 30 2 *", which never fires and walks the whole
		// five year limit: 24.5us against 37.4ms.
		//
		// "Without it" keeps the month test and steps a minute at a time.
		// Deleting the block measures something else: the month column stops
		// being tested, and the walk then answers with the twenty ninth of
		// March. That mistake was made once while taking these numbers.
		//
		// Thirty seven milliseconds inside the loop that also sweeps and
		// reclaims, on every tick, for a schedule that never fires, is the
		// thing this stops. cron_bench_test.go holds the measurement.
		if !c.month.has(int(civil.Month())) {
			civil = time.Date(civil.Year(), civil.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
			continue
		}
		if !c.matchesDay(civil) {
			civil = time.Date(civil.Year(), civil.Month(), civil.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
			continue
		}
		if !c.hour.has(civil.Hour()) {
			civil = time.Date(civil.Year(), civil.Month(), civil.Day(), civil.Hour(), 0, 0, 0, time.UTC).Add(time.Hour)
			continue
		}
		if !c.minute.has(civil.Minute()) {
			civil = civil.Add(time.Minute)
			continue
		}

		// Strictly after, in real time and not only on the clock. The two
		// come apart on the day the clock goes back, where a later reading
		// can name an earlier instant.
		if fires := instantOf(civil, place); fires.After(after) {
			return fires, true
		}
		civil = civil.Add(time.Minute)
	}
	return time.Time{}, false
}

// instantOf gives the moment that a wall clock reading names in a place.
//
// A reading that happens twice names the first of the two. time.Date is
// allowed to answer with either and answers with the second, so the first is
// found here. The reason to prefer it is one an operator can check: a daily
// schedule then stays twenty four hours from the day before, where the second
// reading puts twenty five hours between them.
//
// A reading that does not happen at all names the first moment after the gap,
// which is what time.Date already answers. A daily schedule that skipped a
// day once a year would be found in the ledger and not in the log.
func instantOf(civil time.Time, place *time.Location) time.Time {
	at := time.Date(
		civil.Year(), civil.Month(), civil.Day(),
		civil.Hour(), civil.Minute(), 0, 0, place,
	)

	// Three hours covers every change any zone has made, and is short enough
	// that it cannot reach a second one.
	_, offset := at.Zone()
	_, before := at.Add(-3 * time.Hour).Zone()
	if before == offset {
		return at
	}

	moved := at.Add(time.Duration(offset-before) * time.Second)
	if moved.Before(at) && reads(moved, civil) {
		return moved
	}
	return at
}

// reads reports whether a moment shows the same wall clock as a reading.
func reads(at time.Time, civil time.Time) bool {
	return at.Year() == civil.Year() &&
		at.Month() == civil.Month() &&
		at.Day() == civil.Day() &&
		at.Hour() == civil.Hour() &&
		at.Minute() == civil.Minute()
}

// matchesDay is the day half of Matches, which Next needs on its own.
func (c Cron) matchesDay(at time.Time) bool {
	day := c.day.has(at.Day())
	weekday := c.weekday.has(int(at.Weekday()))

	switch {
	case c.day.star && c.weekday.star:
		return true
	case c.day.star:
		return weekday
	case c.weekday.star:
		return day
	default:
		return day || weekday
	}
}
