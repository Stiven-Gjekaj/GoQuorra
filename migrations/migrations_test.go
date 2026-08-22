package migrations

import (
	"strings"
	"testing"
)

func TestEveryMigrationIsFound(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("no migration was embedded, so the schema is empty")
	}
	if names[0] != "0001_init.sql" {
		t.Errorf("the first migration is %q, want 0001_init.sql", names[0])
	}
}

// The names have to sort into the order the files must run in.
//
// A file called 10_x.sql sorts before 9_x.sql, so a project that numbers
// without padding applies its tenth change before its ninth. Every name here
// is checked for the four digit prefix that makes the sort correct.
func TestTheNamesSortIntoTheRightOrder(t *testing.T) {
	for _, name := range Names() {
		if len(name) < 5 {
			t.Errorf("%q is too short to carry a number", name)
			continue
		}
		for i := 0; i < 4; i++ {
			if name[i] < '0' || name[i] > '9' {
				t.Errorf("%q does not start with four digits, so it will sort into the wrong place", name)
				break
			}
		}
		if name[4] != '_' {
			t.Errorf("%q does not separate its number from its name", name)
		}
	}
}

func TestSchemaHoldsEveryFile(t *testing.T) {
	whole := Schema()
	for _, name := range Names() {
		body, err := Read(name)
		if err != nil {
			t.Fatalf("Read(%q): %v", name, err)
		}
		if !strings.Contains(whole, body) {
			t.Errorf("the schema does not hold %q", name)
		}
		// Each one is named in a comment, so a failure from PostgreSQL can be
		// traced back to a file.
		if !strings.Contains(whole, "-- "+name) {
			t.Errorf("the schema does not name %q", name)
		}
	}
}

// Applying the schema twice must work.
//
// It is a list of files and not a table of what has run, so every one of them
// has to be written to be safe to apply again. A CREATE TABLE without IF NOT
// EXISTS passes the first run and fails every one after it, which turns a
// container restart into an outage.
func TestEveryMigrationIsSafeToApplyTwice(t *testing.T) {
	for _, name := range Names() {
		body, err := Read(name)
		if err != nil {
			t.Fatalf("Read(%q): %v", name, err)
		}

		for _, statement := range []string{"CREATE TABLE", "CREATE INDEX"} {
			for _, line := range strings.Split(body, "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, statement) && !strings.Contains(trimmed, "IF NOT EXISTS") {
					t.Errorf("%s: %q cannot be applied twice", name, trimmed)
				}
			}
		}
	}
}
