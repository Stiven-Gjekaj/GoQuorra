package api

import (
	"context"
	"crypto/sha256"
	"net/http"
	"time"
)

// fold hashes a key to a fixed length.
//
// crypto/subtle.ConstantTimeCompare returns immediately when two slices are
// different lengths, so comparing the raw keys still tells an attacker how
// long the real one is. Hashing both first makes every comparison the same
// size.
func fold(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}
