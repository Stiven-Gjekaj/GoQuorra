package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Text that is not a UUID is told apart by the SQLSTATE and not by the
// sentence beside it.
//
// The message here is not the English one. PostgreSQL translates its messages
// when lc_messages names a locale with a catalogue, and the wording is free
// to change between versions, so a decision that reads the sentence rests on
// a string somebody else owns.
//
// This test fails against the version that read the message, which is the
// point of writing it this way. No locale has to be installed to run it.
func TestABadIdentifierIsToldApartByItsCode(t *testing.T) {
	translated := &pgconn.PgError{
		Code:    invalidText,
		Message: `sintassi di input non valida per il tipo uuid: "not-a-uuid"`,
	}
	if !isInvalidUUID(translated) {
		t.Error("a 22P02 with a translated message does not read as a bad identifier")
	}

	// And wrapped, because every caller in this package wraps before the
	// answer reaches the code that asks.
	if !isInvalidUUID(fmt.Errorf("postgres: cannot read the job: %w", translated)) {
		t.Error("a wrapped 22P02 does not read as a bad identifier")
	}
}

// Another code from the same server is not a bad identifier.
//
// Without this the test above passes against a function that says yes to
// every error from PostgreSQL, and a table that is not there would be
// reported to a caller as a job that does not exist.
func TestAnotherCodeIsNotABadIdentifier(t *testing.T) {
	for name, code := range map[string]string{
		"a relation that is not there": "42P01",
		"a unique violation":           "23505",
		"the server shutting down":     "57P01",
	} {
		other := &pgconn.PgError{Code: code, Message: "something else went wrong"}
		if isInvalidUUID(other) {
			t.Errorf("%s reads as a bad identifier", name)
		}
	}

	// And an error that did not come from PostgreSQL at all.
	if isInvalidUUID(errors.New("dial tcp: connection refused")) {
		t.Error("a failure to connect reads as a bad identifier")
	}
}

// The English sentence alone is not enough.
//
// This is what the old version decided on. An error carrying that text and no
// code is not something PostgreSQL sent, so it must not be read as one.
func TestTheEnglishSentenceAloneIsNotEnough(t *testing.T) {
	if isInvalidUUID(errors.New(`invalid input syntax for type uuid: "not-a-uuid"`)) {
		t.Error("a plain error carrying the English sentence reads as a bad identifier")
	}
}
