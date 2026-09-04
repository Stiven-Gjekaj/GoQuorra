package client_test

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/client"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/api"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/auth"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/memory"
)

const key = "a-key-that-somebody-chose"

// The package is driven against the real API handler.
//
// A stub server would agree with whatever the test author believed the API
// answers, so a field renamed on one side would go on passing here.
func connect(t *testing.T) *client.Client {
	t.Helper()
	c, _ := connectWithStore(t)
	return c
}

// connectWithStore also hands back the store behind the server.
//
// Leasing and reporting are the gRPC side, and this package deliberately
// speaks only HTTP. A test about what a job did has to make something
// happen to the job first, and this is the honest way to do it without
// pretending the client can lease.
func connectWithStore(t *testing.T) (*client.Client, *memory.Store) {
	t.Helper()

	backing := memory.New(store.Options{})
	t.Cleanup(func() { _ = backing.Close() })

	server := httptest.NewServer(api.New(api.Options{
		Store:   backing,
		Metrics: metrics.New(),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Keys:    testKeys(t, key),
	}).Handler())
	t.Cleanup(server.Close)

	c, err := client.New(client.Config{Server: server.URL, APIKey: key})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return c, backing
}

func TestSubmitAndGet(t *testing.T) {
	c := connect(t)
	ctx := t.Context()

	type mail struct {
		To string `json:"to"`
	}

	made, err := c.Submit(ctx, client.NewJob{
		Type:     "email",
		Payload:  mail{To: "a@b.c"},
		Queue:    "post",
		Priority: 7,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if made.ID == "" || made.Status != "pending" || made.Queue != "post" {
		t.Fatalf("the answer is %+v", made)
	}

	got, err := c.Get(ctx, made.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Type != "email" || got.Priority != 7 {
		t.Errorf("the job is %+v", got)
	}

	var payload mail
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("the payload is not what was sent: %v", err)
	}
	if payload.To != "a@b.c" {
		t.Errorf("the payload is %+v", payload)
	}
}

// A refusal reaches the caller as an error it can test, and not as a status
// code it has to interpret.
func TestRefusalsAreTypedErrors(t *testing.T) {
	c := connect(t)
	ctx := t.Context()

	if _, err := c.Get(ctx, "6f1c0c64-0000-0000-0000-000000000000"); !errors.Is(err, client.ErrNotFound) {
		t.Errorf("an unknown job gave %v, want ErrNotFound", err)
	}

	made, err := c.Submit(ctx, client.NewJob{Type: "work"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// A waiting job cannot be revived, because it is already in the queue.
	_, err = c.Revive(ctx, made.ID)
	if !errors.Is(err, client.ErrWrongState) {
		t.Errorf("reviving a waiting job gave %v, want ErrWrongState", err)
	}
	// And the server's own sentence comes with it.
	if err != nil && !strings.Contains(err.Error(), "pending") {
		t.Errorf("the error does not say what state the job is in: %v", err)
	}
}

func TestAWrongKeyIsItsOwnError(t *testing.T) {
	backing := memory.New(store.Options{})
	t.Cleanup(func() { _ = backing.Close() })

	server := httptest.NewServer(api.New(api.Options{
		Store:   backing,
		Metrics: metrics.New(),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Keys:    testKeys(t, key),
	}).Handler())
	t.Cleanup(server.Close)

	c, err := client.New(client.Config{Server: server.URL, APIKey: "wrong"})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	if _, err := c.Submit(t.Context(), client.NewJob{Type: "work"}); !errors.Is(err, client.ErrUnauthorized) {
		t.Errorf("a wrong key gave %v, want ErrUnauthorized", err)
	}
}

func TestCancelAndRevive(t *testing.T) {
	c := connect(t)
	ctx := t.Context()

	made, _ := c.Submit(ctx, client.NewJob{Type: "work"})

	stopped, err := c.Cancel(ctx, made.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if stopped.Status != "cancelled" || !stopped.Finished() {
		t.Errorf("the job is %+v", stopped)
	}

	back, err := c.Revive(ctx, made.ID)
	if err != nil {
		t.Fatalf("Revive: %v", err)
	}
	if back.Status != "pending" || back.Finished() {
		t.Errorf("the job is %+v", back)
	}
}

// Many jobs are submitted in one call, and one bad one does not lose the rest.
func TestAProducerSubmitsManyJobsInOneCall(t *testing.T) {
	c := connect(t)
	ctx := t.Context()

	results, err := c.SubmitMany(ctx, []client.NewJob{
		{Type: "a", Payload: map[string]int{"n": 1}},
		{Type: "b", Queue: "mail"},
		{Type: "c", Key: "k1"},
	})
	if err != nil {
		t.Fatalf("SubmitMany: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("the answer holds %d results, want 3", len(results))
	}
	for i, one := range results {
		if one.Index != i || one.ID == "" || !one.Created || one.Error != "" {
			t.Errorf("result %d is %+v", i, one)
		}
	}

	// The jobs are really there, with what was sent.
	first, err := c.Get(ctx, results[0].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(first.Payload) != `{"n":1}` {
		t.Errorf("the payload is %s, want what was sent", first.Payload)
	}

	// A job the server refuses is reported beside the others rather than
	// ending the call.
	mixed, err := c.SubmitMany(ctx, []client.NewJob{
		{Type: "good"},
		{Type: "bad", After: []string{"8de1a3d0-0000-0000-0000-000000000000"}},
	})
	if err != nil {
		t.Fatalf("SubmitMany with one bad job: %v", err)
	}
	if mixed[0].Error != "" || mixed[0].ID == "" {
		t.Errorf("the good job is %+v", mixed[0])
	}
	if mixed[1].Error == "" {
		t.Errorf("the bad job is %+v, want it to say why", mixed[1])
	}
}

// A job with no type is refused here rather than being sent.
//
// The single path already refuses it, and the bulk path builds its bodies
// through the same function, so a field added to one is in the other.
func TestSubmitManyRefusesAJobWithNoType(t *testing.T) {
	c := connect(t)

	if _, err := c.SubmitMany(t.Context(), []client.NewJob{{Type: "good"}, {}}); err == nil {
		t.Error("a job with no type was sent")
	}
	if _, err := c.SubmitMany(t.Context(), nil); err == nil {
		t.Error("an empty list was sent")
	}
}

// A dead letter queue is cleared in one call.
func TestAClientClearsADeadLetterQueueInOneCall(t *testing.T) {
	c, backing := connectWithStore(t)
	ctx := t.Context()

	none := 0
	for i := 0; i < 3; i++ {
		made, err := c.Submit(ctx, client.NewJob{Type: "charge", MaxRetries: &none})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		held, err := backing.Lease(ctx, store.LeaseRequest{
			Queue: "default", WorkerID: "w1", Limit: 1, TTL: time.Minute,
		})
		if err != nil || len(held) != 1 {
			t.Fatalf("Lease: %v, %d jobs", err, len(held))
		}
		if _, err := backing.Report(ctx, store.Report{
			JobID: made.ID, LeaseID: held[0].LeaseID, Outcome: jobs.OutcomeFailed, Error: "no",
		}); err != nil {
			t.Fatalf("Report: %v", err)
		}
	}

	moved, err := c.ReviveMatching(ctx, client.Many{Status: "dead", Limit: 100})
	if err != nil {
		t.Fatalf("ReviveMatching: %v", err)
	}
	if moved != 3 {
		t.Errorf("the revive moved %d jobs, want 3", moved)
	}
}

// A bulk call with no limit is refused here rather than at the server.
//
// A mistake reported next to the call that made it is one somebody can fix.
// Sending it and reading a 400 back tells them the same thing a round trip
// later, and only if they are watching.
func TestABulkCallNeedsALimit(t *testing.T) {
	// Pointed at an address nothing is listening on. A check on this side
	// answers about the limit, and a call that goes out answers about the
	// connection, so the two cannot be confused. Against a real server both
	// give an error and the test would pass either way.
	c, err := client.New(client.Config{Server: "http://127.0.0.1:1", APIKey: key})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	for name, m := range map[string]client.Many{
		"no limit":         {Status: "dead"},
		"a negative limit": {Status: "dead", Limit: -1},
	} {
		_, err := c.CancelMatching(t.Context(), m)
		if err == nil {
			t.Fatalf("%s: a bulk call was sent", name)
		}
		if !strings.Contains(err.Error(), "needs a limit") {
			t.Errorf("%s: the call went out and failed on the connection: %v", name, err)
		}
	}
}

// A producer can submit a job that follows another.
//
// The pipeline case: extract, then load, and the load must not run on a half
// written table.
func TestAProducerCanSubmitAJobThatFollowsAnother(t *testing.T) {
	c, backing := connectWithStore(t)
	ctx := t.Context()

	first, err := c.Submit(ctx, client.NewJob{Type: "extract"})
	if err != nil {
		t.Fatalf("Submit the first: %v", err)
	}

	second, err := c.Submit(ctx, client.NewJob{Type: "load", After: []string{first.ID}})
	if err != nil {
		t.Fatalf("Submit the second: %v", err)
	}
	if !second.Waiting() {
		t.Fatalf("the second job is %q, want it waiting", second.Status)
	}
	if second.Finished() {
		t.Error("a waiting job says it is finished")
	}
	if len(second.After) != 1 || second.After[0] != first.ID {
		t.Errorf("the job waits for %v, want the first job", second.After)
	}

	held, err := backing.Lease(ctx, store.LeaseRequest{
		Queue: "default", WorkerID: "w1", Limit: 1, TTL: time.Minute,
	})
	if err != nil || len(held) != 1 || held[0].ID != first.ID {
		t.Fatalf("Lease: %v, %v", err, held)
	}
	if _, err := backing.Report(ctx, store.Report{
		JobID: first.ID, LeaseID: held[0].LeaseID, Outcome: jobs.OutcomeDone,
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}

	released, err := c.Get(ctx, second.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if released.Waiting() {
		t.Errorf("the second job is still %q after its parent succeeded", released.Status)
	}
}

// A job that follows one that is not there is refused, and the answer says
// which one.
func TestAJobCannotFollowAJobThatIsNotThere(t *testing.T) {
	c := connect(t)

	_, err := c.Submit(t.Context(), client.NewJob{
		Type: "load", After: []string{"8de1a3d0-0000-0000-0000-000000000000"},
	})
	if err == nil {
		t.Fatal("a job following a job that is not there was accepted")
	}
	if !strings.Contains(err.Error(), "8de1a3d0") {
		t.Errorf("the error does not name the job that is missing: %v", err)
	}
}

// A producer can ask whether anything is out there.
//
// Worth checking before waiting on a job. A queue with a thousand waiting
// jobs and no worker looks exactly like a queue that is busy.
func TestAClientCanAskWhetherAnythingIsOutThere(t *testing.T) {
	c, backing := connectWithStore(t)
	ctx := t.Context()

	nothing, err := c.Workers(ctx)
	if err != nil {
		t.Fatalf("Workers: %v", err)
	}
	if len(nothing) != 0 {
		t.Errorf("nothing has asked, and the answer holds %d workers", len(nothing))
	}

	if _, err := backing.Lease(ctx, store.LeaseRequest{
		Queue: "default", WorkerID: "idle-1", Limit: 1, TTL: time.Minute,
	}); err != nil {
		t.Fatalf("Lease: %v", err)
	}

	seen, err := c.Workers(ctx)
	if err != nil {
		t.Fatalf("Workers: %v", err)
	}
	if len(seen) != 1 || seen[0].ID != "idle-1" || seen[0].Queue != "default" {
		t.Fatalf("the answer is %+v", seen)
	}
	if seen[0].FirstSeenAt.IsZero() || seen[0].LastSeenAt.IsZero() {
		t.Errorf("the moments did not survive the round trip: %+v", seen[0])
	}
	if idle := seen[0].Idle(); idle < 0 || idle > time.Minute {
		t.Errorf("the worker has been idle %s, which is not a moment ago", idle)
	}
}

// A client reads what a job did, run by run.
func TestAClientReadsWhatAJobDid(t *testing.T) {
	c, backing := connectWithStore(t)
	ctx := t.Context()

	made, err := c.Submit(ctx, client.NewJob{Type: "work"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// A job that has not run has no history, and that is not an error.
	nothing, err := c.Attempts(ctx, made.ID)
	if err != nil {
		t.Fatalf("Attempts of a job that has not run: %v", err)
	}
	if len(nothing) != 0 {
		t.Errorf("a job that has not run kept %d runs", len(nothing))
	}

	held, err := backing.Lease(ctx, store.LeaseRequest{
		Queue: "default", WorkerID: "w1", Limit: 1, TTL: time.Minute,
	})
	if err != nil || len(held) != 1 {
		t.Fatalf("Lease: %v, %d jobs", err, len(held))
	}
	if _, err := backing.Report(ctx, store.Report{
		JobID: made.ID, LeaseID: held[0].LeaseID,
		Outcome: jobs.OutcomeFailed, Error: "upstream said no",
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}

	history, err := c.Attempts(ctx, made.ID)
	if err != nil {
		t.Fatalf("Attempts: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("the job kept %d runs, want 1", len(history))
	}

	run := history[0]
	if run.Number != 1 || run.Worker != "w1" || run.Outcome != "failed" {
		t.Errorf("the run is %+v", run)
	}
	if run.Error != "upstream said no" {
		t.Errorf("the run says %q", run.Error)
	}
	if _, known := run.Took(); !known {
		t.Error("the run has no duration, so its start did not survive the round trip")
	}
}

// A job that is not there is told apart from a job that has not run.
func TestTheHistoryOfAMissingJobIsNotFound(t *testing.T) {
	c := connect(t)

	_, err := c.Attempts(t.Context(), "8de1a3d0-0000-0000-0000-000000000000")
	if !errors.Is(err, client.ErrNotFound) {
		t.Fatalf("Attempts of an unknown job gave %v, want ErrNotFound", err)
	}
}

// A client can ask which key it holds.
//
// A producer that starts up asks once and refuses to run, rather than
// failing on the first submission an hour later with a 403 that nobody is
// watching for.
func TestTheClientCanAskWhichKeyItHolds(t *testing.T) {
	c := connect(t)

	who, err := c.Whoami(t.Context())
	if err != nil {
		t.Fatalf("Whoami: %v", err)
	}
	if who.Name != "test" {
		t.Errorf("name = %q, want test", who.Name)
	}
	if !who.CanWrite() {
		t.Errorf("scope = %q, and the harness key may write", who.Scope)
	}
}

// A key that holds everything may write.
//
// The scope a key answers with is a name and not a flag. A key holding
// everything answers "all", and one that also leases answers "write+worker".
// Testing for the one word "write" made this package say that the most
// privileged key there is could not submit a job, which is the answer a
// producer reads at startup to decide whether to run at all.
func TestAKeyThatHoldsEverythingMayWrite(t *testing.T) {
	for scope, want := range map[string]bool{
		"write":        true,
		"all":          true,
		"write+worker": true,
		"read":         false,
		"worker":       false,
		"nothing":      false,
	} {
		if got := (client.Identity{Scope: scope}).CanWrite(); got != want {
			t.Errorf("a %q key answers CanWrite %v, want %v", scope, got, want)
		}
	}
}

// And the same question, asked of a real server holding a real key.
//
// The table above is about the parsing. This is about the word the server
// actually sends, which is the part that would change without this package
// noticing.
func TestAServerHoldingEveryScopeSaysTheKeyMayWrite(t *testing.T) {
	c := connectAs(t, auth.Everything)

	who, err := c.Whoami(t.Context())
	if err != nil {
		t.Fatalf("Whoami: %v", err)
	}
	if who.Scope != "all" {
		t.Fatalf("the server calls every scope %q, and this test is written against \"all\"", who.Scope)
	}
	if !who.CanWrite() {
		t.Error("a key holding every scope answers that it cannot write")
	}
}

// The job a cancel returns names the key that cancelled it.
//
// A producer that cancels its own work reads this back to confirm that the
// action went down under the key it meant to use, rather than under one an
// environment variable supplied.
func TestACancelledJobNamesTheKeyThatStoppedIt(t *testing.T) {
	c := connect(t)
	ctx := t.Context()

	made, _ := c.Submit(ctx, client.NewJob{Type: "work"})
	stopped, err := c.Cancel(ctx, made.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	if stopped.ActedBy != "test" {
		t.Errorf("acted by = %q, want test, the name of the key the harness holds", stopped.ActedBy)
	}
	if stopped.ActedAt == nil {
		t.Fatal("acted at is nil, so the name has no moment beside it")
	}

	// A job nobody has acted on carries neither.
	fresh, _ := c.Submit(ctx, client.NewJob{Type: "work"})
	read, err := c.Get(ctx, fresh.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if read.ActedBy != "" || read.ActedAt != nil {
		t.Errorf("a fresh job claims %q acted on it at %v", read.ActedBy, read.ActedAt)
	}
}

// A key sent twice gives back the first job. This is the reason the field
// exists, and a producer that retries is the caller who needs it.
func TestAKeyMakesASubmissionSafeToRepeat(t *testing.T) {
	c := connect(t)
	ctx := t.Context()

	first, err := c.Submit(ctx, client.NewJob{Type: "charge", Key: "order-77"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	second, err := c.Submit(ctx, client.NewJob{Type: "charge", Key: "order-77"})
	if err != nil {
		t.Fatalf("the repeat: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("the repeat made a second job: %s and %s", first.ID, second.ID)
	}
}

func TestEachWalksEveryPage(t *testing.T) {
	c := connect(t)
	ctx := t.Context()

	const total = 7
	for i := 0; i < total; i++ {
		if _, err := c.Submit(ctx, client.NewJob{Type: "work"}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}

	seen := map[string]int{}
	if err := c.Each(ctx, client.Filter{Limit: 2}, func(job client.Job) error {
		seen[job.ID]++
		return nil
	}); err != nil {
		t.Fatalf("Each: %v", err)
	}

	if len(seen) != total {
		t.Errorf("walked %d jobs, want %d", len(seen), total)
	}
	for id, times := range seen {
		if times != 1 {
			t.Errorf("%s was walked %d times", id, times)
		}
	}
}

// Each stops when the caller says so, and hands back the reason.
func TestEachStopsWhenTheWalkFails(t *testing.T) {
	c := connect(t)
	ctx := t.Context()

	for i := 0; i < 5; i++ {
		c.Submit(ctx, client.NewJob{Type: "work"})
	}

	stop := errors.New("that is enough")
	seen := 0

	err := c.Each(ctx, client.Filter{Limit: 2}, func(client.Job) error {
		seen++
		if seen == 3 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Errorf("Each gave %v, want the walk's own error", err)
	}
	if seen != 3 {
		t.Errorf("the walk ran %d times after asking to stop", seen)
	}
}

// Ready separates the jobs the queue would hand out now from the ones waiting
// out a delay, and Soonest puts them in the order the queue works in.
//
// Both are asked for from a caller that never sends a moment. Ready becomes
// due=now on the wire and the server resolves it, so the answer does not
// depend on the two machines agreeing about the time.
func TestListFindsWhatIsReadyAndInWhatOrder(t *testing.T) {
	c := connect(t)
	ctx := t.Context()

	// Submitted sooner first, so the two orders disagree and the assertion
	// below can tell them apart.
	soon, err := c.Submit(ctx, client.NewJob{Type: "soon", Delay: time.Minute})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	late, err := c.Submit(ctx, client.NewJob{Type: "late", Delay: time.Hour})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	now, err := c.Submit(ctx, client.NewJob{Type: "now"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	ready, err := c.List(ctx, client.Filter{Ready: true, Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ready.Jobs) != 1 || ready.Jobs[0].ID != now.ID {
		t.Errorf("Ready gave %d jobs, want only the one with no delay", len(ready.Jobs))
	}

	sorted, err := c.List(ctx, client.Filter{Soonest: true, Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{now.ID, soon.ID, late.ID}
	for i, job := range sorted.Jobs {
		if i < len(want) && job.ID != want[i] {
			t.Errorf("position %d is %s, want %s", i, job.ID, want[i])
		}
	}

	// The default is the newest first, which is the reverse of that.
	back, err := c.List(ctx, client.Filter{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(back.Jobs) != 3 || back.Jobs[0].ID != now.ID || back.Jobs[2].ID != soon.ID {
		t.Errorf("the default order changed")
	}
}

// Ready leaves out a job that has stopped.
//
// A job that has finished keeps the run_at of its last attempt, so a filter
// that only asks what is due matches every job that has ever run. Ready says
// it gives what the queue would hand out now, so it asks for pending as well.
func TestReadyLeavesOutAJobThatHasStopped(t *testing.T) {
	c := connect(t)
	ctx := t.Context()

	waiting, err := c.Submit(ctx, client.NewJob{Type: "waiting"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	stopped, err := c.Submit(ctx, client.NewJob{Type: "stopped"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := c.Cancel(ctx, stopped.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// Due alone matches both, which is what makes this worth asserting.
	due, err := c.List(ctx, client.Filter{DueBy: time.Now().Add(time.Minute), Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(due.Jobs) != 2 {
		t.Fatalf("due alone gave %d jobs, want both, so this test proves nothing", len(due.Jobs))
	}

	ready, err := c.List(ctx, client.Filter{Ready: true, Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ready.Jobs) != 1 || ready.Jobs[0].ID != waiting.ID {
		got := make([]string, 0, len(ready.Jobs))
		for _, j := range ready.Jobs {
			got = append(got, j.Type+"/"+j.Status)
		}
		t.Errorf("Ready gave %v, want only the pending one", got)
	}
}

func TestListNarrowsToOneWorker(t *testing.T) {
	c := connect(t)
	ctx := t.Context()

	if _, err := c.Submit(ctx, client.NewJob{Type: "held"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Nothing has leased it, so no worker holds anything yet.
	page, err := c.List(ctx, client.Filter{Worker: "worker-7", Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Jobs) != 0 {
		t.Errorf("worker-7 holds %d jobs before anything leased one", len(page.Jobs))
	}
}

func TestListNarrows(t *testing.T) {
	c := connect(t)
	ctx := t.Context()

	c.Submit(ctx, client.NewJob{Type: "alpha", Queue: "one"})
	c.Submit(ctx, client.NewJob{Type: "beta", Queue: "two"})

	page, err := c.List(ctx, client.Filter{Queue: "one", Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Jobs) != 1 || page.Jobs[0].Type != "alpha" {
		t.Errorf("the page holds %d jobs", len(page.Jobs))
	}
	if page.Cursor != "" {
		t.Errorf("a short page carried the cursor %q", page.Cursor)
	}
}

func TestABadConfigurationIsRefusedAtBuildTime(t *testing.T) {
	if _, err := client.New(client.Config{Server: "http://localhost:8080"}); err == nil {
		t.Error("a client with no API key was built")
	}
}

func TestADelayedJobIsHeldBack(t *testing.T) {
	c := connect(t)

	made, err := c.Submit(t.Context(), client.NewJob{Type: "later", Delay: time.Minute})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !made.RunAt.After(time.Now().Add(30 * time.Second)) {
		t.Errorf("run at = %s, want about a minute from now", made.RunAt)
	}

	if _, err := c.Submit(t.Context(), client.NewJob{Type: "x", Delay: -time.Minute}); err == nil {
		t.Error("a negative delay was accepted")
	}
}

func TestSubmitNeedsAType(t *testing.T) {
	c := connect(t)
	if _, err := c.Submit(t.Context(), client.NewJob{}); err == nil {
		t.Error("a job with no type was submitted")
	}
}

// testKeys builds a one key set for a test harness.
//
// Named "test" and allowed to write, because these harnesses drive every
// route. A test about scopes builds its own set rather than using this.
// connectAs builds a server whose one key holds the scope named.
func connectAs(t *testing.T, scope auth.Scope) *client.Client {
	t.Helper()

	backing := memory.New(store.Options{})
	t.Cleanup(func() { _ = backing.Close() })

	only, err := auth.NewKey("test", scope, key)
	if err != nil {
		t.Fatalf("auth.NewKey: %v", err)
	}
	set, err := auth.NewSet(only)
	if err != nil {
		t.Fatalf("auth.NewSet: %v", err)
	}

	server := httptest.NewServer(api.New(api.Options{
		Store:   backing,
		Metrics: metrics.New(),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Keys:    set,
	}).Handler())
	t.Cleanup(server.Close)

	c, err := client.New(client.Config{Server: server.URL, APIKey: key})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return c
}

func testKeys(t *testing.T, secret string) *auth.Set {
	t.Helper()
	key, err := auth.NewKey("test", auth.Write, secret)
	if err != nil {
		t.Fatalf("auth.NewKey: %v", err)
	}
	set, err := auth.NewSet(key)
	if err != nil {
		t.Fatalf("auth.NewSet: %v", err)
	}
	return set
}

// A refusal names the request it came from.
//
// The identifier finds every line the server wrote while it was refusing, so
// a caller asking somebody to look has one string to quote, and does not have
// to know that such a thing exists to end up holding it.
func TestARefusalNamesTheRequest(t *testing.T) {
	c := connect(t)

	_, err := c.Get(t.Context(), "8f14e45f-ceea-467a-9c37-8e8f8f8f8f8f")
	if err == nil {
		t.Fatal("a job that is not there was found")
	}

	if !strings.Contains(err.Error(), "(request ") {
		t.Errorf("the error is %q, and it names no request", err)
	}
}

// An identifier the server would not have sent is left out of the message.
//
// An error message is read by a person, and a page of identifier buries the
// sentence that says what went wrong.
func TestAnEnormousIdentifierIsLeftOutOfTheMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", strings.Repeat("a", 4096))
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no job carries that identifier"}`))
	}))
	t.Cleanup(server.Close)

	c, err := client.New(client.Config{Server: server.URL, APIKey: key})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	_, err = c.Get(t.Context(), "8f14e45f-ceea-467a-9c37-8e8f8f8f8f8f")
	if err == nil {
		t.Fatal("the refusal was not reported")
	}
	if len(err.Error()) > 200 {
		t.Errorf("the message is %d characters long", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "no job carries that identifier") {
		t.Errorf("the sentence that says what went wrong is gone: %q", err)
	}
}
