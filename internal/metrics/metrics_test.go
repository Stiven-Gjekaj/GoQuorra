package metrics

import (
	"fmt"
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

// typed is a job that succeeded, for the tests about the job type label.
func typed(kind string) *store.Job {
	return &store.Job{
		Status:    jobs.Succeeded,
		Queue:     "default",
		Type:      kind,
		CreatedAt: now.Add(-time.Second),
	}
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

	m.JobFinished(job(jobs.Pending, "default", now.Add(-time.Minute)), jobs.OutcomeFailed, now)
	m.JobFinished(job(jobs.Pending, "default", now.Add(-time.Minute)), jobs.OutcomeFailed, now)
	m.JobFinished(job(jobs.Dead, "default", now.Add(-time.Minute)), jobs.OutcomeFailed, now)

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
	m.JobFinished(job(jobs.Succeeded, "mail", now.Add(-30*time.Second)), jobs.OutcomeDone, now)

	if got := testutil.ToFloat64(m.succeeded); got != 1 {
		t.Errorf("succeeded = %v, want 1", got)
	}
}

// A refused job is counted twice on purpose: once as dead and once as
// refused.
//
// The two numbers divide, and the division is the point. A dead letter queue
// filling with refusals says the work being submitted is wrong, and one
// filling with exhausted attempts says something outside is down. Those need
// different people. A refusal counted apart from the deaths rather than
// inside them would not divide, and every dashboard reading dead_total would
// undercount.
func TestARefusedJobIsCountedInsideTheBuriedOnes(t *testing.T) {
	m := New()

	m.JobFinished(job(jobs.Dead, "default", now.Add(-time.Minute)), jobs.OutcomeRefused, now)
	m.JobFinished(job(jobs.Dead, "default", now.Add(-time.Minute)), jobs.OutcomeFailed, now)

	if got := testutil.ToFloat64(m.dead); got != 2 {
		t.Errorf("dead = %v, want both of them", got)
	}
	if got := testutil.ToFloat64(m.refused); got != 1 {
		t.Errorf("refused = %v, want the one the handler refused", got)
	}
	if got := testutil.ToFloat64(m.retried); got != 0 {
		t.Errorf("retried = %v, want none", got)
	}
}

// A refusal that did not bury the job is counted nowhere.
//
// The counter answers what share of the dead letter queue is refusals, and a
// share that can exceed the whole is not an answer. This is the guard on the
// rule that the outcome divides a status and never decides one.
func TestARefusalIsOnlyCountedWhenTheJobIsBuried(t *testing.T) {
	m := New()

	m.JobFinished(job(jobs.Pending, "default", now.Add(-time.Minute)), jobs.OutcomeRefused, now)
	m.JobFinished(job(jobs.Succeeded, "default", now.Add(-time.Minute)), jobs.OutcomeRefused, now)

	if got := testutil.ToFloat64(m.refused); got != 0 {
		t.Errorf("refused = %v, want none, because neither job is dead", got)
	}
}

// A job that went back to the queue has not finished, so it contributes no
// lifetime. Counting it would report the length of one attempt as though it
// were the length of the job, and drag the published figure down.
func TestOnlyAFinishedJobContributesALifetime(t *testing.T) {
	m := New()

	m.JobFinished(job(jobs.Pending, "default", now.Add(-time.Minute)), jobs.OutcomeFailed, now)
	if count := testutil.CollectAndCount(m.lifetime); count != 0 {
		t.Errorf("a retried job recorded %d lifetimes", count)
	}

	m.JobFinished(job(jobs.Succeeded, "default", now.Add(-90*time.Second)), jobs.OutcomeDone, now)
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
	m.JobFinished(nil, jobs.OutcomeDone, now)

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

// A cancellation is counted against the key that asked for it.
//
// One number for the whole deployment says that somebody cancelled forty
// jobs this morning. It does not say which team, and on a queue two teams
// share that is the only part worth acting on.
func TestCancellationsAreCountedByCaller(t *testing.T) {
	m := New()

	m.JobCancelled("ops")
	m.JobCancelled("ops")
	m.JobCancelled("billing")
	m.JobRevived("ops")

	page := gather(t, m)
	for _, want := range []string{
		`quorra_jobs_cancelled_total{caller="ops"} 2`,
		`quorra_jobs_cancelled_total{caller="billing"} 1`,
		`quorra_jobs_revived_total{caller="ops"} 1`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not hold %q", want)
		}
	}
}

// A caller with no name is counted as unknown and not as the empty string.
//
// An empty label value reads as a fault in the exporter. A word says that
// the queue knows there was a caller and does not know which one.
func TestACallerWithNoNameIsCountedAsUnknown(t *testing.T) {
	m := New()
	m.JobCancelled("")

	page := gather(t, m)
	if !strings.Contains(page, `quorra_jobs_cancelled_total{caller="unknown"} 1`) {
		t.Errorf("the page does not name the unknown caller: %s", page)
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
	m.JobFinished(job(jobs.Succeeded, "default", now.Add(-time.Second)), jobs.OutcomeDone, now)
	m.JobFinished(job(jobs.Dead, "default", now.Add(-time.Second)), jobs.OutcomeFailed, now)
	m.JobFinished(job(jobs.Pending, "default", now.Add(-time.Second)), jobs.OutcomeFailed, now)
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

// A job type gets its own row until the bound, and then shares one.
//
// The type of a job is chosen by whoever submits it, so this is the first
// label in the package that a caller fills in. Without a bound, a caller that
// puts an identifier in a job type takes down the metrics store.
func TestAJobTypeKeepsItsOwnRowUntilTheBound(t *testing.T) {
	m := New()

	for i := 0; i < MostJobTypes; i++ {
		m.JobFinished(typed(fmt.Sprintf("type-%d", i)), jobs.OutcomeDone, now)
	}
	m.JobFinished(typed("one-too-many"), jobs.OutcomeDone, now)
	m.JobFinished(typed("another-too-many"), jobs.OutcomeDone, now)

	if got := testutil.ToFloat64(m.finished.WithLabelValues("type-0", "succeeded")); got != 1 {
		t.Errorf("the first type counts %v, want its own row", got)
	}
	if got := testutil.ToFloat64(m.finished.WithLabelValues("one-too-many", "succeeded")); got != 0 {
		t.Errorf("a type past the bound has a row of its own, counting %v", got)
	}
	if got := testutil.ToFloat64(m.finished.WithLabelValues("other", "succeeded")); got != 2 {
		t.Errorf("other counts %v, want both of the types past the bound", got)
	}
	if got := testutil.ToFloat64(m.typesTracked); got != MostJobTypes {
		t.Errorf("quorra_job_types_tracked says %v, want the bound", got)
	}
}

// A type that already has a row keeps it after the bound is reached.
//
// Folding a type that has its own series into "other" partway through a day
// stops that series for no reason a reader of the dashboard can see.
func TestATypeThatHasARowKeepsIt(t *testing.T) {
	m := New()

	m.JobFinished(typed("charge"), jobs.OutcomeDone, now)
	for i := 0; i < MostJobTypes*2; i++ {
		m.JobFinished(typed(fmt.Sprintf("filler-%d", i)), jobs.OutcomeDone, now)
	}
	m.JobFinished(typed("charge"), jobs.OutcomeDone, now)

	if got := testutil.ToFloat64(m.finished.WithLabelValues("charge", "succeeded")); got != 2 {
		t.Errorf("charge counts %v, want both of its jobs", got)
	}
}

// A job with no type is counted as unknown and not as an empty label.
//
// An empty label value is legal and reads as a mistake in the exporter.
func TestAJobWithNoTypeIsCountedAsUnknown(t *testing.T) {
	m := New()

	m.JobFinished(typed(""), jobs.OutcomeDone, now)

	if got := testutil.ToFloat64(m.finished.WithLabelValues("unknown", "succeeded")); got != 1 {
		t.Errorf("a job with no type counts %v under unknown", got)
	}
	if got := testutil.ToFloat64(m.typesTracked); got != 0 {
		t.Errorf("an empty type took one of the %d places", MostJobTypes)
	}
}

// A retried job is counted once, when it stops.
//
// quorra_jobs_finished_total answers which type is failing, and a type whose
// jobs each retry twice before succeeding would otherwise read as three times
// the work.
func TestAFinishedJobIsCountedOnceAndARetryIsNotCounted(t *testing.T) {
	m := New()

	m.JobFinished(typed("charge"), jobs.OutcomeFailed, now)
	m.JobFinished(typed("charge"), jobs.OutcomeFailed, now)

	if got := testutil.ToFloat64(m.finished.WithLabelValues("charge", "pending")); got != 0 {
		t.Errorf("a retry was counted as finished, %v times", got)
	}

	dead := &store.Job{Status: jobs.Dead, Queue: "default", Type: "charge", CreatedAt: now.Add(-time.Minute)}
	m.JobFinished(dead, jobs.OutcomeFailed, now)

	if got := testutil.ToFloat64(m.finished.WithLabelValues("charge", "dead")); got != 1 {
		t.Errorf("the job that stopped counts %v, want one", got)
	}
}
