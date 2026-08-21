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
	for _, s := range []Status{Pending, Leased, Succeeded, Dead} {
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
		Pending:   false,
		Leased:    false,
		Succeeded: true,
		Dead:      true,
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
