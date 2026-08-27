package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/api"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/auth"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/memory"
)

const key = "a-key-that-somebody-chose"

// The tool is driven against the real server, in memory.
//
// A fake HTTP server would test the flag parsing and agree with whatever the
// test author believed the API answers. This is the same handler the binary
// talks to, so a route renamed on one side fails here.
func serve(t *testing.T) []string {
	t.Helper()
	flags, _ := serveWithStore(t)
	return flags
}

// serveWithStore also hands back the store behind the server.
//
// Leasing and reporting are the gRPC side, and this tool speaks only HTTP.
// A test about what a job did has to make something happen to the job first.
func serveWithStore(t *testing.T) ([]string, *memory.Store) {
	t.Helper()

	backing := memory.New(store.Options{})
	t.Cleanup(func() { _ = backing.Close() })

	handler := api.New(api.Options{
		Store:   backing,
		Metrics: metrics.New(),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Keys:    testKeys(t, key),
	}).Handler()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return []string{"-server", server.URL, "-key", key}, backing
}

// runCLI runs one command and gives back what it printed.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer
	err := run(args, &out)
	return out.String(), err
}

// cli runs a command with the connection flags in front of any argument.
func cli(t *testing.T, flags []string, command string, rest ...string) (string, error) {
	t.Helper()

	args := append([]string{command}, flags...)
	return runCLI(t, append(args, rest...)...)
}

