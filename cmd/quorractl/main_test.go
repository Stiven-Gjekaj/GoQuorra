package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/api"
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

	backing := memory.New(store.Options{})
	t.Cleanup(func() { _ = backing.Close() })

	handler := api.New(api.Options{
		Store:   backing,
		Metrics: metrics.New(),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		APIKey:  key,
	}).Handler()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return []string{"-server", server.URL, "-key", key}
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
