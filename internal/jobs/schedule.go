package jobs

import (
	"fmt"
	"strings"
	"time"
)

// CatchUp says what a schedule does about the firings it missed.
//
// docs/milestones.md called this "the part everybody forgets and then argues
// about", so it is a field somebody has to fill in rather than a behaviour
// they discover.
//
// The case: a server is down from Friday to Monday, and a schedule that
// should have fired sixty times did not fire at all. On Monday, what happens?
type CatchUp string

const (
	// CatchUpSkip fires once, now, and forgets the rest. The default,
	// because it is what almost every schedule means: a report that runs
	// every hour does not want sixty reports.
	CatchUpSkip CatchUp = "skip"

	// CatchUpAll fires once for every window that was missed, oldest first,
	// bounded by MostCaughtUp.
	//
	// For a schedule where each firing does different work, keyed on the
	// window it belongs to. Billing a period, or writing a file named after
	// the hour.
	CatchUpAll CatchUp = "all"

	// CatchUpNone fires nothing until the next scheduled moment.
	//
	// For a schedule where a late firing is worse than a missed one. Sending
	// a reminder about a meeting that has already happened is worse than
	// sending none.
	CatchUpNone CatchUp = "none"
)

// mostWindowsWalked bounds the walk that counts the missed windows.
//
// Next gives up after five years, so a schedule that has not fired for longer
// than that already stops. This is the second bound, and it is on the
// arithmetic rather than on the calendar: a rule that fires every minute is
// half a million windows a year, and counting five of those on one tick of a
// background loop is time the loop owes to the sweep and the reclaim.
//
// A count that hits this is a floor rather than a total, and the schedule is
// so far behind that the difference does not change what anybody does.
const mostWindowsWalked = 100_000

// MostCaughtUp bounds one catch up.
//
// A schedule that has not fired for a year is not sixty missed windows, it is
// nine thousand, and enqueueing nine thousand jobs because somebody restored
// an old backup is not recovery. The rest are dropped, and the caller says so
// in the log rather than leaving a number nobody can explain.
const MostCaughtUp = 100

// ParseCatchUp reads a catch up policy.
func ParseCatchUp(text string) (CatchUp, error) {
	switch CatchUp(strings.ToLower(strings.TrimSpace(text))) {
	case CatchUpSkip:
		return CatchUpSkip, nil
	case CatchUpAll:
		return CatchUpAll, nil
	case CatchUpNone:
		return CatchUpNone, nil
	default:
		return "", fmt.Errorf("jobs: %q is not a catch up policy, and it must be skip, all or none", text)
	}
}

// Valid reports whether this is a catch up policy the package knows.
func (c CatchUp) Valid() bool {
	_, err := ParseCatchUp(string(c))
	return err == nil
}

// CatchUps lists every policy, for a message that has to name them.
func CatchUps() []CatchUp { return []CatchUp{CatchUpSkip, CatchUpAll, CatchUpNone} }

// Firings says which moments a schedule should produce jobs for.
//
// last is the moment the schedule last fired at, and is the zero time for one
// that never has. now is the moment the caller is asking about, which is not
// read from a clock here: the whole of this package takes the time as a
// parameter so that a test states the moment instead of waiting for it.
//
// The answer is oldest first, and is empty when nothing is due.
//
// It also gives back the moment to record as the last firing, which is not
// always the last moment returned: a schedule that skips its missed windows
// still has to record that they are behind it, or it catches the same ones up
// on the next tick for ever.
func Firings(c Cron, policy CatchUp, last, now time.Time) (at []time.Time, mark time.Time, dropped int) {
	if !policy.Valid() {
		policy = CatchUpSkip
	}

	// A schedule that has never fired starts from now. Reading the zero time
	// as a missed window would make the first tick of a new schedule catch up
	// from the year one.
	if last.IsZero() {
		return nil, now, 0
	}

	// Every window between the last firing and now.
	//
	// Counted in full and kept in part. The count is what an operator reads
	// to find out how much did not run, so it has to be the real number: a
	// version that stopped walking at the bound reported one missed window
	// for an outage of a hundred and forty, which is a number nobody could
	// act on.
	//
	// What is kept is a ring of the newest MostCaughtUp. A schedule left for
	// a decade would otherwise hold five million moments in memory before
	// any of them were dropped.
	ring := make([]time.Time, 0, MostCaughtUp)
	total := 0
	from := last

	for {
		next, found := c.Next(from)
		if !found || next.After(now) {
			break
		}
		total++
		from = next

		if len(ring) < MostCaughtUp {
			ring = append(ring, next)
		} else {
			ring = append(ring[1:], next)
		}

		// The walk is bounded, and the bound is the honest one: Next gives
		// up after five years, so a schedule that has not fired for longer
		// than that stops here rather than walking to the epoch.
		if total >= mostWindowsWalked {
			break
		}
	}

	if total == 0 {
		return nil, last, 0
	}

	missed := ring

	// The last window reached is what the schedule is now behind, whichever
	// policy applies. A policy that fired nothing still moves on: leaving the
	// mark where it was would catch the same windows up on the next tick, for
	// ever.
	mark = missed[len(missed)-1]

	switch policy {
	case CatchUpNone:
		return nil, mark, total

	case CatchUpAll:
		// The oldest are dropped and the newest kept. A catch up that ran
		// out of room has to choose, and the recent windows are the ones
		// still worth doing.
		return missed, mark, total - len(missed)

	default: // CatchUpSkip
		// One firing, at the most recent missed window rather than at now.
		// The job then carries the moment it was meant to run at, which is
		// what a handler keyed on a window needs even when it only gets one.
		return missed[len(missed)-1:], mark, total - 1
	}
}
