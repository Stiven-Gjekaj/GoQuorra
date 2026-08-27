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
			"jobs: %q has %d fields, and a schedule has five: minute hour day-of-month month day-of-week",
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
			return Cron{}, fmt.Errorf("jobs: the %s field of %q: %w", column.name, text, err)
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
	// From the start of the next minute. A schedule fires on a minute, and
	// the seconds and smaller parts of the moment asked about are not part of
	// the question.
	at := after.Truncate(time.Minute).Add(time.Minute)
	limit := after.AddDate(5, 0, 0)

	for at.Before(limit) {
		// A whole month that cannot match is skipped as a month rather than
		// as forty thousand minutes.
		//
		// Measured, because this is the only line here that is an
		// optimisation rather than a rule, and one of those has to earn its
		// place. "0 0 29 2 *" asked in March: 7.9us with this line and 19.6ms
		// without it. "0 0 30 2 *", which never fires and walks the whole
		// five year limit: 41us against 49ms.
		//
		// Fifty milliseconds inside the loop that also sweeps and reclaims,
		// on every tick, for a schedule that never fires, is the thing this
		// stops. cron_bench_test.go holds the measurement.
		if !c.month.has(int(at.Month())) {
			at = time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, at.Location()).AddDate(0, 1, 0)
			continue
		}
		if !c.matchesDay(at) {
			at = time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location()).AddDate(0, 0, 1)
			continue
		}
		if !c.hour.has(at.Hour()) {
			at = time.Date(at.Year(), at.Month(), at.Day(), at.Hour(), 0, 0, 0, at.Location()).Add(time.Hour)
			continue
		}
		if !c.minute.has(at.Minute()) {
			at = at.Add(time.Minute)
			continue
		}
		return at, true
	}
	return time.Time{}, false
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
