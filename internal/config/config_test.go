package config

import (
	"log/slog"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"strings"
	"testing"
	"time"
)

// The smallest environment that starts a server.
func minimalServer() map[string]string {
	return map[string]string{
		"QUORRA_API_KEY": "a-key-that-somebody-chose",
		"DATABASE_URL":   "postgres://quorra:quorra@localhost:5432/quorra",
	}
}

func TestLoadServerFillsTheDefaults(t *testing.T) {
	got, err := LoadServer(FromMap(minimalServer()))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}

	if got.HTTPAddr != ":8080" || got.GRPCAddr != ":50051" {
		t.Errorf("addresses = %q and %q", got.HTTPAddr, got.GRPCAddr)
	}
	if got.Backend != "postgres" {
		t.Errorf("backend = %q", got.Backend)
	}
	if got.LogLevel != slog.LevelInfo {
		t.Errorf("log level = %v", got.LogLevel)
	}
	if err := got.Policy.Validate(); err != nil {
		t.Errorf("the default policy is not usable: %v", err)
	}
}

// The test this package exists for.
//
// The old reader walked the characters of a value and kept the digits. So -5
// meant 5, "1o" meant 10, and "five" meant the default. Each of those started
// a server running under a setting nobody chose, and nothing anywhere said
// so. Every one of them is now an error naming the variable and the value.
func TestABadNumberIsAnErrorAndNotAGuess(t *testing.T) {
	for _, value := range []string{"-5", "1o", "five", "5.5", "", " ", "0x10"} {
		env := minimalServer()
		env["QUORRA_MAX_RETRIES"] = value

		got, err := LoadServer(FromMap(env))

		// An empty value means the variable is not set, which is not a fault.
		if strings.TrimSpace(value) == "" {
			if err != nil {
				t.Errorf("%q was treated as a bad value: %v", value, err)
			}
			continue
		}

		if value == "-5" {
			// -5 parses as a number, so the reader accepts it and the policy
			// refuses it. Either way it is reported and does not become 5.
			if err == nil {
				t.Errorf("QUORRA_MAX_RETRIES=-5 was accepted, giving %d", got.Policy.MaxRetries)
			}
			continue
		}

		if err == nil {
			t.Errorf("QUORRA_MAX_RETRIES=%q was accepted, giving %d", value, got.Policy.MaxRetries)
		}
	}
}

// Every fault is reported at once.
//
// Reporting only the first makes an operator find them one deployment at a
// time.
func TestEveryFaultIsReportedTogether(t *testing.T) {
	_, err := LoadServer(FromMap(map[string]string{
		"DATABASE_URL":         "postgres://localhost/quorra",
		"QUORRA_MAX_RETRIES":   "many",
		"QUORRA_RETRY_BASE":    "soon",
		"QUORRA_STORE":         "sqlite",
		"QUORRA_WORKER_QUEUES": "a,b",
	}))
	if err == nil {
		t.Fatal("a broken environment was accepted")
	}

	message := err.Error()
	for _, want := range []string{"QUORRA_API_KEY", "QUORRA_MAX_RETRIES", "QUORRA_RETRY_BASE", "QUORRA_STORE"} {
		if !strings.Contains(message, want) {
			t.Errorf("the report does not name %s:\n%s", want, message)
		}
	}
}

func TestTheAPIKeyHasNoDefault(t *testing.T) {
	env := minimalServer()
	delete(env, "QUORRA_API_KEY")

	_, err := LoadServer(FromMap(env))
	if err == nil {
		t.Fatal("a server started with no API key")
	}
	if !strings.Contains(err.Error(), "QUORRA_API_KEY") {
		t.Errorf("the error does not name the variable: %v", err)
	}
}

func TestPostgresNeedsADatabaseURL(t *testing.T) {
	env := minimalServer()
	delete(env, "DATABASE_URL")

	if _, err := LoadServer(FromMap(env)); err == nil {
		t.Fatal("the postgres backend was accepted with no database named")
	}

	// The memory backend needs none, which is what makes it useful for
	// trying the server without installing anything.
	env["QUORRA_STORE"] = "memory"
	if _, err := LoadServer(FromMap(env)); err != nil {
		t.Fatalf("the memory backend was refused: %v", err)
	}
}

func TestDurationsAndLevelsAreParsed(t *testing.T) {
	env := minimalServer()
	env["QUORRA_RETRY_BASE"] = "250ms"
	env["QUORRA_RETRY_MAX"] = "2h"
	env["QUORRA_LOG_LEVEL"] = "debug"

	got, err := LoadServer(FromMap(env))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if got.Policy.Base != 250*time.Millisecond || got.Policy.Max != 2*time.Hour {
		t.Errorf("waits = %s and %s", got.Policy.Base, got.Policy.Max)
	}
	if got.LogLevel != slog.LevelDebug {
		t.Errorf("log level = %v", got.LogLevel)
	}

	env["QUORRA_LOG_LEVEL"] = "chatty"
	if _, err := LoadServer(FromMap(env)); err == nil {
		t.Error("QUORRA_LOG_LEVEL=chatty was accepted")
	}
}

