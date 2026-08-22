// Package migrations carries the SQL that builds the database.
//
// The files live at the top of the repository so that an operator can find
// them, and they are embedded so that the test suite, the container stack and
// the server apply the same bytes the operator reads. A schema kept only on
// disk drifts from the one the tests run against, and the drift shows up as a
// passing suite over a database nobody has.
//
// Every file is applied, in the order its name sorts. Each one is written to
// be safe to apply twice, which is what lets this be a list rather than a
// table of what has already run. That choice has a limit worth knowing: it
// works while every change is additive, and the first one that is not needs a
// real migration tool. docs/milestones.md says so.
package migrations

import (
	"embed"
	"io/fs"
	"sort"
	"strings"
)

//go:embed *.sql
var files embed.FS

// Names lists the migration files in the order they are applied.
func Names() []string {
	entries, err := fs.Glob(files, "*.sql")
	if err != nil {
		// Glob fails only on a bad pattern, and the pattern is a constant.
		panic("migrations: " + err.Error())
	}
	sort.Strings(entries)
	return entries
}

// Read returns one migration by name.
func Read(name string) (string, error) {
	raw, err := files.ReadFile(name)
	return string(raw), err
}

// Schema is every migration, joined in order.
//
// One string, because the callers that apply it are a test and a command line
// tool, and both hand the whole thing to PostgreSQL in one go. A file that
// fails takes the ones after it with it, which is the behaviour to want: a
// half built schema is worse than none.
func Schema() string {
	var whole strings.Builder
	for _, name := range Names() {
		body, err := Read(name)
		if err != nil {
			panic("migrations: " + err.Error())
		}
		whole.WriteString("-- ")
		whole.WriteString(name)
		whole.WriteString("\n")
		whole.WriteString(body)
		whole.WriteString("\n")
	}
	return whole.String()
}
