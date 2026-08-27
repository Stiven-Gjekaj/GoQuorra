package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// MostJobTypes is how many job types get a label value of their own.
//
// The type of a job is chosen by whoever submits it. Every distinct value of
// a label is a time series the metrics store keeps for as long as its
// retention says, so a caller that puts an identifier in a job type takes
// down the metrics store, and every dashboard with it, without ever meaning
// to. Every other label in this package is filled in from configuration,
// which is why this is the first one that needs a bound.
//
// Fifty because a deployment with more than fifty kinds of work has a
// naming problem that a metric will not fix, and a reader looking at fifty
// rows can still find the one that is failing.
const MostJobTypes = 50

// bounded keeps a label from growing without end.
//
// The first MostJobTypes values seen keep their own name. Everything after
// that is counted under "other". Nothing is dropped and no counter
// undercounts: a sum over the label is still every job, which is what makes
// this safe to do quietly.
//
// quorra_job_types_tracked says how many names are being kept. Sitting at the
// bound is how an operator finds out that "other" is holding more than it
// looks.
type bounded struct {
	mu      sync.Mutex
	seen    map[string]struct{}
	most    int
	tracked prometheus.Gauge
}

func newBounded(most int, tracked prometheus.Gauge) *bounded {
	return &bounded{seen: make(map[string]struct{}), most: most, tracked: tracked}
}

// label gives the value to record name under.
func (b *bounded) label(name string) string {
	// An empty label value is legal and reads as a mistake in the exporter. A
	// word says that the queue knows there was a type and does not know what
	// it was, which is the true answer.
	if name == "" {
		return "unknown"
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if _, known := b.seen[name]; known {
		return name
	}
	// A name already being kept is kept whatever the bound says, which is the
	// reason the check above comes first. The alternative folds a type that
	// has its own series into "other" partway through a day, and the series
	// then stops for no reason a reader can see.
	if len(b.seen) >= b.most {
		return "other"
	}

	b.seen[name] = struct{}{}
	b.tracked.Set(float64(len(b.seen)))
	return name
}
