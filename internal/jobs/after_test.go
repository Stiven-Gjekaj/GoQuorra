package jobs

import "testing"

// The whole rule, as a table.
//
// The order of the parents must not change the answer, so every case that
// mixes states appears both ways round.
func TestAfterStateDecidesFromTheParents(t *testing.T) {
	cases := []struct {
		name    string
		parents []Status
		want    Status
	}{
		{"a job that waits for nothing is ready", nil, Pending},
		{"an empty list is the same as none", []Status{}, Pending},

		{"one parent that succeeded", []Status{Succeeded}, Pending},
		{"every parent succeeded", []Status{Succeeded, Succeeded, Succeeded}, Pending},

		{"a parent still waiting", []Status{Pending}, Blocked},
		{"a parent a worker is holding", []Status{Leased}, Blocked},
		{"a parent that is itself waiting", []Status{Blocked}, Blocked},
		{"one done and one not", []Status{Succeeded, Pending}, Blocked},
		{"one not and one done", []Status{Pending, Succeeded}, Blocked},

		{"a parent that died", []Status{Dead}, Cancelled},
		{"a parent somebody cancelled", []Status{Cancelled}, Cancelled},
		{"one done and one dead", []Status{Succeeded, Dead}, Cancelled},
		{"one dead and one done", []Status{Dead, Succeeded}, Cancelled},

		// A parent that cannot succeed decides, whichever way round it comes,
		// and whatever else is still running. A job that will never run
		// should not sit blocked until somebody notices.
		{"one dead and one running", []Status{Dead, Leased}, Cancelled},
		{"one running and one dead", []Status{Leased, Dead}, Cancelled},
	}

	for _, c := range cases {
		if got := AfterState(c.parents); got != c.want {
			t.Errorf("%s: AfterState(%v) = %q, want %q", c.name, c.parents, got, c.want)
		}
	}
}

// The answer is only ever one of three.
//
// Nothing here may return leased or succeeded: this decides what a job that
// has not run should be, and both of those claim it has.
func TestAfterStateNeverClaimsAJobHasRun(t *testing.T) {
	for _, parent := range All() {
		got := AfterState([]Status{parent})
		switch got {
		case Pending, Blocked, Cancelled:
		default:
			t.Errorf("a parent that is %q gave %q, which says the job has run", parent, got)
		}
	}
}