func TestTheQueueListIsSplitAndTrimmed(t *testing.T) {
	got, err := LoadWorker(FromMap(map[string]string{
		"QUORRA_WORKER_QUEUES": " default , email ,, processing ",
	}))
	if err != nil {
		t.Fatalf("LoadWorker: %v", err)
	}

	want := []string{"default", "email", "processing"}
	if len(got.Queues) != len(want) {
		t.Fatalf("queues = %q, want %q", got.Queues, want)
	}
	for i := range want {
		if got.Queues[i] != want[i] {
			t.Errorf("queues = %q, want %q", got.Queues, want)
			break
		}
	}
}

// A worker whose shutdown wait is longer than its lease will still be running
// a job when the server takes that job back and gives it to somebody else.
// The duplicate work that follows is very hard to see from the outside, so
// the pairing is refused at startup.
func TestAShutdownWaitPastTheLeaseIsRefused(t *testing.T) {
	_, err := LoadWorker(FromMap(map[string]string{
		"QUORRA_WORKER_LEASE_TTL": "30s",
		"QUORRA_SHUTDOWN_GRACE":   "60s",
	}))
	if err == nil {
		t.Fatal("a shutdown wait longer than the lease was accepted")
	}
	if !strings.Contains(err.Error(), "given to another worker") {
		t.Errorf("the error does not say what goes wrong: %v", err)
	}
}

// A value that holds only spaces means the variable is not set.
//
// Every reader here trims first, so one setting cannot behave differently
// from the others when a deployment tool writes an empty string into it.
func TestAValueOfSpacesMeansTheVariableIsNotSet(t *testing.T) {
	got, err := LoadWorker(FromMap(map[string]string{
		"QUORRA_WORKER_ID":       "   ",
		"QUORRA_WORKER_QUEUES":   "  ",
		"QUORRA_WORKER_MAX_JOBS": " ",
	}))
	if err != nil {
		t.Fatalf("LoadWorker: %v", err)
	}
	if got.ID != "worker-1" {
		t.Errorf("id = %q, want the default", got.ID)
	}
	if len(got.Queues) != 1 || got.Queues[0] != "default" {
		t.Errorf("queues = %q, want the default", got.Queues)
	}
	if got.MaxJobs != 5 {
		t.Errorf("max jobs = %d, want the default", got.MaxJobs)
	}
}

func TestLoadWorkerRefusesSettingsThatDoNothing(t *testing.T) {
	bad := []map[string]string{
		{"QUORRA_WORKER_MAX_JOBS": "0"},
		{"QUORRA_WORKER_LEASE_TTL": "0s"},
		{"QUORRA_WORKER_POLL_EVERY": "0s"},
	}
	for _, env := range bad {
		if _, err := LoadWorker(FromMap(env)); err == nil {
			t.Errorf("%v was accepted", env)
		}
	}
}

// Every job is kept for ever unless somebody says otherwise.
//
// A queue holds the only record that a piece of work happened. A default that
// quietly removed it would take that record from every deployment that
// upgraded without reading the notes.
func TestNothingIsRemovedByDefault(t *testing.T) {
	got, err := LoadServer(FromMap(minimalServer()))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}

	if got.RemovesAnything() {
		t.Error("the default configuration removes jobs")
	}
	for status, keep := range got.Retention {
		if keep != 0 {
			t.Errorf("%s jobs are kept for %s by default", status, keep)
		}
	}
}

func TestRetentionIsReadPerStatus(t *testing.T) {
	env := minimalServer()
	env["QUORRA_RETAIN_SUCCEEDED"] = "168h"
	env["QUORRA_RETAIN_CANCELLED"] = "24h"

	got, err := LoadServer(FromMap(env))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}

	if got.Retention[jobs.Succeeded] != 168*time.Hour {
		t.Errorf("succeeded = %s", got.Retention[jobs.Succeeded])
	}
	if got.Retention[jobs.Cancelled] != 24*time.Hour {
		t.Errorf("cancelled = %s", got.Retention[jobs.Cancelled])
	}
	// Dead jobs are evidence, and this deployment did not ask for them to go.
	if got.Retention[jobs.Dead] != 0 {
		t.Errorf("dead = %s, want kept for ever", got.Retention[jobs.Dead])
	}
	if !got.RemovesAnything() {
		t.Error("RemovesAnything is false with two retentions set")
	}
}

// A negative retention would put the cutoff in the future and remove every
// job of that status on the first sweep.
func TestANegativeRetentionIsRefused(t *testing.T) {
	env := minimalServer()
	env["QUORRA_RETAIN_SUCCEEDED"] = "-1h"

	if _, err := LoadServer(FromMap(env)); err == nil {
		t.Fatal("a negative retention was accepted")
	}
}
