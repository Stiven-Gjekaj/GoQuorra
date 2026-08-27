package auth

import (
	"strings"
	"testing"
)

const secret = "a-secret-long-enough-to-pass"

func mustKey(t *testing.T, name string, scope Scope, s string) Key {
	t.Helper()
	key, err := NewKey(name, scope, s)
	if err != nil {
		t.Fatalf("NewKey(%q): %v", name, err)
	}
	return key
}

func TestAKeyIsFoundBySecretAndCarriesItsName(t *testing.T) {
	set, err := NewSet(
		mustKey(t, "ops", Write, secret+"-ops"),
		mustKey(t, "dashboard", Read, secret+"-dash"),
	)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}

	got, found := set.Lookup(secret + "-ops")
	if !found {
		t.Fatal("the key was not found")
	}
	if got.Name != "ops" || got.Scope != Write {
		t.Errorf("found %q with scope %s", got.Name, got.Scope)
	}

	got, found = set.Lookup(secret + "-dash")
	if !found || got.Name != "dashboard" || got.Scope != Read {
		t.Errorf("the second key came back as %q/%s, found=%v", got.Name, got.Scope, found)
	}
}

func TestAKeyNobodyHoldsIsNotFound(t *testing.T) {
	set, _ := NewSet(mustKey(t, "ops", Write, secret))

	for _, given := range []string{"", "wrong", secret + "x", strings.ToUpper(secret)} {
		if _, found := set.Lookup(given); found {
			t.Errorf("%q was accepted", given)
		}
	}

	// And a nil set answers rather than panicking, because a server that
	// failed to build one still has a request to refuse.
	var empty *Set
	if _, found := empty.Lookup(secret); found {
		t.Error("a nil set accepted a key")
	}
}

// Write covers read.
//
// A deployment that had to hand out two keys to let one service look and
// change would hand out the larger one twice, which is the opposite of what
// scopes are for.
func TestWriteCoversRead(t *testing.T) {
	if !Write.Allows(Read) {
		t.Error("a write key cannot read")
	}
	if !Write.Allows(Write) || !Read.Allows(Read) {
		t.Error("a scope does not allow itself")
	}
	if Read.Allows(Write) {
		t.Error("a read key can write")
	}
}

// The smaller scope is the zero value.
//
// A key built by a future caller that forgets to set the scope can look and
// cannot touch. The other way round, a forgotten field hands out everything.
func TestTheZeroScopeIsTheSmallerOne(t *testing.T) {
	var scope Scope
	if scope != Read {
		t.Errorf("the zero scope is %s, want read", scope)
	}
}

func TestASetRefusesWhatCannotWork(t *testing.T) {
	if _, err := NewSet(); err == nil {
		t.Error("an empty set was built, and nothing could ever be authorised against it")
	}

	// Two names the same: a log line could not say which one acted.
	if _, err := NewSet(
		mustKey(t, "ops", Write, secret+"-1"),
		mustKey(t, "ops", Read, secret+"-2"),
	); err == nil {
		t.Error("two keys with one name were accepted")
	}

	// Two secrets the same: whichever Lookup reached first would decide the
	// name and the scope, so the answer would depend on the configuration
	// order.
	_, err := NewSet(
		mustKey(t, "ops", Write, secret),
		mustKey(t, "dashboard", Read, secret),
	)
	if err == nil {
		t.Fatal("two keys sharing a secret were accepted")
	}
	if !strings.Contains(err.Error(), "ops") || !strings.Contains(err.Error(), "dashboard") {
		t.Errorf("the error does not name both keys: %v", err)
	}
}

func TestAKeyRefusesWhatCannotWork(t *testing.T) {
	bad := map[string]struct {
		name, secret string
	}{
		"no name":             {"", secret},
		"no secret":           {"ops", ""},
		"secret too short":    {"ops", "short"},
		"a colon in the name": {"ops:write", secret},
		"a comma in the name": {"ops,dashboard", secret},
	}
	for why, c := range bad {
		if _, err := NewKey(c.name, Write, c.secret); err == nil {
			t.Errorf("%s was accepted", why)
		}
	}
}

// The names reach a startup line and the secrets do not.
func TestTheSetNamesItsKeysAndNeverItsSecrets(t *testing.T) {
	set, _ := NewSet(
		mustKey(t, "ops", Write, secret+"-ops"),
		mustKey(t, "dashboard", Read, secret+"-dash"),
	)

	names := strings.Join(set.Names(), " ")
	if !strings.Contains(names, "ops:write") || !strings.Contains(names, "dashboard:read") {
		t.Errorf("the names are %q", names)
	}
	if strings.Contains(names, secret) {
		t.Fatalf("a secret reached the names: %q", names)
	}
}

func TestParseScope(t *testing.T) {
	for text, want := range map[string]Scope{"read": Read, "write": Write, "  WRITE  ": Write} {
		got, err := ParseScope(text)
		if err != nil || got != want {
			t.Errorf("ParseScope(%q) = %s, %v", text, got, err)
		}
	}
	if _, err := ParseScope("admin"); err == nil {
		t.Error("an unknown scope was accepted")
	}
}
