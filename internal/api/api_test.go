package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/api"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/memory"
)

const key = "a-key-that-somebody-chose"

func serve(t *testing.T) (http.Handler, store.Store) {
	t.Helper()

	backing := memory.New(store.Options{
		Policy: jobs.Policy{MaxRetries: 2, Base: time.Second, Max: time.Minute},
	})
	t.Cleanup(func() { _ = backing.Close() })

	return api.New(api.Options{
		Store:            backing,
		Metrics:          metrics.New(),
		Log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		APIKey:           key,
		MaxBodyBytes:     1 << 16,
		DashboardEnabled: true,
	}).Handler(), backing
}

func call(t *testing.T, handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	for name, value := range headers {
		request.Header.Set(name, value)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func withKey(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return call(t, handler, method, path, body, map[string]string{
		"X-API-Key":    key,
		"Content-Type": "application/json",
	})
}

func TestAJobIsCreatedAndReadBack(t *testing.T) {
	handler, _ := serve(t)

	made := withKey(t, handler, "POST", "/v1/jobs", `{"type":"email","payload":{"to":"a@b.c"},"queue":"mail","priority":7}`)
	if made.Code != http.StatusCreated {
		t.Fatalf("POST /v1/jobs = %d, body %s", made.Code, made.Body)
	}

	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Queue  string `json:"queue"`
	}
	if err := json.Unmarshal(made.Body.Bytes(), &created); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if created.ID == "" || created.Status != "pending" || created.Queue != "mail" {
		t.Fatalf("the answer is %+v", created)
	}
	if got := made.Header().Get("Location"); got != "/v1/jobs/"+created.ID {
		t.Errorf("Location = %q", got)
	}

	read := withKey(t, handler, "GET", "/v1/jobs/"+created.ID, "")
	if read.Code != http.StatusOK {
		t.Fatalf("GET the job = %d, body %s", read.Code, read.Body)
	}
	if !strings.Contains(read.Body.String(), `"to":"a@b.c"`) {
		t.Errorf("the payload did not come back: %s", read.Body)
	}
}

// The key travels in a header and nowhere else.
//
// The old server also read it from the query string, and its own dashboard
// put the key in every URL it fetched. A query string is written to the
// access log of every proxy in front of the server, kept in browser history,
// and sent on in the Referer header. A key that has been in one has to be
// replaced.
func TestTheKeyIsRefusedInTheQueryString(t *testing.T) {
	handler, _ := serve(t)

	got := call(t, handler, "GET", "/v1/queues?api_key="+key, "", nil)
	if got.Code != http.StatusUnauthorized {
		t.Fatalf("a key in the query string was accepted, giving %d", got.Code)
	}
}

func TestEveryGuardedRouteNeedsTheKey(t *testing.T) {
	handler, _ := serve(t)

	routes := []struct{ method, path string }{
		{"POST", "/v1/jobs"},
		{"GET", "/v1/jobs"},
		{"GET", "/v1/jobs/6f1c0c64-0000-0000-0000-000000000000"},
		{"GET", "/v1/queues"},
	}
	for _, route := range routes {
		for _, given := range []string{"", "wrong", key + "x", strings.ToUpper(key)} {
			got := call(t, handler, route.method, route.path, `{"type":"x"}`,
				map[string]string{"X-API-Key": given, "Content-Type": "application/json"})
			if got.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with key %q = %d, want 401", route.method, route.path, given, got.Code)
			}
		}
	}
}

// The health routes must not need a key. One that did would have to carry a
// key in every load balancer and every container definition.
func TestTheHealthRoutesArePublic(t *testing.T) {
	handler, _ := serve(t)

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		if got := call(t, handler, "GET", path, "", nil); got.Code != http.StatusOK {
			t.Errorf("GET %s = %d", path, got.Code)
		}
	}
}