func TestCreateThenGet(t *testing.T) {
	flags := serve(t)

	printed, err := cli(t, flags, "create", "-type", "email", "-payload", `{"to":"a@b.c"}`)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	id := strings.TrimSpace(printed)
	if id == "" {
		t.Fatal("create printed no identifier")
	}

	shown, err := cli(t, flags, "get", id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	for _, want := range []string{id, "email", "pending", "a@b.c"} {
		if !strings.Contains(shown, want) {
			t.Errorf("get does not show %q:\n%s", want, shown)
		}
	}
}

// workers shows what the queue has heard from.
//
// The answer to "is anything out there", which no other command gives.
func TestWorkersShowsWhatTheQueueHasHeardFrom(t *testing.T) {
	flags, backing := serveWithStore(t)

	nothing, err := cli(t, flags, "workers")
	if err != nil {
		t.Fatalf("workers: %v", err)
	}
	if !strings.Contains(nothing, "No worker has asked") {
		t.Errorf("workers printed %q", nothing)
	}

	// An ask that finds nothing is still an ask.
	if _, err := backing.Lease(t.Context(), store.LeaseRequest{
		Queue: "invoices", WorkerID: "mailer-3", Limit: 1, TTL: time.Minute,
	}); err != nil {
		t.Fatalf("Lease: %v", err)
	}

	got, err := cli(t, flags, "workers")
	if err != nil {
		t.Fatalf("workers: %v", err)
	}
	for _, want := range []string{"WORKER", "QUEUE", "IDLE", "mailer-3", "invoices"} {
		if !strings.Contains(got, want) {
			t.Errorf("workers printed %q, want it to hold %q", got, want)
		}
	}
}

// history shows one line for each run of a job.
//
// A table and not the JSON get prints. The question it answers is which
// worker kept failing and whether it was getting slower, and that is read
// down a column.
func TestHistoryShowsOneLineForEachRun(t *testing.T) {
	flags, backing := serveWithStore(t)
	ctx := t.Context()

	printed, _ := cli(t, flags, "create", "-type", "work")
	id := strings.TrimSpace(printed)

	// A job that has not run says so rather than printing an empty table.
	nothing, err := cli(t, flags, "history", id)
	if err != nil {
		t.Fatalf("history of a job that has not run: %v", err)
	}
	if !strings.Contains(nothing, "has not finished a run") {
		t.Errorf("history printed %q", nothing)
	}

	held, err := backing.Lease(ctx, store.LeaseRequest{
		Queue: "default", WorkerID: "mailer-3", Limit: 1, TTL: time.Minute,
	})
	if err != nil || len(held) != 1 {
		t.Fatalf("Lease: %v, %d jobs", err, len(held))
	}
	if _, err := backing.Report(ctx, store.Report{
		JobID: id, LeaseID: held[0].LeaseID,
		Outcome: jobs.OutcomeFailed, Error: "upstream said no",
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}

	got, err := cli(t, flags, "history", id)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	for _, want := range []string{"RUN", "WORKER", "mailer-3", "failed", "upstream said no"} {
		if !strings.Contains(got, want) {
			t.Errorf("history printed %q, want it to hold %q", got, want)
		}
	}
}

// A run that worked prints nothing in the error column.
//
// The field is omitempty, so a run that finished carries no key at all.
// Printing the missing value with %v puts the word nil in every one of those
// rows, which reads as an error nobody can look up. Found by running the
// command, not by a test.
func TestARunThatWorkedPrintsNoError(t *testing.T) {
	flags, backing := serveWithStore(t)
	ctx := t.Context()

	printed, _ := cli(t, flags, "create", "-type", "work")
	id := strings.TrimSpace(printed)

	held, err := backing.Lease(ctx, store.LeaseRequest{
		Queue: "default", WorkerID: "w1", Limit: 1, TTL: time.Minute,
	})
	if err != nil || len(held) != 1 {
		t.Fatalf("Lease: %v, %d jobs", err, len(held))
	}
	if _, err := backing.Report(ctx, store.Report{
		JobID: id, LeaseID: held[0].LeaseID, Outcome: jobs.OutcomeDone,
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}

	got, err := cli(t, flags, "history", id)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if strings.Contains(got, "nil") {
		t.Errorf("history printed %q, and a run that worked has no error to show", got)
	}
}

// A run whose start is not known prints a dash and not a duration.
//
// A job leased by a build older than the history carries no start. Measuring
// from the zero time would give every one of those rows the same wrong
// answer, in years, and it would look like a real number.
func TestARunWithNoKnownStartPrintsADash(t *testing.T) {
	if got := took(nil, "2026-01-01T00:00:00Z"); got != "-" {
		t.Errorf("a run with no start printed %q, want a dash", got)
	}
	if got := took("", "2026-01-01T00:00:00Z"); got != "-" {
		t.Errorf("a run with an empty start printed %q, want a dash", got)
	}
	if got := took("2026-01-01T00:00:00Z", nil); got != "-" {
		t.Errorf("a run with no end printed %q, want a dash", got)
	}

	// A run with both prints the difference, so the dash is not simply
	// always the answer.
	if got := took("2026-01-01T00:00:00Z", "2026-01-01T00:00:02Z"); got != "2s" {
		t.Errorf("a two second run printed %q", got)
	}
}

// whoami answers the name and the scope of the key in use.
//
// A profile that exports QUORRA_API_KEY gives no hint of which key it is,
// and a key that may only read looks exactly like one that may write until
// something is refused.
func TestWhoamiNamesTheKeyInUse(t *testing.T) {
	flags := serve(t)

	printed, err := cli(t, flags, "whoami")
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if !strings.Contains(printed, "test") {
		t.Errorf("whoami printed %q, want the name of the key", printed)
	}
	if !strings.Contains(printed, "write") {
		t.Errorf("whoami printed %q, want the scope of the key", printed)
	}
}

// The line an action prints names the key the queue recorded.
//
// An operator with two keys in a shell profile finds out here that the
// cancel went down under the wrong name, rather than a month later when
// somebody reads the job and asks who stopped it.
func TestAnActionSaysWhichKeyTheQueueRecorded(t *testing.T) {
	flags := serve(t)

	printed, _ := cli(t, flags, "create", "-type", "work")
	id := strings.TrimSpace(printed)

	stopped, err := cli(t, flags, "cancel", id)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !strings.Contains(stopped, "by test") {
		t.Errorf("cancel printed %q, want the name of the key beside it", stopped)
	}
}

func TestCancelAndRevive(t *testing.T) {
	flags := serve(t)

	printed, _ := cli(t, flags, "create", "-type", "work")
	id := strings.TrimSpace(printed)

	stopped, err := cli(t, flags, "cancel", id)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !strings.Contains(stopped, "cancelled") {
		t.Errorf("cancel printed %q", stopped)
	}

	back, err := cli(t, flags, "revive", id)
	if err != nil {
		t.Fatalf("revive: %v", err)
	}
	if !strings.Contains(back, "pending") {
		t.Errorf("revive printed %q", back)
	}
}

// A refusal from the server reaches the person at the terminal.
//
// The tool reads the body whatever the status, because the server explains
// itself there. Reporting the status alone would send somebody to the server
// logs for a sentence already sitting in their terminal.
func TestAServerRefusalIsExplained(t *testing.T) {
	flags := serve(t)

	printed, _ := cli(t, flags, "create", "-type", "work")
	id := strings.TrimSpace(printed)

	_, err := cli(t, flags, "revive", id)
	if err == nil {
		t.Fatal("reviving a waiting job was reported as success")
	}
	if !strings.Contains(err.Error(), "pending") {
		t.Errorf("the error does not say what state the job is in: %v", err)
	}
}

func TestAMissingKeyIsRefusedBeforeTheRequest(t *testing.T) {
	server := httptest.NewServer(nil)
	t.Cleanup(server.Close)

	_, err := runCLI(t, "list", "-server", server.URL, "-key", "")
	if err == nil {
		t.Fatal("a command with no key was sent anyway")
	}
	if !strings.Contains(err.Error(), "QUORRA_API_KEY") {
		t.Errorf("the error does not say how to set the key: %v", err)
	}
}

func TestABadPayloadIsCaughtBeforeTheRequest(t *testing.T) {
	flags := serve(t)

	_, err := cli(t, flags, "create", "-type", "work", "-payload", "{oops")
	if err == nil {
		t.Fatal("a payload that is not JSON was sent")
	}
	if !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}

func TestAnUnknownCommandIsRefused(t *testing.T) {
	if _, err := runCLI(t, "frobnicate"); err == nil {
		t.Fatal("an unknown command was accepted")
	}
	if _, err := runCLI(t); err == nil {
		t.Fatal("no command at all was accepted")
	}
	if _, err := runCLI(t, "help"); err != nil {
		t.Fatalf("help: %v", err)
	}
}

func TestListAndQueues(t *testing.T) {
	flags := serve(t)

	empty, err := cli(t, flags, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(empty, "No jobs") {
		t.Errorf("an empty list printed %q", empty)
	}

	cli(t, flags, "create", "-type", "alpha", "-queue", "one")
	cli(t, flags, "create", "-type", "beta", "-queue", "two")

	listed, err := cli(t, flags, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"alpha", "beta", "one", "two", "0 of 4"} {
		if !strings.Contains(listed, want) {
			t.Errorf("list does not show %q:\n%s", want, listed)
		}
	}

	counted, err := cli(t, flags, "queues")
	if err != nil {
		t.Fatalf("queues: %v", err)
	}
	if !strings.Contains(counted, "one") || !strings.Contains(counted, "pending") {
		t.Errorf("queues printed %q", counted)
	}
}

// An option after an argument works.
//
// The flag package stops parsing at the first thing that is not an option, so
// this is the shape that fails without the reordering: every other tool on
// the machine takes them in either order, and somebody who types them that
// way gets a message about how many job identifiers they gave.
func TestAnOptionAfterAnArgumentWorks(t *testing.T) {
	flags := serve(t)

	printed, _ := cli(t, flags, "create", "-type", "work")
	id := strings.TrimSpace(printed)

	// The identifier first, then the connection options.
	shown, err := runCLI(t, append([]string{"get", id}, flags...)...)
	if err != nil {
		t.Fatalf("get with the options last: %v", err)
	}
	if !strings.Contains(shown, id) {
		t.Errorf("get printed %q", shown)
	}

	// And mixed through.
	stopped, err := runCLI(t, "cancel", flags[0], flags[1], id, flags[2], flags[3])
	if err != nil {
		t.Fatalf("cancel with the options either side: %v", err)
	}
	if !strings.Contains(stopped, "cancelled") {
		t.Errorf("cancel printed %q", stopped)
	}
}

// The value of an option is not mistaken for an argument.
//
// -limit takes the token after it and a boolean does not, so the reordering
// asks the flag set rather than guessing.
func TestAnOptionKeepsItsValue(t *testing.T) {
	flags := serve(t)

	cli(t, flags, "create", "-type", "alpha")
	cli(t, flags, "create", "-type", "beta")

	listed, err := runCLI(t, append([]string{"list", "-limit", "1"}, flags...)...)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Contains(listed, "alpha") {
		t.Errorf("the limit was not applied:\n%s", listed)
	}

	// And in the equals form.
	same, err := runCLI(t, append([]string{"list", "-limit=1"}, flags...)...)
	if err != nil {
		t.Fatalf("list with an equals: %v", err)
	}
	if strings.Contains(same, "alpha") {
		t.Errorf("the limit was not applied:\n%s", same)
	}
}

// Everything after -- is an argument, whatever it looks like.
func TestADoubleDashEndsTheOptions(t *testing.T) {
	flags := serve(t)

	args := append([]string{"get"}, flags...)
	args = append(args, "--", "-not-a-flag")

	_, err := runCLI(t, args...)
	if err == nil {
		t.Fatal("a job identifier of -not-a-flag was accepted")
	}
	// It reaches the server as an identifier and comes back missing, rather
	// than being refused by the flag package as an unknown option.
	if strings.Contains(err.Error(), "flag provided but not defined") {
		t.Errorf("the token after -- was read as an option: %v", err)
	}
}

// The tool names an option that has to exist.
//
// The last page of a listing printed "add -all, or -before <id>", and -before
// was never registered, so following the instruction answered "flag provided
// but not defined". The message named a flag the tool refused.
func TestTheOptionTheListNamesIsRegistered(t *testing.T) {
	flags := serve(t)

	cli(t, flags, "create", "-type", "alpha")
	cli(t, flags, "create", "-type", "beta")

	first, err := cli(t, flags, "list", "-limit", "1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(first, "-before ") {
		t.Fatalf("the page does not offer -before:\n%s", first)
	}

	// Take the identifier the message gives and hand it straight back, which
	// is what somebody reading the message does.
	fields := strings.Fields(first[strings.Index(first, "-before "):])
	if len(fields) < 2 {
		t.Fatalf("the message carries no identifier: %q", first)
	}

	second, err := cli(t, flags, "list", "-limit", "1", "-before", fields[1])
	if err != nil {
		t.Fatalf("the option the message named was refused: %v", err)
	}
	if strings.Contains(second, fields[1]) {
		t.Errorf("the second page repeats the job the cursor named:\n%s", second)
	}
}

// The tool can answer the questions somebody asks when a queue is stuck.
func TestListFindsWhatIsReadyAndInWhatOrder(t *testing.T) {
	flags := serve(t)

	cli(t, flags, "create", "-type", "waiting", "-delay", "3600")
	cli(t, flags, "create", "-type", "ready")

	// A job that has stopped. Its run_at is in the past, so a filter that
	// only asks what is due matches it, and -ready says it lists what the
	// queue would hand out now.
	made, err := cli(t, flags, "create", "-type", "stopped")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := cli(t, flags, "cancel", strings.TrimSpace(made)); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	ready, err := cli(t, flags, "list", "-ready")
	if err != nil {
		t.Fatalf("list -ready: %v", err)
	}
	if !strings.Contains(ready, "ready") || strings.Contains(ready, "waiting") {
		t.Errorf("-ready showed the delayed job:\n%s", ready)
	}
	// Nothing that has finished. A job the queue would hand out now is
	// pending as well as due, and asking only what is due lists every job
	// that has ever run.
	for _, gone := range []string{"succeeded", "dead", "cancelled"} {
		if strings.Contains(ready, gone) {
			t.Errorf("-ready listed a %s job, which the queue will not hand out:\n%s", gone, ready)
		}
	}

	// A column for when a job runs, because a list sorted by a value it does
	// not show reads as a list that is not sorted.
	if !strings.Contains(ready, "RUNS AT") {
		t.Errorf("-ready does not show when a job runs:\n%s", ready)
	}

	sorted, err := cli(t, flags, "list", "-soonest")
	if err != nil {
		t.Fatalf("list -soonest: %v", err)
	}
	if !strings.Contains(sorted, "RUNS AT") {
		t.Errorf("-soonest does not show when a job runs:\n%s", sorted)
	}
	if strings.Index(sorted, "ready") > strings.Index(sorted, "waiting") {
		t.Errorf("-soonest put the delayed job first:\n%s", sorted)
	}

	// And the plain listing keeps its old shape, with no extra column.
	plain, err := cli(t, flags, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Contains(plain, "RUNS AT") {
		t.Errorf("a plain listing grew a column:\n%s", plain)
	}
}

func TestListNarrowsToOneWorker(t *testing.T) {
	flags := serve(t)
	cli(t, flags, "create", "-type", "work")

	// Nothing has leased it, so no worker holds it.
	held, err := cli(t, flags, "list", "-worker", "worker-7")
	if err != nil {
		t.Fatalf("list -worker: %v", err)
	}
	if !strings.Contains(held, "No jobs") {
		t.Errorf("worker-7 holds something before anything leased it:\n%s", held)
	}
}

func TestListFilters(t *testing.T) {
	flags := serve(t)

	cli(t, flags, "create", "-type", "alpha", "-queue", "one")
	cli(t, flags, "create", "-type", "beta", "-queue", "two")

	byQueue, err := cli(t, flags, "list", "-queue", "one")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(byQueue, "alpha") || strings.Contains(byQueue, "beta") {
		t.Errorf("the queue filter did not narrow the list:\n%s", byQueue)
	}

	byType, err := cli(t, flags, "list", "-type", "beta")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(byType, "beta") || strings.Contains(byType, "alpha") {
		t.Errorf("the type filter did not narrow the list:\n%s", byType)
	}

	empty, err := cli(t, flags, "list", "-status", "dead")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(empty, "No jobs") {
		t.Errorf("an empty result printed %q", empty)
	}
}

// A page that is not the last says so, and says how to see the rest.
//
// Printing every job in a table holding a month of them is a decision to make
// for somebody rather than on their behalf.
func TestListSaysWhenThereIsMore(t *testing.T) {
	flags := serve(t)

	for i := 0; i < 5; i++ {
		cli(t, flags, "create", "-type", "work")
	}

	page, err := cli(t, flags, "list", "-limit", "2")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(page, "There are more") || !strings.Contains(page, "-all") {
		t.Errorf("the page does not say how to see the rest:\n%s", page)
	}
	if strings.Count(page, " of 4") != 2 {
		t.Errorf("a limit of 2 printed a different number of rows:\n%s", page)
	}

	every, err := cli(t, flags, "list", "-limit", "2", "-all")
	if err != nil {
		t.Fatalf("list -all: %v", err)
	}
	if strings.Count(every, " of 4") != 5 {
		t.Errorf("-all did not follow the pages:\n%s", every)
	}
	if strings.Contains(every, "There are more") {
		t.Errorf("-all still offered more:\n%s", every)
	}

	// The heading is printed once, and not on every page.
	if strings.Count(every, "ATTEMPTS") != 1 {
		t.Errorf("the heading was printed %d times:\n%s", strings.Count(every, "ATTEMPTS"), every)
	}
}

func TestListRefusesAStatusThatDoesNotExist(t *testing.T) {
	flags := serve(t)

	_, err := cli(t, flags, "list", "-status", "processing")
	if err == nil {
		t.Fatal("a status that does not exist was accepted")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("the error does not list the valid statuses: %v", err)
	}
}

// testKeys builds a one key set for a test harness.
//
// Named "test" and allowed to write, because these harnesses drive every
// route. A test about scopes builds its own set rather than using this.
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
