package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/memory"
)

// counting wraps a store and records which methods the caller used.
//
// The question this test asks is which call the readiness probe makes, and no
// answer from a working store can say that. A probe that counted every row
// and one that asked for a round trip both answer 200.
type counting struct {
	store.Store

	reachable int
	counted   int

	// refuse is what Reachable gives back, so the test can make the store
	// unreachable without stopping a database.
	refuse error
}

func (c *counting) Reachable(ctx context.Context) error {
	c.reachable++
	return c.refuse
}

func (c *counting) QueueStats(ctx context.Context) ([]store.QueueStat, error) {
	c.counted++
	return c.Store.QueueStats(ctx)
}

func probeAPI(t *testing.T) (http.Handler, *counting) {
	t.Helper()

	backing := memory.New(store.Options{})
	t.Cleanup(func() { _ = backing.Close() })

	watched := &counting{Store: backing}
	return New(Options{
		Store:   watched,
		Metrics: metrics.New(),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:     time.Now,
	}).Handler(), watched
}

// The readiness probe asks whether the store can be reached, and asks for
// nothing else.
//
// It used to ask for a count of every row by queue and by status, which is
// the most expensive read in the system. Measured against 500,003 rows that
// took 57ms and read the whole table, and Kubernetes asks every five seconds
// on every replica, so the check was slowest under exactly the load it is
// there to watch for.
func TestTheReadinessProbeOnlyAsksWhetherTheStoreCanBeReached(t *testing.T) {
	handler, watched := probeAPI(t)

	got := httptest.NewRecorder()
	handler.ServeHTTP(got, httptest.NewRequest("GET", "/readyz", nil))

	if got.Code != http.StatusOK {
		t.Fatalf("the probe answered %d, want 200", got.Code)
	}
	if watched.reachable != 1 {
		t.Errorf("the probe made %d reachability checks, want one", watched.reachable)
	}
	if watched.counted != 0 {
		t.Errorf("the probe counted the queues %d times", watched.counted)
	}
}

// A store it cannot reach answers 503.
//
// Without this the test above passes against a probe that calls Reachable,
// throws the answer away and always says 200, which is worse than the slow
// version: a replica with no database would stay in the load balancer.
func TestAStoreItCannotReachAnswers503(t *testing.T) {
	handler, watched := probeAPI(t)
	watched.refuse = errors.New("dial tcp: connection refused")

	got := httptest.NewRecorder()
	handler.ServeHTTP(got, httptest.NewRequest("GET", "/readyz", nil))

	if got.Code != http.StatusServiceUnavailable {
		t.Fatalf("a store that cannot be reached answered %d, want 503", got.Code)
	}

	// The answer says the store, and not what the driver said. A probe answer
	// is read by a load balancer and by whoever is looking at the outage, and
	// neither needs the address the pool tried.
	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if body.Status == "" {
		t.Errorf("the answer carries no reason: %s", got.Body.String())
	}
	if body.Status == "dial tcp: connection refused" {
		t.Error("the answer repeats what the driver said")
	}
}

// The liveness probe reaches nothing at all.
//
// It must stay the cheap one. Pointing it at the store means a database that
// goes away for a minute restarts every replica, which is the one thing
// guaranteed to make the outage worse. The manifests say so, and this is the
// rule that holds it.
func TestTheLivenessProbeReachesNothing(t *testing.T) {
	handler, watched := probeAPI(t)

	got := httptest.NewRecorder()
	handler.ServeHTTP(got, httptest.NewRequest("GET", "/healthz", nil))

	if got.Code != http.StatusOK {
		t.Fatalf("the liveness probe answered %d, want 200", got.Code)
	}
	if watched.reachable != 0 || watched.counted != 0 {
		t.Errorf("the liveness probe touched the store: %d reachability checks, %d counts",
			watched.reachable, watched.counted)
	}
}
