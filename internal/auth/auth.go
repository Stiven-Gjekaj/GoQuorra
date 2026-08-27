// Package auth tells one caller from another.
//
// The server used to hold a single API key in a string. Everything that
// reaches for a caller runs into that: there is no per queue permission, no
// per caller limit and no way to record who cancelled a job, because there is
// nobody to record. All four need a name first, and this package is that
// name.
//
// It is not a user model. A key stands for a service, not a person, and
// nothing here has a password, a session or a role. A deployment that needs
// people to sign in needs something in front of this, and it should not be
// this package grown sideways.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"sort"
	"strings"
)

// Scope is what a key may do.
//
// A set of bits and not a number on a line. The first version was ordered,
// read below write, and the comparison was a greater than. That works while
// every permission is more of the same thing, and it stops working the moment
// one of them is a different thing: leasing work off the queue is not more
// than changing a job, it is a different door, and a key for an operator's
// shell must not open it.
type Scope uint8

const (
	// Read allows every route that only reads.
	Read Scope = 1 << iota

	// Change allows everything that changes a job over HTTP: submitting,
	// cancelling and reviving.
	Change

	// Work allows leasing a job and reporting on it, which is the worker
	// protocol and nothing else.
	//
	// Separate from Change on purpose. A key that an operator keeps in a
	// shell profile to cancel a job must not be able to lease the queue
	// empty, and a worker must not be able to cancel anything.
	Work
)

// Write is what a key marked "write" holds: reading and changing.
//
// A caller who may change a job may also look at one, and a deployment that
// had to hand out two keys for that would end up handing out the larger one
// twice. It does not include Work, which is the whole point of the split.
const Write = Read | Change

// Worker is what a key marked "worker" holds.
//
// Work alone. A worker is given jobs and reports on them, and it has no
// reason to read the listing or to cancel anything.
const Worker = Work

// Everything is every permission there is.
//
// The short form of the configuration grants it, because a deployment that
// sets one key is saying it does not want to divide anything yet.
const Everything = Read | Change | Work

// ParseScope reads a scope from configuration.
//
// A name, or several joined by a plus. "write+worker" is the one that comes
// up: a small deployment that wants one key for its tools and its workers
// without giving that key to a third thing.
func ParseScope(text string) (Scope, error) {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return 0, fmt.Errorf("auth: a key needs a scope, and it must be read, write, worker or all")
	}

	var scope Scope
	for _, part := range strings.Split(text, "+") {
		switch strings.TrimSpace(part) {
		case "read":
			scope |= Read
		case "write":
			scope |= Write
		case "worker":
			scope |= Worker
		case "all":
			scope |= Everything
		default:
			return 0, fmt.Errorf(
				"auth: %q is not a scope, and it must be read, write, worker or all, or several joined by a plus", text)
		}
	}
	return scope, nil
}

// String names the scope the way configuration writes it.
func (s Scope) String() string {
	if s == Everything {
		return "all"
	}

	var parts []string
	if s&Change != 0 {
		parts = append(parts, "write")
	} else if s&Read != 0 {
		parts = append(parts, "read")
	}
	if s&Work != 0 {
		parts = append(parts, "worker")
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, "+")
}

// Allows says whether this key holds every permission asked for.
//
// Every one and not any one. A route that needed two would otherwise be open
// to a key that held either.
func (s Scope) Allows(wanted Scope) bool { return wanted != 0 && s&wanted == wanted }

// Key is one caller.
type Key struct {
	// Name reaches the logs and the job row. It is chosen by whoever writes
	// the configuration, so it is theirs to make meaningful.
	Name string

	Scope Scope

	// digest is the hashed secret. The secret itself is not kept, so a heap
	// dump of a running server does not hand it over.
	digest [sha256.Size]byte
}

// NewKey builds a key from a name, a scope and a secret.
func NewKey(name string, scope Scope, secret string) (Key, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Key{}, fmt.Errorf("auth: a key needs a name")
	}
	if strings.ContainsAny(name, ":,") {
		return Key{}, fmt.Errorf("auth: the key name %q holds a colon or a comma, which separate the fields", name)
	}
	if scope == 0 {
		return Key{}, fmt.Errorf("auth: the key %q has no scope, so it could do nothing", name)
	}
	if secret == "" {
		return Key{}, fmt.Errorf("auth: the key %q has no secret", name)
	}
	// Short enough to guess is short enough to refuse. This is the one place
	// that can say so, and saying it at startup is better than saying it in
	// an incident report.
	if len(secret) < 16 {
		return Key{}, fmt.Errorf("auth: the secret for %q is %d characters, and the shortest allowed is 16", name, len(secret))
	}
	return Key{Name: name, Scope: scope, digest: sha256.Sum256([]byte(secret))}, nil
}

// Set is every key the server accepts.
type Set struct {
	keys []Key
}

// NewSet builds a set and refuses one that cannot work.
func NewSet(keys ...Key) (*Set, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("auth: no keys, so nothing could ever be authorised")
	}

	seenName := map[string]bool{}
	for _, key := range keys {
		if seenName[key.Name] {
			return nil, fmt.Errorf("auth: two keys are named %q, so a log line could not say which acted", key.Name)
		}
		seenName[key.Name] = true
	}

	// Two keys sharing a secret is refused rather than tolerated. Whichever
	// one Lookup happened to reach first would decide the name and the scope,
	// so the answer would depend on the order of the configuration.
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[i].digest == keys[j].digest {
				return nil, fmt.Errorf("auth: the keys %q and %q share a secret, so the caller could not be told apart",
					keys[i].Name, keys[j].Name)
			}
		}
	}

	return &Set{keys: keys}, nil
}

// Lookup finds the key a caller presented.
//
// Every key is compared, and the comparison does not stop at the first match.
// Returning early would make the time taken depend on the position of the key
// in the set, which tells somebody guessing whether they are getting closer.
func (s *Set) Lookup(presented string) (Key, bool) {
	if s == nil || presented == "" {
		return Key{}, false
	}

	given := sha256.Sum256([]byte(presented))

	found := Key{}
	matched := 0
	for _, key := range s.keys {
		if subtle.ConstantTimeCompare(given[:], key.digest[:]) == 1 {
			found = key
			matched++
		}
	}
	return found, matched == 1
}

// Names lists the key names, sorted, for a startup line.
//
// The names and never the secrets. A server that logged what it accepted
// would put every key in the log of every deployment that read its own
// startup.
func (s *Set) Names() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.keys))
	for _, key := range s.keys {
		out = append(out, key.Name+":"+key.Scope.String())
	}
	sort.Strings(out)
	return out
}

// Len is how many keys the set holds.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.keys)
}
