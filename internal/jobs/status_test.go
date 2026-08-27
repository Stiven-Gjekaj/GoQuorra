package jobs

import "testing"

// The list and the check must agree.
//
// This is the test that earns its place. Adding a status means writing a
// constant, teaching Valid about it, and adding it to All. Forgetting the
// third is silent: the new status works everywhere, and it is missing from
// the dashboard and from the queue statistics, which is a gap nobody sees
// until they wonder where their jobs went.
func TestAllHoldsEveryValidStatus(t *testing.T) {
	for _, s := range All() {
		if !s.Valid() {
			t.Errorf("All returns %q, and Valid refuses it", s)
		}
	}

	// The other direction, over every status this package could name. The
	// list is short and closed, so writing it out is honest. Deriving it from
	// All would make the test agree with itself.
	for _, s := range []Status{Blocked, Pending, Leased, Succeeded, Dead, Cancelled} {
		found := false
		for _, listed := range All() {
			if listed == s {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is a status, and All does not list it", s)
		}
	}
}

// The two states that were removed must stay removed.
//
// Both were named in the old documentation and in the old status table, and
// no code ever wrote either. Somebody reading that documentation could
// reasonably add one back. This says no, and says it at the point of the
// decision rather than in a paragraph somewhere.
func TestTheRemovedStatesAreNotStatuses(t *testing.T) {
	for _, text := range []string{"processing", "failed"} {
		if Status(text).Valid() {
			t.Errorf("%q is valid again, and it describes a state the server cannot observe", text)
		}
		if _, err := ParseStatus(text); err == nil {
			t.Errorf("ParseStatus accepted %q", text)
		}
	}
}

func TestTerminal(t *testing.T) {
	cases := map[Status]bool{
		// A job waiting for another one is not finished. A parent that
		// succeeds moves it on, and a parent that never will moves it to
		// cancelled. Calling it terminal would stop both.
		Blocked:   false,
		Pending:   false,
		Leased:    false,
		Succeeded: true,
		Dead:      true,
		Cancelled: true,
	}
	for status, want := range cases {
		if got := status.Terminal(); got != want {
			t.Errorf("%q.Terminal() = %v, want %v", status, got, want)
		}
	}
}

func TestParseStatusRoundTrip(t *testing.T) {
	for _, s := range All() {
		got, err := ParseStatus(s.String())
		if err != nil {
			t.Fatalf("ParseStatus(%q): %v", s, err)
		}
		if got != s {
			t.Errorf("ParseStatus(%q) = %q", s, got)
		}
	}

	if _, err := ParseStatus(""); err == nil {
		t.Error("ParseStatus accepted the empty string")
	}
}

// A cancelled job is not a dead one.
//
// Both are endings that are not a success, and an operator counting failures
// wants one number and not the other. Letting cancel write "dead" would be
// simpler and would make the dead letter queue a mix of jobs the queue gave up
// on and jobs somebody stopped on purpose, which is a number nobody can act
// on.
func TestCancelledIsItsOwnEnding(t *testing.T) {
	if Cancelled == Dead {
		t.Fatal("a cancelled job is recorded as a dead one")
	}
	if !Cancelled.Terminal() {
		t.Error("a cancelled job is not terminal, so something could still pick it up")
	}
	if !Cancelled.Valid() {
		t.Error("cancelled is not a valid status")
	}
}

// A blocked job is not a pending one, and the difference is the point.
//
// Pending is a claim: the queue will hand this job to the next worker that
// asks, once RunAt has passed. A job waiting for a parent is not that.
// Calling it pending would make the queue length, the dashboard and every
// listing count work as ready when it is not.
func TestBlockedIsItsOwnStateAndNotPending(t *testing.T) {
	if Blocked == Pending {
		t.Fatal("blocked and pending are the same value")
	}
	if !Blocked.Valid() {
		t.Error("blocked is not a status")
	}
	if Blocked.Terminal() {
		t.Error("blocked is terminal, so nothing could ever release the job")
	}

	// It survives being written down and read back, which is what the store
	// depends on.
	got, err := ParseStatus("blocked")
	if err != nil || got != Blocked {
		t.Errorf(`ParseStatus("blocked") = %q, %v`, got, err)
	}
}
