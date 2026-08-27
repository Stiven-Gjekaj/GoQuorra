// Package reqid gives every request an identifier and carries it in a context.
//
// The job identifier already joins the line that accepted a job to the line
// that reported on it: both carry job=<id>. What it cannot do is join a
// caller to the server. A caller that says "it failed at 14:02" has nothing
// to quote, and a server writing two hundred lines a second at 14:02 has
// nothing to look for.
//
// So every request gets an identifier. It is taken from what the caller sent
// when the caller sent one, made here when it did not, written on the lines
// that request produces, and given back in the answer.
package reqid

import (
	"context"

	"github.com/google/uuid"
)

// Header is where the identifier travels.
//
// The same name over HTTP and over gRPC. gRPC lowercases metadata keys, so a
// caller that sets "X-Request-Id" and one that sets "x-request-id" are the
// same caller and neither has to know which. Go canonicalises HTTP header
// names for the same reason.
const Header = "X-Request-Id"

// Most is the longest identifier a caller may send.
//
// A caller supplied value is written into the log, and a log is read by a
// person and by whatever collects it. A value of a megabyte writes a megabyte
// on every line about that request.
const Most = 64

// New makes an identifier.
func New() string { return uuid.NewString() }

type key struct{}

// Into puts an identifier on a context.
func Into(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, key{}, id)
}

// From reads the identifier off a context, and gives an empty string when
// there is none.
func From(ctx context.Context) string {
	id, _ := ctx.Value(key{}).(string)
	return id
}

// Clean gives the identifier to use for what a caller sent.
//
// An empty answer means the caller sent nothing usable and the server should
// make its own. What a caller sent is refused rather than trimmed or
// rewritten: an identifier the server changed is one that no longer matches
// the identifier the caller kept, which is worse than a fresh one, because
// the caller does not know it was changed.
//
// Refused: anything longer than Most, and anything holding a character
// outside printable ASCII. A newline is the one that matters. A log line is a
// line, so a value with a newline in it writes a line of the caller's own
// choosing into the log of the server, saying whatever the caller likes.
func Clean(sent string) string {
	if sent == "" || len(sent) > Most {
		return ""
	}
	for i := 0; i < len(sent); i++ {
		if sent[i] < '!' || sent[i] > '~' {
			return ""
		}
	}
	return sent
}

// Of gives the identifier to use for what a caller sent, making one when
// there is nothing usable.
func Of(sent string) string {
	if clean := Clean(sent); clean != "" {
		return clean
	}
	return New()
}
