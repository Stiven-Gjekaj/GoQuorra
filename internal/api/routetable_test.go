package api

import (
	"io"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
)

// documented finds the routes the README's own table names.
//
// The table is the reference a person reads before writing a caller. It is
// kept by hand, and the router is kept by hand, so the two are two copies of
// one list. This repository keeps one copy of CLAUDE.md for exactly that
// reason, and the route table had drifted: POST /v1/jobs, the route that
// submits a job, was not in it.
var documented = regexp.MustCompile("(?m)^\\| `([A-Z]+ /v1[^`]*)`")

func readmeRoutes(t *testing.T) []string {
	t.Helper()

	// Two directories up, because this package is internal/api and the README
	// is at the top of the repository.
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("cannot read the README: %v", err)
	}

	var found []string
	for _, match := range documented.FindAllStringSubmatch(string(raw), -1) {
		found = append(found, match[1])
	}
	if len(found) == 0 {
		t.Fatal("the README names no routes, so this test is reading the wrong thing")
	}
	sort.Strings(found)
	return found
}

// The README's route table and the router hold the same routes.
//
// It fails both ways round. A route added to the router and not written down
// is a route a caller cannot find. A route written down and not served is a
// caller writing against something that answers 404.
func TestTheRouteTableAndTheRouterAgree(t *testing.T) {
	served := api(t).Routes()

	var routed []string
	for _, route := range served {
		if !strings.HasPrefix(strings.SplitN(route.Pattern, " ", 2)[1], "/v1") {
			continue
		}
		routed = append(routed, route.Pattern)
	}
	sort.Strings(routed)

	written := readmeRoutes(t)

	missing := notIn(routed, written)
	if len(missing) > 0 {
		t.Errorf("the router serves routes the README does not name:\n  %s",
			strings.Join(missing, "\n  "))
	}
	gone := notIn(written, routed)
	if len(gone) > 0 {
		t.Errorf("the README names routes the router does not serve:\n  %s",
			strings.Join(gone, "\n  "))
	}
}

// notIn gives the entries of first that second does not hold.
func notIn(first, second []string) []string {
	held := make(map[string]bool, len(second))
	for _, one := range second {
		held[one] = true
	}

	var out []string
	for _, one := range first {
		if !held[one] {
			out = append(out, one)
		}
	}
	return out
}

func api(t *testing.T) *API {
	t.Helper()
	return New(Options{
		Metrics: metrics.New(),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}
