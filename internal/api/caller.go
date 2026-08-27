package api

import (
	"context"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/auth"
)

// callerKey is the context key the guard puts a caller under.
//
// A type of its own, unexported, so that nothing outside this package can
// write a caller into a context and nothing can collide with it.
type callerKey struct{}

// withCaller puts the key that made a request on its context.
func withCaller(ctx context.Context, key auth.Key) context.Context {
	return context.WithValue(ctx, callerKey{}, key)
}

// callerOf reads the key that made a request.
//
// It answers a zero key when there is none. Every guarded route has one,
// because the guard is what puts it there, so an empty name in a log line
// means a route that is not guarded rather than a caller with no name.
func callerOf(ctx context.Context) auth.Key {
	key, _ := ctx.Value(callerKey{}).(auth.Key)
	return key
}
