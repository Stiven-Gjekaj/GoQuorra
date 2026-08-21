// Package migrations carries the SQL that builds the database.
//
// The files live at the top of the repository so that an operator can find
// them, and they are embedded so that the test suite and the server apply the
// same bytes the operator reads. A schema kept only on disk drifts from the
// one the tests run against, and the drift shows up as a passing suite over a
// database nobody has.
package migrations

import (
	_ "embed"
)

// Schema builds the jobs table, its constraints, and its indexes. It is safe
// to apply more than once.
//
//go:embed 0001_init.sql
var Schema string