// A missing job is 404, and nothing else is.
//
// The old handler answered 404 to every failure from the store, so a database
// that had fallen over was reported to the client as a missing job.
func TestAMissingJobIsFourOhFour(t *testing.T) {
	handler, _ := serve(t)

	got := withKey(t, handler, "GET", "/v1/jobs/6f1c0c64-0000-0000-0000-000000000000", "")
	if got.Code != http.StatusNotFound {
		t.Errorf("an unknown job gave %d", got.Code)
	}

	// Text that is not an identifier at all is also a missing job, and not a
	// 500 carrying a database error.
	got = withKey(t, handler, "GET", "/v1/jobs/not-a-uuid", "")
	if got.Code != http.StatusNotFound {
		t.Errorf("a malformed identifier gave %d, body %s", got.Code, got.Body)
	}
}

func TestABadRequestIsRefusedWithAReason(t *testing.T) {
	handler, _ := serve(t)

	cases := map[string]string{
		"no type":          `{"payload":{}}`,
		"not json":         `{"type":`,
		"two values":       `{"type":"a"} {"type":"b"}`,
		"unknown field":    `{"type":"a","maxRetries":3}`,
		"negative delay":   `{"type":"a","delay_seconds":-5}`,
		"negative retries": `{"type":"a","max_retries":-1}`,
	}
	for name, body := range cases {
		got := withKey(t, handler, "POST", "/v1/jobs", body)
		if got.Code != http.StatusBadRequest {
			t.Errorf("%s gave %d, want 400, body %s", name, got.Code, got.Body)
		}
	}
}

// A field the server does not know is refused rather than ignored.
//
// A client sending maxRetries instead of max_retries otherwise gets a job
// carrying the default and no hint that its setting went nowhere.
func TestAMisspeltFieldIsReportedRatherThanIgnored(t *testing.T) {
	handler, _ := serve(t)

	got := withKey(t, handler, "POST", "/v1/jobs", `{"type":"a","maxRetries":9}`)
	if got.Code != http.StatusBadRequest {
		t.Fatalf("a misspelt field was accepted, giving %d", got.Code)
	}
	if !strings.Contains(got.Body.String(), "maxRetries") {
		t.Errorf("the answer does not name the field: %s", got.Body)
	}
}

// Zero retries means zero.
//
// The old handler used a plain integer, where zero and absent are the same
// value, so a caller asking for no retries silently received three.
func TestAskingForNoRetriesGivesNoRetries(t *testing.T) {
	handler, backing := serve(t)

	made := withKey(t, handler, "POST", "/v1/jobs", `{"type":"once","max_retries":0}`)
	if made.Code != http.StatusCreated {
		t.Fatalf("POST = %d, body %s", made.Code, made.Body)
	}

	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(made.Body.Bytes(), &created)

	job, err := backing.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.MaxRetries != 0 {
		t.Errorf("max retries = %d, want the 0 that was asked for", job.MaxRetries)
	}
}

func TestABodyOverTheLimitIsRefused(t *testing.T) {
	handler, _ := serve(t)

	huge := `{"type":"a","payload":{"x":"` + strings.Repeat("y", 1<<17) + `"}}`
	got := withKey(t, handler, "POST", "/v1/jobs", huge)
	if got.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized body gave %d, want 413", got.Code)
	}
}

func TestAnEmptyListIsAnArrayAndNotNull(t *testing.T) {
	handler, _ := serve(t)

	got := withKey(t, handler, "GET", "/v1/jobs", "")
	if !strings.Contains(got.Body.String(), `"jobs":[]`) {
		t.Errorf("an empty list came back as %s", got.Body)
	}

	got = withKey(t, handler, "GET", "/v1/queues", "")
	if !strings.Contains(got.Body.String(), `"queues":[]`) {
		t.Errorf("an empty list came back as %s", got.Body)
	}
}

func TestTheLimitIsChecked(t *testing.T) {
	handler, _ := serve(t)

	for _, limit := range []string{"0", "-1", "2000", "many"} {
		got := withKey(t, handler, "GET", "/v1/jobs?limit="+limit, "")
		if got.Code != http.StatusBadRequest {
			t.Errorf("limit=%s gave %d, want 400", limit, got.Code)
		}
	}
	if got := withKey(t, handler, "GET", "/v1/jobs?limit=10", ""); got.Code != http.StatusOK {
		t.Errorf("limit=10 gave %d", got.Code)
	}
}
