package rpc

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Recorder is what an observer reports to.
//
// An interface and not the metrics type, so that this package keeps its one
// direction of dependency and a test can pass a counter of its own.
type Recorder interface {
	GRPCRequest(method, code string, took time.Duration)
	GRPCStream(method, code string)
}

// Observer times the worker protocol.
//
// Every job in this queue is leased and reported over gRPC, and until this
// existed the whole of that path was untimed. The HTTP API, which carries
// submissions and the dashboard, has been measured since the first release.
// So the one number an operator had was for the half of the traffic that does
// not run the work.
type Observer struct {
	to Recorder
}

// NewObserver builds an observer that reports to a recorder.
func NewObserver(to Recorder) *Observer { return &Observer{to: to} }

// Unary times calls that answer once.
func (o *Observer) Unary() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		started := time.Now()
		answer, err := handler(ctx, req)
		o.to.GRPCRequest(shortMethod(info.FullMethod), codeOf(err), time.Since(started))
		return answer, err
	}
}

// Stream counts streams and does not time them.
//
// A stream here is a watch that lives as long as the worker does, so the time
// it was open is a fact about how long the worker ran and not about how fast
// this server is. Putting it in the same histogram as a lease would put hours
// in a bucket beside milliseconds and make the quantiles useless.
//
// The count is still worth having. It says how many watchers connected and
// how their streams ended, which is the difference between a fleet that is
// attached and one that is reconnecting in a loop.
func (o *Observer) Stream() grpc.StreamServerInterceptor {
	return func(
		srv any,
		stream grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		err := handler(srv, stream)
		o.to.GRPCStream(shortMethod(info.FullMethod), codeOf(err))
		return err
	}
}

// shortMethod is the method name without the service in front of it.
//
// The full name is "/quorra.v1.QueueService/Lease". The service is the same
// on every line, so it is noise in a label, and the label is bounded either
// way: these names come from the protocol file and not from a caller.
func shortMethod(full string) string {
	if at := strings.LastIndex(full, "/"); at >= 0 && at+1 < len(full) {
		return full[at+1:]
	}
	if full == "" {
		return "unknown"
	}
	return full
}

// codeOf names the code a call ended with.
//
// status.Code answers OK for a nil error, which is what makes a successful
// call and a failed one the same label until it is asked.
func codeOf(err error) string {
	if err == nil {
		return codes.OK.String()
	}
	return status.Code(err).String()
}
