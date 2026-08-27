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
type Scope int

const (
	// Read allows every route that only reads. It is the smaller of the two,
	// so it is the zero value: a key built without a scope can look and
	// cannot touch.
	Read Scope = iota

	// Write allows everything Read allows, and everything that changes a job.
	Write
)

// ParseScope reads a scope from configuration.
func ParseScope(text string) (Scope, error) {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "read":
		return Read, nil
	case "write":
		return Write, nil
	default:
		return Read, fmt.Errorf("auth: %q is not a scope, and it must be read or write", text)
	}
}

func (s Scope) String() string {
	switch s {
	case Read:
		return "read"
	case Write:
		return "write"
	default:
		return fmt.Sprintf("Scope(%d)", int(s))
	}
}

// Allows says whether this scope covers the one asked for.
//
// Write covers Read. A caller who may change a job may also look at one, and
// a deployment that had to hand out two keys for that would end up handing
// out the larger one twice.
func (s Scope) Allows(wanted Scope) bool { return s >= wanted }

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
