package client_test

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/client"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/api"
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

	backing := memory.New(store.Options{})
	t.Cleanup(func() { _ = backing.Close() })

	server := httptest.NewServer(api.New(api.Options{
		Store:   backing,
		Metrics: metrics.New(),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		APIKey:  key,
	}).Handler())
	t.Cleanup(server.Close)

	c, err := client.New(client.Config{Server: server.URL, APIKey: key})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return c
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
		APIKey:  key,
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
