package reqid_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/reqid"
)

// What a caller sent is kept, so that the two sides quote the same string.
func TestWhatACallerSentIsKept(t *testing.T) {
	if got := reqid.Of("trace-abc-123"); got != "trace-abc-123" {
		t.Errorf("the identifier is %q, and the caller sent trace-abc-123", got)
	}
}

// A caller that sent nothing gets one made for it.
func TestACallerThatSentNothingGetsOne(t *testing.T) {
	first := reqid.Of("")
	second := reqid.Of("")

	if first == "" || second == "" {
		t.Fatal("a request has no identifier")
	}
	if first == second {
		t.Errorf("two requests share the identifier %q", first)
	}
}

// A value with a newline in it is refused.
//
// This is the one that matters. A log line is a line, so a value with a
// newline in it writes a line of the caller's own choosing into the log of
// the server, saying whatever the caller likes.
func TestAnIdentifierWithANewlineIsRefused(t *testing.T) {
	// No space anywhere in it. A check that refused only spaces would pass
	// this, and the first draft of this test could not tell the two apart.
	sent := "ok\nlevel=ERROR\tmsg=the-database-is-gone"

	if got := reqid.Clean(sent); got != "" {
		t.Errorf("a value with a newline was kept as %q", got)
	}
	if got := reqid.Of(sent); strings.Contains(got, "\n") {
		t.Errorf("the identifier in use is %q, and it holds a newline", got)
	}
}

// A value that is refused is refused, and never trimmed into a usable one.
//
// An identifier the server rewrote no longer matches the one the caller kept,
// and the caller does not know it was changed. A fresh one is at least
// obviously not theirs.
func TestALongValueIsRefusedAndNotTrimmed(t *testing.T) {
	sent := strings.Repeat("a", reqid.Most+1)

	if got := reqid.Clean(sent); got != "" {
		t.Errorf("a value of %d characters was kept as %q", len(sent), got)
	}
	if got := reqid.Of(sent); strings.HasPrefix(sent, got) {
		t.Errorf("the identifier in use is %q, which is the caller's value trimmed", got)
	}
}

// A value of exactly the limit is kept, so that the limit is a limit and not
// one less.
func TestAValueOfExactlyTheLimitIsKept(t *testing.T) {
	sent := strings.Repeat("a", reqid.Most)

	if got := reqid.Clean(sent); got != sent {
		t.Errorf("a value of exactly %d characters was refused", reqid.Most)
	}
}

// A context with no identifier answers with an empty string and does not
// panic.
func TestAContextWithNoIdentifierIsEmpty(t *testing.T) {
	if got := reqid.From(context.Background()); got != "" {
		t.Errorf("a context with nothing on it holds %q", got)
	}

	ctx := reqid.Into(context.Background(), "r1")
	if got := reqid.From(ctx); got != "r1" {
		t.Errorf("the context holds %q", got)
	}
}
