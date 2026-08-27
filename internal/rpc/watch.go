package rpc

import (
	"strings"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/quorrapb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MostWatchedQueues bounds how many queues one worker may watch.
//
// A worker asks for the queues it serves, and a fleet that watched fifty
// would be one that is woken by work it cannot take.
const MostWatchedQueues = 16

// Watch tells a worker when a queue it serves may have work.
//
// It holds the stream open and sends nothing but queue names. The worker
// answers by calling Lease, which is the call that decides.
//
// A hint and never a promise. The worker still polls, so a hint that is lost
// costs latency and nothing else, and this is what makes the whole feature
// safe to add to a protocol whose correctness already worked without it.
func (s *Service) Watch(req *quorrapb.WatchRequest, stream quorrapb.QueueService_WatchServer) error {
	wanted := map[string]bool{}
	for _, queue := range req.GetQueues() {
		if name := strings.TrimSpace(queue); name != "" {
			wanted[name] = true
		}
	}

	// Empty is refused rather than read as "all of them". A worker that
	// watched every queue would be woken by work it cannot take, and a
	// caller that meant to name one and sent none should hear about it.
	if len(wanted) == 0 {
		return status.Error(codes.InvalidArgument,
			"name at least one queue to watch. Watching every queue would wake this worker for work it cannot take.")
	}
	if len(wanted) > MostWatchedQueues {
		return status.Errorf(codes.InvalidArgument,
			"this asks to watch %d queues, and one worker may watch %d", len(wanted), MostWatchedQueues)
	}

	ctx := stream.Context()
	hints, err := s.store.Watch(ctx)
	if err != nil {
		return status.Errorf(codes.Unavailable, "cannot watch for work: %v", err)
	}

	caller := CallerOf(ctx)
	s.log.Debug("a worker is watching",
		"worker", req.GetWorkerId(), "queues", req.GetQueues(), "key", caller.Name)

	for {
		select {
		case <-ctx.Done():
			return nil
		case queue, open := <-hints:
			if !open {
				// The store stopped watching. The worker falls back to its
				// poll, which is what it was doing before this call existed.
				return status.Error(codes.Unavailable, "the server stopped watching for work")
			}
			if !wanted[queue] {
				continue
			}
			if err := stream.Send(&quorrapb.WatchResponse{
				Queue:  queue,
				Reason: "a job is ready",
			}); err != nil {
				return err
			}
		}
	}
}

// ensure the service satisfies the generated interface, including the stream.
var _ quorrapb.QueueServiceServer = (*Service)(nil)
