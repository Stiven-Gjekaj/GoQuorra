package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

var now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func job(status jobs.Status, queue string, created time.Time) *store.Job {
	return &store.Job{Status: status, Queue: queue, CreatedAt: created}
}

// Two sets of metrics can exist at once.
//
// promauto registers on the global default registry, so a second set panics
// on a duplicate registration and no test can ever build one. That is the
// reason this package owns its registry, and this is the test that says so.
func TestTwoSetsCanExistAtOnce(t *testing.T) {
	first := New()
	second := New()

	first.JobCreated()
	if got := testutil.ToFloat64(first.created); got != 1 {
		t.Errorf("the first set counts %v", got)
	}
	if got := testutil.ToFloat64(second.created); got != 0 {
		t.Errorf("the second set counts %v, so the two share state", got)
	}
}

// A buried job raises the dead counter and not the retry counter.
//
// The old code raised one failure counter on every failure, the last one
// included, and never raised the dead counter at all. The dead letter panel
// on any dashboard read zero for ever, and the failure rate counted the
// burial twice.
func TestABuriedJobIsCountedAsBuriedAndNotAsARetry(t *testing.T) {
	m := New()

	m.JobFinished(job(jobs.Pending, "default", now.Add(-time.Minute)), now)
	m.JobFinished(job(jobs.Pending, "default", now.Add(-time.Minute)), now)
	m.JobFinished(job(jobs.Dead, "default", now.Add(-time.Minute)), now)

	if got := testutil.ToFloat64(m.dead); got != 1 {
		t.Errorf("dead = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.retried); got != 2 {
		t.Errorf("retried = %v, want the two that went back to the queue", got)
	}
	if got := testutil.ToFloat64(m.succeeded); got != 0 {
		t.Errorf("succeeded = %v", got)
	}
}

func TestASucceededJobIsCounted(t *testing.T) {
	m := New()
	m.JobFinished(job(jobs.Succeeded, "mail", now.Add(-30*time.Second)), now)

	if got := testutil.ToFloat64(m.succeeded); got != 1 {
		t.Errorf("succeeded = %v, want 1", got)
	}
}

// A job that went back to the queue has not finished, so it contributes no
// lifetime. Counting it would report the length of one attempt as though it
// were the length of the job, and drag the published figure down.
func TestOnlyAFinishedJobContributesALifetime(t *testing.T) {
	m := New()

	m.JobFinished(job(jobs.Pending, "default", now.Add(-time.Minute)), now)
	if count := testutil.CollectAndCount(m.lifetime); count != 0 {
		t.Errorf("a retried job recorded %d lifetimes", count)
	}

	m.JobFinished(job(jobs.Succeeded, "default", now.Add(-90*time.Second)), now)
	if count := testutil.CollectAndCount(m.lifetime); count != 1 {
		t.Errorf("a finished job recorded %d lifetimes, want 1", count)
	}

	// The lifetime runs from acceptance and not from the last attempt, so it
	// includes the waiting, the retries and the backoff. That is the time
	// somebody who submitted the job actually waited.
	page := gather(t, m)
	if !strings.Contains(page, `quorra_job_lifetime_seconds_sum{queue="default",status="succeeded"} 90`) {
		t.Errorf("the lifetime is not 90 seconds:\n%s", page)
	}
}

// A queue that empties must report zero, and not keep its last value.
//
// Nothing sends a zero for a row the database has stopped returning, so
// without the reset an alert on queue depth stays lit after the queue has
// drained.
func TestAQueueThatEmptiesStopsBeingReported(t *testing.T) {
	m := New()

	m.SetQueueLengths([]store.QueueStat{
		{Queue: "mail", Status: jobs.Pending, Count: 12},
		{Queue: "default", Status: jobs.Pending, Count: 3},
	})
	if got := testutil.ToFloat64(m.queueLength.WithLabelValues("mail", "pending")); got != 12 {
		t.Fatalf("mail pending = %v, want 12", got)
	}

	// The mail queue drains, so the next refresh does not mention it.
	m.SetQueueLengths([]store.QueueStat{
		{Queue: "default", Status: jobs.Pending, Count: 3},
	})

	page := gather(t, m)
	if strings.Contains(page, `quorra_queue_length{queue="mail"`) {
		t.Errorf("the drained queue is still published:\n%s", page)
	}
	if !strings.Contains(page, `quorra_queue_length{queue="default",status="pending"} 3`) {
		t.Errorf("the queue that still has jobs is missing:\n%s", page)
	}
}

func TestCountersIgnoreNothingAndNegatives(t *testing.T) {
	m := New()

	m.JobsLeased(0)
	m.JobsLeased(-3)
	m.LeasesReclaimed(0)
	m.JobFinished(nil, now)

	if got := testutil.ToFloat64(m.leased); got != 0 {
		t.Errorf("leased = %v after leasing nothing", got)
	}
	if got := testutil.ToFloat64(m.reclaimed); got != 0 {
		t.Errorf("reclaimed = %v after reclaiming nothing", got)
	}

	m.JobsLeased(4)
	if got := testutil.ToFloat64(m.leased); got != 4 {
		t.Errorf("leased = %v, want 4", got)
	}
}

func TestTheMetricsPageNamesEveryCounter(t *testing.T) {
	m := New()

	// Move every counter, because a Prometheus counter that has never been
	// touched is still published at zero, and a test over an untouched set
	// would pass even if nothing were wired to it.
	m.JobCreated()
	m.JobsLeased(2)
	m.LeasesReclaimed(1)
	m.JobFinished(job(jobs.Succeeded, "default", now.Add(-time.Second)), now)
	m.JobFinished(job(jobs.Dead, "default", now.Add(-time.Second)), now)
	m.JobFinished(job(jobs.Pending, "default", now.Add(-time.Second)), now)
	m.HTTPRequest("/v1/jobs", "POST", "201", 12*time.Millisecond)

	page := gather(t, m)
	for _, name := range []string{
		"quorra_jobs_created_total 1",
		"quorra_jobs_leased_total 2",
		"quorra_jobs_succeeded_total 1",
		"quorra_jobs_retried_total 1",
		"quorra_jobs_dead_total 1",
		"quorra_leases_reclaimed_total 1",
		"quorra_http_request_duration_seconds_count",
	} {
		if !strings.Contains(page, name) {
			t.Errorf("the page does not hold %q", name)
		}
	}
}

// gather reads the page the server actually serves.
//
// Through the handler rather than through the registry. The handler is what a
// Prometheus server scrapes, so a fault in the way it is built shows up here
// rather than in production, and this needs no second formatting library to
// do it.
func gather(t *testing.T, m *Metrics) string {
	t.Helper()

	recorder := httptest.NewRecorder()
	m.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("the metrics page answered %d", recorder.Code)
	}
	return recorder.Body.String()
}
