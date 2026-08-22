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

// submit stores a job through the API and gives back its identifier.
func submit(t *testing.T, handler http.Handler, body string) string {
	t.Helper()

	made := withKey(t, handler, "POST", "/v1/jobs", body)
	if made.Code != http.StatusCreated {
		t.Fatalf("POST /v1/jobs = %d, body %s", made.Code, made.Body)
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(made.Body.Bytes(), &created); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	return created.ID
}

func statusOf(t *testing.T, handler http.Handler, id string) string {
	t.Helper()

	got := withKey(t, handler, "GET", "/v1/jobs/"+id, "")
	if got.Code != http.StatusOK {
		t.Fatalf("GET the job = %d", got.Code)
	}

	var job struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &job); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	return job.Status
}

func TestAJobIsCancelledOverHTTP(t *testing.T) {
	handler, _ := serve(t)

	id := submit(t, handler, `{"type":"work"}`)
	if got := withKey(t, handler, "POST", "/v1/jobs/"+id+"/cancel", ""); got.Code != http.StatusOK {
		t.Fatalf("cancel = %d, body %s", got.Code, got.Body)
	}
	if got := statusOf(t, handler, id); got != "cancelled" {
		t.Errorf("status = %q, want cancelled", got)
	}
}

func TestAJobIsRevivedOverHTTP(t *testing.T) {
	handler, _ := serve(t)

	id := submit(t, handler, `{"type":"work"}`)
	withKey(t, handler, "POST", "/v1/jobs/"+id+"/cancel", "")

	if got := withKey(t, handler, "POST", "/v1/jobs/"+id+"/revive", ""); got.Code != http.StatusOK {
		t.Fatalf("revive = %d, body %s", got.Code, got.Body)
	}
	if got := statusOf(t, handler, id); got != "pending" {
		t.Errorf("status = %q, want pending", got)
	}
}

// A job in the wrong state is 409 and not 400.
//
// The request is well formed and would be correct against the same job in
// another state, so a client that retries once the job moves is behaving
// sensibly. 400 tells it never to try again, which is the wrong advice.
func TestTheWrongStateIsAConflict(t *testing.T) {
	handler, _ := serve(t)

	id := submit(t, handler, `{"type":"work"}`)

	// A waiting job cannot be revived, because it is already in the queue.
	got := withKey(t, handler, "POST", "/v1/jobs/"+id+"/revive", "")
	if got.Code != http.StatusConflict {
		t.Fatalf("revive of a waiting job = %d, want 409, body %s", got.Code, got.Body)
	}
	if !strings.Contains(got.Body.String(), "pending") {
		t.Errorf("the answer does not say what state the job is in: %s", got.Body)
	}

	// Cancelling twice is the same shape.
	withKey(t, handler, "POST", "/v1/jobs/"+id+"/cancel", "")
	if again := withKey(t, handler, "POST", "/v1/jobs/"+id+"/cancel", ""); again.Code != http.StatusConflict {
		t.Errorf("cancelling twice = %d, want 409", again.Code)
	}
}

func TestActingOnAnUnknownJobIsFourOhFour(t *testing.T) {
	handler, _ := serve(t)

	for _, verb := range []string{"cancel", "revive"} {
		got := withKey(t, handler, "POST", "/v1/jobs/6f1c0c64-0000-0000-0000-000000000000/"+verb, "")
		if got.Code != http.StatusNotFound {
			t.Errorf("%s of an unknown job = %d, want 404", verb, got.Code)
		}
	}
}

func TestTheNewRoutesNeedTheKey(t *testing.T) {
	handler, _ := serve(t)

	id := submit(t, handler, `{"type":"work"}`)
	for _, verb := range []string{"cancel", "revive"} {
		got := call(t, handler, "POST", "/v1/jobs/"+id+"/"+verb, "", nil)
		if got.Code != http.StatusUnauthorized {
			t.Errorf("%s with no key = %d, want 401", verb, got.Code)
		}
	}
}

// The verbs are POST only. A GET that changes a job would be followed by any
// crawler, any link checker, and any browser prefetching a link.
func TestTheVerbsRefuseAGet(t *testing.T) {
	handler, _ := serve(t)

	id := submit(t, handler, `{"type":"work"}`)
	for _, verb := range []string{"cancel", "revive"} {
		got := withKey(t, handler, "GET", "/v1/jobs/"+id+"/"+verb, "")
		if got.Code == http.StatusOK {
			t.Errorf("GET %s was accepted", verb)
		}
	}
	if got := statusOf(t, handler, id); got != "pending" {
		t.Errorf("a GET changed the job to %q", got)
	}
}
