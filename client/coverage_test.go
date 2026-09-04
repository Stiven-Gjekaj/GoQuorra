package client_test

import (
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/api"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
)

// reached names the method of this package that calls each guarded route.
//
// The value is read by a person and not by the test. What the test checks is
// that the two lists hold the same patterns, so a route added to the server
// fails this until somebody either writes the method or records here that
// there is deliberately none.
var reached = map[string]string{
	"POST /v1/jobs":                     "Submit",
	"POST /v1/jobs/bulk":                "SubmitMany",
	"GET /v1/jobs":                      "List, and Each over it",
	"GET /v1/jobs/{id}":                 "Get",
	"GET /v1/jobs/{id}/attempts":        "Attempts",
	"POST /v1/jobs/cancel":              "CancelMatching",
	"POST /v1/jobs/revive":              "ReviveMatching",
	"POST /v1/jobs/{id}/cancel":         "Cancel",
	"POST /v1/jobs/{id}/revive":         "Revive",
	"GET /v1/queues":                    "Queues",
	"GET /v1/workers":                   "Workers",
	"POST /v1/schedules":                "CreateSchedule",
	"GET /v1/schedules":                 "Schedules",
	"GET /v1/schedules/{name}":          "Schedule",
	"DELETE /v1/schedules/{name}":       "DeleteSchedule",
	"POST /v1/schedules/{name}/enable":  "EnableSchedule",
	"POST /v1/schedules/{name}/disable": "DisableSchedule",
	"GET /v1/whoami":                    "Whoami",
}

// This package reaches every guarded route the server serves.
//
// It reached eleven of eighteen, and nothing said so. A caller wanting the
// other seven wrote the requests by hand and kept a second copy of the
// shapes, which is the thing this package exists to save them from.
//
// The list comes from the server rather than from this file, so adding a
// route to the router fails this test. That is the point: a list written here
// agrees with whatever its author believed and goes on agreeing for ever.
func TestThisPackageReachesEveryGuardedRoute(t *testing.T) {
	served := api.New(api.Options{
		Metrics: metrics.New(),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Routes()

	var missing, gone []string
	seen := map[string]bool{}

	for _, route := range served {
		// The public routes are a health check and a page. Neither is
		// something a producer calls from Go.
		if !route.NeedsKey() {
			continue
		}
		seen[route.Pattern] = true
		if _, found := reached[route.Pattern]; !found {
			missing = append(missing, route.Pattern)
		}
	}
	for pattern := range reached {
		if !seen[pattern] {
			gone = append(gone, pattern)
		}
	}

	sort.Strings(missing)
	sort.Strings(gone)
	if len(missing) > 0 {
		t.Errorf("the server serves routes this package cannot reach:\n  %s",
			strings.Join(missing, "\n  "))
	}
	if len(gone) > 0 {
		t.Errorf("this list names routes the server does not serve:\n  %s",
			strings.Join(gone, "\n  "))
	}
}
