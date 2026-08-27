package rpc

import (
	"context"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/auth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// keyHeader is where a worker puts its key.
//
// The same name the HTTP API uses. gRPC lowercases metadata keys, so a worker
// that sets "X-API-Key" and one that sets "x-api-key" are the same worker,
// and neither has to know which.
const keyHeader = "x-api-key"

// Guard checks the key a worker presented.
//
// The gRPC port had no authentication at all, and docs/milestones.md recorded
// that: a process that could reach it could lease from any queue. The entry
// said the shape of the fix was known and that the work was the key
// distribution rather than the check. Keys came with the named keys, so the
// distribution story is the one a deployment already has for its HTTP keys,
// and this is the check.
//
// A worker needs auth.Work and nothing else. A key an operator keeps in a
// shell profile must not be able to lease the queue empty, and a worker must
// not be able to cancel anything.
type Guard struct {
	keys *auth.Set
}

// NewGuard builds a guard over a set of keys.
func NewGuard(keys *auth.Set) *Guard { return &Guard{keys: keys} }

// Unary returns the interceptor for calls that answer once.
func (g *Guard) Unary() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		next grpc.UnaryHandler,
	) (any, error) {
		caller, err := g.check(ctx)
		if err != nil {
			return nil, err
		}
		return next(withCaller(ctx, caller), req)
	}
}

// Stream returns the interceptor for calls that hold a stream open.
//
// Both are needed. A guard on one of the two is a door beside an open window,
// and the worker protocol is free to grow a streaming call at any time.
func (g *Guard) Stream() grpc.StreamServerInterceptor {
	return func(
		srv any,
		stream grpc.ServerStream,
		info *grpc.StreamServerInfo,
		next grpc.StreamHandler,
	) error {
		caller, err := g.check(stream.Context())
		if err != nil {
			return err
		}
		return next(srv, &guarded{ServerStream: stream, caller: caller})
	}
}

// guarded carries the caller on a stream's context.
type guarded struct {
	grpc.ServerStream
	caller auth.Key
}

func (g *guarded) Context() context.Context {
	return withCaller(g.ServerStream.Context(), g.caller)
}

// check reads the key off a call and resolves it.
func (g *Guard) check(ctx context.Context) (auth.Key, error) {
	presented := ""
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if values := md.Get(keyHeader); len(values) > 0 {
			presented = values[0]
		}
	}

	key, found := g.keys.Lookup(presented)
	if !found {
		// Unauthenticated and not PermissionDenied. Nothing here knows who
		// the caller is, so there is nothing to deny.
		return auth.Key{}, status.Errorf(codes.Unauthenticated,
			"the %s metadata is missing or wrong", keyHeader)
	}

	if !key.Scope.Allows(auth.Work) {
		// PermissionDenied, and the message names the key. The key is real
		// and the server knows whose it is; what is missing is permission,
		// and answering Unauthenticated would send somebody to check a key
		// that is working correctly.
		return auth.Key{}, status.Errorf(codes.PermissionDenied,
			"the key %q may %s, and the worker protocol needs worker", key.Name, key.Scope)
	}

	return key, nil
}

// callerKey is the context key a guard puts a caller under.
type callerKey struct{}

func withCaller(ctx context.Context, key auth.Key) context.Context {
	return context.WithValue(ctx, callerKey{}, key)
}

// CallerOf reads the key that made a call.
//
// It answers a zero key when there is none, which happens only on a server
// built without a guard.
func CallerOf(ctx context.Context) auth.Key {
	key, _ := ctx.Value(callerKey{}).(auth.Key)
	return key
}
