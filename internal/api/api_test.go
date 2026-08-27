package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/api"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/auth"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/reqid"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/memory"
)

const key = "a-key-that-somebody-chose"

// frozen is the moment every test in this file runs at.
//
// Stated and not read, so a test can ask what the queue looked like at a
// moment rather than arranging for its assertion to be true at the instant it
// happens to run.
var frozen = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func serve(t *testing.T) (http.Handler, store.Store) {
	t.Helper()
	handler, backing, _ := serveWithLog(t)
	return handler, backing
}

// serveWithLog also hands back what the server wrote.
func serveWithLog(t *testing.T) (http.Handler, store.Store, *strings.Builder) {
	t.Helper()

	backing := memory.New(store.Options{
		Policy: jobs.Policy{MaxRetries: 2, Base: time.Second, Max: time.Minute},
		Now:    func() time.Time { return frozen },
	})
	t.Cleanup(func() { _ = backing.Close() })

	written := &strings.Builder{}
	return api.New(api.Options{
		Store:            backing,
		Metrics:          metrics.New(),
		Log:              slog.New(slog.NewTextHandler(written, nil)),
		Keys:             testKeys(t, key),
		MaxBodyBytes:     1 << 16,
		DashboardEnabled: true,
		Now:              func() time.Time { return frozen },
	}).Handler(), backing, written
}

// metricsPage reads the page a Prometheus server would scrape.
//
// Through the handler rather than through the registry, because the handler
// is what gets scraped, and a counter that is raised and not registered reads
// the same either way only in the second one.
func metricsPage(t *testing.T, handler http.Handler) string {
	t.Helper()
	return call(t, handler, "GET", "/metrics", "", nil).Body.String()
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

// A key that may only read cannot change a job.
//
// The scope is named at the route rather than checked inside the handler, so
// this test walks every route and asserts the whole policy in one place. A
// route added without a scope is a route anybody can call, and this is what
// notices.
func TestAReadKeyCannotChangeAJob(t *testing.T) {
	backing := memory.New(store.Options{
		Policy: jobs.Policy{MaxRetries: 2, Base: time.Second, Max: time.Minute},
		Now:    func() time.Time { return frozen },
	})
	t.Cleanup(func() { _ = backing.Close() })

	const readSecret = "a-secret-that-may-only-read"
	reader, err := auth.NewKey("watcher", auth.Read, readSecret)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	writer, err := auth.NewKey("ops", auth.Write, key)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	keys, err := auth.NewSet(reader, writer)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}

	handler := api.New(api.Options{
		Store:        backing,
		Metrics:      metrics.New(),
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Keys:         keys,
		MaxBodyBytes: 1 << 16,
		Now:          func() time.Time { return frozen },
	}).Handler()

	// A job to act on, made with the key that may.
	made := call(t, handler, "POST", "/v1/jobs", `{"type":"work"}`,
		map[string]string{"X-API-Key": key, "Content-Type": "application/json"})
	if made.Code != http.StatusCreated {
		t.Fatalf("the write key could not create a job: %d %s", made.Code, made.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(made.Body.Bytes(), &created)

	changes := []struct{ method, path, body string }{
		{"POST", "/v1/jobs", `{"type":"work"}`},
		{"POST", "/v1/jobs/bulk", `{"jobs":[{"type":"work"}]}`},
		{"POST", "/v1/jobs/cancel", `{"status":"pending","limit":1}`},
		{"POST", "/v1/jobs/revive", `{"status":"dead","limit":1}`},
		{"POST", "/v1/jobs/" + created.ID + "/cancel", ""},
		{"POST", "/v1/jobs/" + created.ID + "/revive", ""},
		{"POST", "/v1/schedules", `{"name":"x","cron":"0 3 * * *","type":"r","catch_up":"skip"}`},
		{"POST", "/v1/schedules/nightly/disable", ""},
		{"POST", "/v1/schedules/nightly/enable", ""},
		{"DELETE", "/v1/schedules/nightly", ""},
	}
	for _, change := range changes {
		got := call(t, handler, change.method, change.path, change.body,
			map[string]string{"X-API-Key": readSecret, "Content-Type": "application/json"})

		// 403 and not 401. The key is real and the server knows whose it is.
		// Answering 401 sends the caller to check a key that works.
		if got.Code != http.StatusForbidden {
			t.Errorf("%s %s with a read key = %d, want 403", change.method, change.path, got.Code)
		}
		// The message names the key and both scopes, so the reader learns
		// what to ask for rather than only that they were refused.
		for _, want := range []string{"watcher", "read", "write"} {
			if !strings.Contains(got.Body.String(), want) {
				t.Errorf("%s %s: the answer does not say %q: %s", change.method, change.path, want, got.Body)
			}
		}
	}

	// And the same key reads everything it should.
	for _, path := range []string{
		"/v1/jobs", "/v1/jobs/" + created.ID, "/v1/jobs/" + created.ID + "/attempts",
		"/v1/queues", "/v1/workers", "/v1/schedules",
	} {
		got := call(t, handler, "GET", path, "", map[string]string{"X-API-Key": readSecret})
		if got.Code != http.StatusOK {
			t.Errorf("GET %s with a read key = %d, want 200", path, got.Code)
		}
	}
}

// A caller can ask which key it holds and what that key may do.
//
// Somebody holding a secret out of a configuration file has no other way to
// find out which of several it is, or whether it may write, short of trying
// something that changes a job and reading the refusal.
func TestACallerCanAskWhichKeyItHolds(t *testing.T) {
	handler, _ := serve(t)

	got := withKey(t, handler, "GET", "/v1/whoami", "")
	if got.Code != http.StatusOK {
		t.Fatalf("GET /v1/whoami = %d, body %s", got.Code, got.Body)
	}

	var who struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &who); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if who.Name != "test" || who.Scope != "write" {
		t.Errorf("the answer is %+v, want the test key", who)
	}

	// It says nothing about any other key, and never a secret.
	if strings.Contains(got.Body.String(), key) {
		t.Errorf("the answer carries the secret: %s", got.Body)
	}

	// And it needs a key like every other guarded route.
	if anon := call(t, handler, "GET", "/v1/whoami", "", nil); anon.Code != http.StatusUnauthorized {
		t.Errorf("GET /v1/whoami with no key = %d, want 401", anon.Code)
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

// A repeat schedule is stored and read back, and says when it fires next.
func TestAScheduleIsStoredOverHTTP(t *testing.T) {
	handler, _ := serve(t)

	made := withKey(t, handler, "POST", "/v1/schedules", `{
		"name":"nightly","cron":"0 3 * * *","timezone":"Europe/Berlin",
		"catch_up":"skip","type":"report","payload":{"kind":"summary"},"queue":"reports"
	}`)
	if made.Code != http.StatusCreated {
		t.Fatalf("POST /v1/schedules = %d, body %s", made.Code, made.Body)
	}

	var one struct {
		Name     string `json:"name"`
		Cron     string `json:"cron"`
		Timezone string `json:"timezone"`
		CatchUp  string `json:"catch_up"`
		Enabled  bool   `json:"enabled"`
		Next     string `json:"next_firing_at"`
	}
	if err := json.Unmarshal(made.Body.Bytes(), &one); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if one.Name != "nightly" || one.Cron != "0 3 * * *" || one.Timezone != "Europe/Berlin" {
		t.Errorf("the schedule came back as %+v", one)
	}
	if one.CatchUp != "skip" || !one.Enabled {
		t.Errorf("the schedule came back as %+v", one)
	}

	// When it fires next is worked out on the server. A caller would need a
	// cron parser and this clock to do it, and a browser would answer in
	// whatever zone the reader's machine is set to.
	if one.Next == "" {
		t.Fatal("the schedule does not say when it fires next")
	}
	next, err := time.Parse(time.RFC3339, one.Next)
	if err != nil {
		t.Fatalf("the next firing is not a moment: %v", err)
	}
	if !next.After(frozen) {
		t.Errorf("the next firing is %s, which is not after the clock at %s", next, frozen)
	}

	// And it is really stored.
	read := withKey(t, handler, "GET", "/v1/schedules/nightly", "")
	if read.Code != http.StatusOK {
		t.Fatalf("GET the schedule = %d, body %s", read.Code, read.Body)
	}
	if !strings.Contains(read.Body.String(), "Europe/Berlin") {
		t.Errorf("the stored schedule is %s", read.Body)
	}
}

// The catch up policy is required, and the message says why.
//
// The record called this the part everybody forgets and then argues about, so
// a caller says what it wants rather than discovering what it got.
func TestAScheduleWithNoCatchUpPolicyIsRefused(t *testing.T) {
	handler, _ := serve(t)

	got := withKey(t, handler, "POST", "/v1/schedules",
		`{"name":"nightly","cron":"0 3 * * *","type":"report"}`)
	if got.Code != http.StatusBadRequest {
		t.Fatalf("a schedule with no catch up = %d, want 400, body %s", got.Code, got.Body)
	}
	for _, want := range []string{"catch_up is required", "skip", "all", "none"} {
		if !strings.Contains(got.Body.String(), want) {
			t.Errorf("the answer does not say %q: %s", want, got.Body)
		}
	}

	// A policy nobody knows is named back.
	bad := withKey(t, handler, "POST", "/v1/schedules",
		`{"name":"nightly","cron":"0 3 * * *","type":"report","catch_up":"maybe"}`)
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), "maybe") {
		t.Errorf("an unknown policy = %d, body %s", bad.Code, bad.Body)
	}
}

// A rule or a zone the server cannot read is 400 and names what is wrong.
func TestAScheduleTheServerCannotReadIsRefused(t *testing.T) {
	handler, _ := serve(t)

	cases := map[string]string{
		"a rule that is not a rule": `{"name":"a","cron":"every night","type":"r","catch_up":"skip"}`,
		"a rule with four fields":   `{"name":"a","cron":"0 3 * *","type":"r","catch_up":"skip"}`,
		"a zone that is not a zone": `{"name":"a","cron":"0 3 * * *","timezone":"Mars/Olympus","type":"r","catch_up":"skip"}`,
		"no job type":               `{"name":"a","cron":"0 3 * * *","type":"","catch_up":"skip"}`,
		"no name":                   `{"name":"","cron":"0 3 * * *","type":"r","catch_up":"skip"}`,
	}
	for name, body := range cases {
		if got := withKey(t, handler, "POST", "/v1/schedules", body); got.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400, body %s", name, got.Code, got.Body)
		}
	}
}

// A schedule is switched off rather than deleted, and says nothing about when
// it fires next while it is off.
func TestAScheduleIsSwitchedOffAndOn(t *testing.T) {
	handler, _ := serve(t)

	withKey(t, handler, "POST", "/v1/schedules",
		`{"name":"nightly","cron":"0 3 * * *","type":"report","catch_up":"skip"}`)

	off := withKey(t, handler, "POST", "/v1/schedules/nightly/disable", "")
	if off.Code != http.StatusOK {
		t.Fatalf("disable = %d, body %s", off.Code, off.Body)
	}
	if strings.Contains(off.Body.String(), "next_firing_at") {
		t.Errorf("a schedule that is off says when it fires next: %s", off.Body)
	}

	on := withKey(t, handler, "POST", "/v1/schedules/nightly/enable", "")
	if !strings.Contains(on.Body.String(), "next_firing_at") {
		t.Errorf("a schedule that is on does not say when it fires next: %s", on.Body)
	}

	// Removing it says that the jobs it produced are kept, because a caller
	// who expected them to go should find out here.
	gone := withKey(t, handler, "DELETE", "/v1/schedules/nightly", "")
	if gone.Code != http.StatusOK {
		t.Fatalf("delete = %d, body %s", gone.Code, gone.Body)
	}
	if !strings.Contains(gone.Body.String(), "work that happened") {
		t.Errorf("the answer does not say what happens to the jobs: %s", gone.Body)
	}

	if after := withKey(t, handler, "GET", "/v1/schedules/nightly", ""); after.Code != http.StatusNotFound {
		t.Errorf("the schedule is still there: %d", after.Code)
	}
}

// Many jobs are submitted in one request.
//
// A producer with a thousand rows to queue otherwise makes a thousand
// requests, and the round trips cost more than the work does.
func TestManyJobsAreSubmittedInOneRequest(t *testing.T) {
	handler, _ := serve(t)

	got := withKey(t, handler, "POST", "/v1/jobs/bulk", `{"jobs":[
		{"type":"a"},
		{"type":"b","queue":"mail","priority":5},
		{"type":"c","idempotency_key":"k1"}
	]}`)
	if got.Code != http.StatusOK {
		t.Fatalf("bulk submit = %d, body %s", got.Code, got.Body)
	}

	var answer struct {
		Created int `json:"created"`
		Refused int `json:"refused"`
		Results []struct {
			Index   int    `json:"index"`
			ID      string `json:"id"`
			Created bool   `json:"created"`
			Error   string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if answer.Created != 3 || answer.Refused != 0 {
		t.Fatalf("created %d and refused %d, want 3 and 0: %s", answer.Created, answer.Refused, got.Body)
	}
	for i, one := range answer.Results {
		if one.Index != i || one.ID == "" || !one.Created {
			t.Errorf("result %d is %+v", i, one)
		}
	}

	// The idempotency key still holds across a bulk submission.
	again := withKey(t, handler, "POST", "/v1/jobs/bulk", `{"jobs":[{"type":"c","idempotency_key":"k1"}]}`)
	var second struct {
		Created int `json:"created"`
		Results []struct {
			ID      string `json:"id"`
			Created bool   `json:"created"`
		} `json:"results"`
	}
	_ = json.Unmarshal(again.Body.Bytes(), &second)
	if second.Created != 0 || second.Results[0].Created {
		t.Errorf("a repeated key created a second job: %s", again.Body)
	}
	if second.Results[0].ID != answer.Results[2].ID {
		t.Errorf("a repeated key gave back a different job")
	}
}

// One bad job does not lose the good ones.
//
// Jobs are independent, so one transaction for the batch is the wrong shape:
// a single bad payload would lose the nine hundred and ninety nine beside it.
func TestOneBadJobDoesNotLoseTheGoodOnes(t *testing.T) {
	handler, _ := serve(t)

	got := withKey(t, handler, "POST", "/v1/jobs/bulk", `{"jobs":[
		{"type":"good"},
		{"type":""},
		{"type":"also-good"}
	]}`)
	if got.Code != http.StatusOK {
		t.Fatalf("bulk submit = %d, body %s", got.Code, got.Body)
	}

	var answer struct {
		Created int `json:"created"`
		Refused int `json:"refused"`
		Results []struct {
			Index int    `json:"index"`
			ID    string `json:"id"`
			Error string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if answer.Created != 2 || answer.Refused != 1 {
		t.Fatalf("created %d and refused %d, want 2 and 1: %s", answer.Created, answer.Refused, got.Body)
	}

	// The failure names which row it was, because that is what the caller
	// needs to fix it.
	if answer.Results[1].Index != 1 || answer.Results[1].Error == "" {
		t.Errorf("the refused row is %+v", answer.Results[1])
	}
	if answer.Results[0].ID == "" || answer.Results[2].ID == "" {
		t.Error("a job beside the bad one was lost")
	}
}

// A request with no jobs, or with too many, is refused.
func TestABulkSubmissionIsBounded(t *testing.T) {
	handler, _ := serve(t)

	if got := withKey(t, handler, "POST", "/v1/jobs/bulk", `{"jobs":[]}`); got.Code != http.StatusBadRequest {
		t.Errorf("an empty submission = %d, want 400", got.Code)
	}

	var many []string
	for i := 0; i < 1001; i++ {
		many = append(many, `{"type":"work"}`)
	}
	got := withKey(t, handler, "POST", "/v1/jobs/bulk", `{"jobs":[`+strings.Join(many, ",")+`]}`)
	if got.Code != http.StatusBadRequest {
		t.Errorf("a submission of 1001 jobs = %d, want 400, body %s", got.Code, got.Body)
	}
	if !strings.Contains(got.Body.String(), "1000") {
		t.Errorf("the answer does not say how many one request may carry: %s", got.Body)
	}
}

// A dead letter queue is cleared in one request.
func TestJobsAreRevivedInOneRequest(t *testing.T) {
	handler, backing := serve(t)
	ctx := t.Context()

	// Three jobs the store has buried, and one that is waiting.
	var buried []string
	for i := 0; i < 3; i++ {
		id := submit(t, handler, `{"type":"charge","max_retries":0}`)
		held, err := backing.Lease(ctx, store.LeaseRequest{
			Queue: "default", WorkerID: "w1", Limit: 1, TTL: time.Minute,
		})
		if err != nil || len(held) != 1 {
			t.Fatalf("Lease: %v, %d jobs", err, len(held))
		}
		if _, err := backing.Report(ctx, store.Report{
			JobID: id, LeaseID: held[0].LeaseID, Outcome: jobs.OutcomeFailed, Error: "no",
		}); err != nil {
			t.Fatalf("Report: %v", err)
		}
		buried = append(buried, id)
	}
	waiting := submit(t, handler, `{"type":"charge"}`)

	got := withKey(t, handler, "POST", "/v1/jobs/revive", `{"status":"dead","limit":100}`)
	if got.Code != http.StatusOK {
		t.Fatalf("revive = %d, body %s", got.Code, got.Body)
	}

	var answer struct {
		Moved int `json:"moved"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if answer.Moved != 3 {
		t.Fatalf("the revive moved %d jobs, want 3: %s", answer.Moved, got.Body)
	}

	for _, id := range buried {
		if status := statusOf(t, handler, id); status != "pending" {
			t.Errorf("a revived job is %q, want pending", status)
		}
	}
	if status := statusOf(t, handler, waiting); status != "pending" {
		t.Errorf("the waiting job is %q, and the filter named only the dead ones", status)
	}
}

// The limit is required, and the message says what to add.
//
// A default would make the most dangerous request in this API the shortest
// one to write.
func TestABulkRequestWithNoLimitIsRefused(t *testing.T) {
	handler, _ := serve(t)

	for name, body := range map[string]string{
		"no limit at all": `{"status":"dead"}`,
		"a limit of zero": `{"status":"dead","limit":0}`,
		"a limit past the most one request may move": `{"status":"dead","limit":100000}`,
	} {
		got := withKey(t, handler, "POST", "/v1/jobs/cancel", body)
		if got.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400, body %s", name, got.Code, got.Body)
		}
		if !strings.Contains(got.Body.String(), "limit is required") {
			t.Errorf("%s: the answer does not say what to add: %s", name, got.Body)
		}
	}
}

// A bulk action is counted once for each job it moved.
//
// The counters answer how many jobs an operator stopped. A batch of four
// hundred counted as one would make the number useless the day it matters.
func TestABulkActionIsCountedForEachJob(t *testing.T) {
	handler, _ := serve(t)

	for i := 0; i < 3; i++ {
		submit(t, handler, `{"type":"work"}`)
	}
	withKey(t, handler, "POST", "/v1/jobs/cancel", `{"status":"pending","limit":100}`)

	page := metricsPage(t, handler)
	if !strings.Contains(page, `quorra_jobs_cancelled_total{caller="test"} 3`) {
		t.Errorf("the counter does not hold 3 cancellations: %s", page)
	}
}

// A bulk route needs write, and a bad status is named.
func TestABulkRequestWithABadStatusIsRefused(t *testing.T) {
	handler, _ := serve(t)

	got := withKey(t, handler, "POST", "/v1/jobs/cancel", `{"status":"finished","limit":10}`)
	if got.Code != http.StatusBadRequest {
		t.Fatalf("a bad status = %d, want 400, body %s", got.Code, got.Body)
	}
	if !strings.Contains(got.Body.String(), "finished") {
		t.Errorf("the answer does not name the status: %s", got.Body)
	}
}

// A job submitted to follow another is not handed out until it succeeds.
func TestAJobIsSubmittedToFollowAnotherOverHTTP(t *testing.T) {
	handler, backing := serve(t)
	ctx := t.Context()

	first := submit(t, handler, `{"type":"extract"}`)

	made := withKey(t, handler, "POST", "/v1/jobs",
		`{"type":"load","after":["`+first+`"]}`)
	if made.Code != http.StatusCreated {
		t.Fatalf("POST /v1/jobs = %d, body %s", made.Code, made.Body)
	}

	var created struct {
		ID     string   `json:"id"`
		Status string   `json:"status"`
		After  []string `json:"after"`
	}
	if err := json.Unmarshal(made.Body.Bytes(), &created); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if created.Status != "blocked" {
		t.Errorf("status = %q, want blocked", created.Status)
	}
	if len(created.After) != 1 || created.After[0] != first {
		t.Errorf("the job waits for %v, want the first job", created.After)
	}

	// Finish the first, and the second is ready.
	held, err := backing.Lease(ctx, store.LeaseRequest{
		Queue: "default", WorkerID: "w1", Limit: 1, TTL: time.Minute,
	})
	if err != nil || len(held) != 1 || held[0].ID != first {
		t.Fatalf("Lease: %v, %v", err, held)
	}
	if _, err := backing.Report(ctx, store.Report{
		JobID: first, LeaseID: held[0].LeaseID, Outcome: jobs.OutcomeDone,
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}

	if got := statusOf(t, handler, created.ID); got != "pending" {
		t.Errorf("status = %q after the parent succeeded, want pending", got)
	}
}

// A job that waits for one that is not there is 400 and not 404.
//
// The route exists, and the job the caller asked to create is not the thing
// that is missing.
func TestAJobThatFollowsAMissingJobIsRefused(t *testing.T) {
	handler, _ := serve(t)

	got := withKey(t, handler, "POST", "/v1/jobs",
		`{"type":"load","after":["8de1a3d0-0000-0000-0000-000000000000"]}`)
	if got.Code != http.StatusBadRequest {
		t.Fatalf("POST with a missing parent = %d, want 400, body %s", got.Code, got.Body)
	}
	if !strings.Contains(got.Body.String(), "8de1a3d0") {
		t.Errorf("the answer does not name the job that is missing: %s", got.Body)
	}
}

// A job that waits for nothing answers exactly what it answered before.
//
// Every caller that exists was written before this field, and an answer that
// grew a null field would break a client that refuses unknown keys.
func TestAJobThatFollowsNothingCarriesNoAfter(t *testing.T) {
	handler, _ := serve(t)

	made := withKey(t, handler, "POST", "/v1/jobs", `{"type":"work"}`)
	if strings.Contains(made.Body.String(), "after") {
		t.Errorf("a job that waits for nothing answers with an after field: %s", made.Body)
	}
}

// The queue says whether anything is out there.
//
// Every other question this API answers is about the jobs. A queue with a
// thousand waiting jobs and no worker looks exactly like a queue that is
// busy, and the second one is fine.
func TestTheWorkersAreListedOverHTTP(t *testing.T) {
	handler, backing := serve(t)
	ctx := t.Context()

	// Nothing has asked yet.
	empty := withKey(t, handler, "GET", "/v1/workers", "")
	if empty.Code != http.StatusOK {
		t.Fatalf("workers = %d, body %s", empty.Code, empty.Body)
	}
	if !strings.Contains(empty.Body.String(), `"workers":[]`) {
		t.Errorf("the answer is %s, want an empty list rather than null", empty.Body)
	}

	// An ask that finds nothing still counts, which is the whole point.
	if _, err := backing.Lease(ctx, store.LeaseRequest{
		Queue: "default", WorkerID: "idle-1", Limit: 1, TTL: time.Minute,
	}); err != nil {
		t.Fatalf("Lease: %v", err)
	}

	got := withKey(t, handler, "GET", "/v1/workers", "")
	var answer struct {
		Workers []struct {
			ID          string  `json:"id"`
			Queue       string  `json:"queue"`
			IdleSeconds float64 `json:"idle_seconds"`
		} `json:"workers"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if len(answer.Workers) != 1 {
		t.Fatalf("the answer holds %d workers: %s", len(answer.Workers), got.Body)
	}
	if answer.Workers[0].ID != "idle-1" || answer.Workers[0].Queue != "default" {
		t.Errorf("the worker is %+v", answer.Workers[0])
	}

	// How long it has been quiet is worked out on the server. A caller a
	// second out of step with this clock reads a fleet that is fine as one
	// that stopped.
	if answer.Workers[0].IdleSeconds != 0 {
		t.Errorf("idle seconds = %v, want 0 against the frozen clock", answer.Workers[0].IdleSeconds)
	}
}

// The history of a job is its own route.
//
// It is not a field on the job: the history of a job that retried all day is
// longer than the job, and every listing carries jobs.
func TestTheHistoryOfAJobIsReadOverHTTP(t *testing.T) {
	handler, backing := serve(t)
	ctx := t.Context()

	id := submit(t, handler, `{"type":"work"}`)

	// A job that has not run answers 200 with an empty list, and not 404.
	empty := withKey(t, handler, "GET", "/v1/jobs/"+id+"/attempts", "")
	if empty.Code != http.StatusOK {
		t.Fatalf("the attempts of a job that has not run = %d, body %s", empty.Code, empty.Body)
	}
	if !strings.Contains(empty.Body.String(), `"attempts":[]`) {
		t.Errorf("the answer is %s, want an empty list rather than null", empty.Body)
	}

	// Run it once and fail it, through the store, because the HTTP API has no
	// route that leases a job. That is the gRPC side.
	held, err := backing.Lease(ctx, store.LeaseRequest{Queue: "default", WorkerID: "w1", Limit: 1, TTL: time.Minute})
	if err != nil || len(held) != 1 {
		t.Fatalf("Lease: %v, %d jobs", err, len(held))
	}
	if _, err := backing.Report(ctx, store.Report{
		JobID: id, LeaseID: held[0].LeaseID, Outcome: jobs.OutcomeFailed, Error: "upstream said no",
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}

	got := withKey(t, handler, "GET", "/v1/jobs/"+id+"/attempts", "")
	if got.Code != http.StatusOK {
		t.Fatalf("attempts = %d, body %s", got.Code, got.Body)
	}

	var answer struct {
		Attempts []struct {
			Number  int    `json:"attempt"`
			Worker  string `json:"worker"`
			Outcome string `json:"outcome"`
			Error   string `json:"error"`
		} `json:"attempts"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if len(answer.Attempts) != 1 {
		t.Fatalf("the answer holds %d runs, want 1: %s", len(answer.Attempts), got.Body)
	}

	run := answer.Attempts[0]
	if run.Number != 1 || run.Worker != "w1" || run.Error != "upstream said no" {
		t.Errorf("the run is %+v", run)
	}

	// The outcome is its name and not its number, so a client does not have
	// to carry a copy of the order the constants are declared in.
	if run.Outcome != "failed" {
		t.Errorf("the outcome is %q, want failed", run.Outcome)
	}
}

// A job that is not there is 404, and a job that has not run is not.
func TestTheHistoryOfAMissingJobIsNotFound(t *testing.T) {
	handler, _ := serve(t)

	got := withKey(t, handler, "GET", "/v1/jobs/8de1a3d0-0000-0000-0000-000000000000/attempts", "")
	if got.Code != http.StatusNotFound {
		t.Errorf("the attempts of an unknown job = %d, want 404, body %s", got.Code, got.Body)
	}
}

// The name of the key that cancelled a job comes back on the job.
//
// Read from the answer and not from a log line, because this is what the
// dashboard and quorractl show, and what somebody asks the queue six months
// later when they want to know who stopped a run.
func TestACancelRecordsTheKeyThatAskedForIt(t *testing.T) {
	handler, _ := serve(t)

	id := submit(t, handler, `{"type":"work"}`)
	got := withKey(t, handler, "POST", "/v1/jobs/"+id+"/cancel", "")
	if got.Code != http.StatusOK {
		t.Fatalf("cancel = %d, body %s", got.Code, got.Body)
	}

	var job struct {
		ActedBy string `json:"acted_by"`
		ActedAt string `json:"acted_at"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &job); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if job.ActedBy != "test" {
		t.Errorf("acted by = %q, want test, the name of the key the harness holds", job.ActedBy)
	}
	if job.ActedAt == "" {
		t.Error("acted at is missing, so the name has no moment beside it")
	}
}

// A job nobody has acted on carries neither field.
//
// Both are omitempty, so a fresh job answers without them rather than with a
// name that is the empty string, which reads as a caller called nothing.
func TestAFreshJobCarriesNoAction(t *testing.T) {
	handler, _ := serve(t)

	id := submit(t, handler, `{"type":"work"}`)
	got := withKey(t, handler, "GET", "/v1/jobs/"+id, "")

	for _, field := range []string{"acted_by", "acted_at"} {
		if strings.Contains(got.Body.String(), field) {
			t.Errorf("a job nobody touched carries %s: %s", field, got.Body)
		}
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

func listed(t *testing.T, handler http.Handler, query string) (ids []string, next string) {
	t.Helper()

	got := withKey(t, handler, "GET", "/v1/jobs"+query, "")
	if got.Code != http.StatusOK {
		t.Fatalf("GET /v1/jobs%s = %d, body %s", query, got.Code, got.Body)
	}

	var answer struct {
		Jobs []struct {
			ID string `json:"id"`
		} `json:"jobs"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	for _, job := range answer.Jobs {
		ids = append(ids, job.ID)
	}
	return ids, answer.NextCursor
}

func TestTheListNarrowsByQueueStatusAndType(t *testing.T) {
	handler, _ := serve(t)

	wanted := submit(t, handler, `{"type":"email","queue":"mail"}`)
	submit(t, handler, `{"type":"email","queue":"other"}`)
	submit(t, handler, `{"type":"report","queue":"mail"}`)

	if got, _ := listed(t, handler, "?queue=mail"); len(got) != 2 {
		t.Errorf("queue=mail gave %d jobs, want 2", len(got))
	}
	if got, _ := listed(t, handler, "?type=email"); len(got) != 2 {
		t.Errorf("type=email gave %d jobs, want 2", len(got))
	}

	both, _ := listed(t, handler, "?queue=mail&type=email")
	if len(both) != 1 || both[0] != wanted {
		t.Errorf("the two filters together gave %v", both)
	}

	if got, _ := listed(t, handler, "?status=pending"); len(got) != 3 {
		t.Errorf("status=pending gave %d jobs, want 3", len(got))
	}
	if got, _ := listed(t, handler, "?status=dead"); len(got) != 0 {
		t.Errorf("status=dead gave %d jobs, want none", len(got))
	}
}

// due=now is resolved by the handler, from a clock the caller can state.
//
// The store is not allowed to read a clock, so this route reads it and passes
// a moment. Stating the moment is what lets this test ask what the queue
// looked like then, rather than arranging for the answer to be true at the
// instant it happens to run.
func TestTheListAnswersWhatIsReadyNow(t *testing.T) {
	handler, _ := serve(t)

	ready := submit(t, handler, `{"type":"now"}`)
	submit(t, handler, `{"type":"later","delay_seconds":3600}`)

	got, _ := listed(t, handler, "?due=now")
	if len(got) != 1 || got[0] != ready {
		t.Errorf("due=now gave %v, want only the job that is ready", got)
	}

	// And a stated moment reaches past the delay.
	future := frozen.Add(2 * time.Hour).Format(time.RFC3339)
	got, _ = listed(t, handler, "?due="+url.QueryEscape(future))
	if len(got) != 2 {
		t.Errorf("due=%s gave %d jobs, want both", future, len(got))
	}

	// Without the filter, every job.
	got, _ = listed(t, handler, "")
	if len(got) != 2 {
		t.Errorf("no filter gave %d jobs, want both", len(got))
	}
}

func TestTheListOrdersBySoonestWhenAsked(t *testing.T) {
	handler, _ := serve(t)

	// Submitted sooner first and later second, so the two orders disagree.
	// Submitting them the other way round makes both orders give the same
	// answer, and the test then passes without telling them apart.
	soon := submit(t, handler, `{"type":"soon","delay_seconds":60}`)
	late := submit(t, handler, `{"type":"late","delay_seconds":3600}`)

	got, _ := listed(t, handler, "?order=soonest")
	if len(got) != 2 || got[0] != soon || got[1] != late {
		t.Errorf("order=soonest gave %v, want the sooner one first", got)
	}

	// The default is unchanged, and here it is the reverse.
	got, _ = listed(t, handler, "")
	if len(got) != 2 || got[0] != late || got[1] != soon {
		t.Errorf("the default order gave %v, want the newest first", got)
	}
}

func TestTheListNarrowsToOneWorker(t *testing.T) {
	handler, backing := serve(t)

	submit(t, handler, `{"type":"a"}`)
	submit(t, handler, `{"type":"b"}`)

	held, err := backing.Lease(t.Context(), store.LeaseRequest{
		Queue: "default", WorkerID: "worker-7", Limit: 1, TTL: time.Minute,
	})
	if err != nil || len(held) != 1 {
		t.Fatalf("Lease: %v (%d jobs)", err, len(held))
	}

	got, _ := listed(t, handler, "?worker=worker-7")
	if len(got) != 1 || got[0] != held[0].ID {
		t.Errorf("worker=worker-7 gave %v, want the one job it holds", got)
	}
	if other, _ := listed(t, handler, "?worker=worker-8"); len(other) != 0 {
		t.Errorf("worker-8 holds %v, want nothing", other)
	}
}

// An order and a moment the server cannot read are refused, and each answer
// says what would have worked. A caller who guessed wrong once guesses wrong
// again without that.
func TestABadOrderOrMomentSaysWhatWouldWork(t *testing.T) {
	handler, _ := serve(t)

	got := withKey(t, handler, "GET", "/v1/jobs?order=oldest", "")
	if got.Code != http.StatusBadRequest {
		t.Fatalf("order=oldest = %d, want 400", got.Code)
	}
	for _, want := range []string{"newest", "soonest"} {
		if !strings.Contains(got.Body.String(), want) {
			t.Errorf("the answer does not name %q: %s", want, got.Body)
		}
	}

	got = withKey(t, handler, "GET", "/v1/jobs?due=tomorrow", "")
	if got.Code != http.StatusBadRequest {
		t.Fatalf("due=tomorrow = %d, want 400", got.Code)
	}
	for _, want := range []string{"now", "RFC 3339"} {
		if !strings.Contains(got.Body.String(), want) {
			t.Errorf("the answer does not name %q: %s", want, got.Body)
		}
	}
}

// A status the server does not know is refused, and the answer lists the ones
// it does. A caller who guessed wrong once guesses wrong again without them.
func TestABadStatusFilterNamesTheValidOnes(t *testing.T) {
	handler, _ := serve(t)

	got := withKey(t, handler, "GET", "/v1/jobs?status=processing", "")
	if got.Code != http.StatusBadRequest {
		t.Fatalf("status=processing = %d, want 400", got.Code)
	}
	for _, want := range []string{"pending", "leased", "succeeded", "dead", "cancelled"} {
		if !strings.Contains(got.Body.String(), want) {
			t.Errorf("the answer does not list %q: %s", want, got.Body)
		}
	}
}

func TestTheListPagesWithACursor(t *testing.T) {
	handler, _ := serve(t)

	const total = 7
	var made []string
	for i := 0; i < total; i++ {
		made = append(made, submit(t, handler, `{"type":"work"}`))
	}

	seen := map[string]int{}
	query := "?limit=3"
	for page := 0; page < total; page++ {
		got, next := listed(t, handler, query)
		if len(got) == 0 {
			break
		}
		for _, id := range got {
			seen[id]++
		}
		if next == "" {
			break
		}
		query = "?limit=3&before=" + next
	}

	for _, id := range made {
		if seen[id] != 1 {
			t.Errorf("%s was seen %d times, want once", id, seen[id])
		}
	}
}

// A short page is the end, so it carries no cursor. Handing one back there
// makes every caller ask once more to find that out.
func TestAShortPageCarriesNoCursor(t *testing.T) {
	handler, _ := serve(t)

	submit(t, handler, `{"type":"work"}`)
	submit(t, handler, `{"type":"work"}`)

	full, next := listed(t, handler, "?limit=2")
	if len(full) != 2 || next == "" {
		t.Errorf("a full page gave %d jobs and cursor %q, want a cursor", len(full), next)
	}

	short, next := listed(t, handler, "?limit=10")
	if len(short) != 2 {
		t.Fatalf("got %d jobs", len(short))
	}
	if next != "" {
		t.Errorf("a short page carried the cursor %q", next)
	}
}

// A stale cursor is 400 and not 404. The route exists, and answering 404 to a
// listing suggests it does not.
func TestAStaleCursorIsABadRequest(t *testing.T) {
	handler, _ := serve(t)
	submit(t, handler, `{"type":"work"}`)

	got := withKey(t, handler, "GET", "/v1/jobs?before=6f1c0c64-0000-0000-0000-000000000000", "")
	if got.Code != http.StatusBadRequest {
		t.Fatalf("a stale cursor = %d, want 400, body %s", got.Code, got.Body)
	}
	if !strings.Contains(got.Body.String(), "without it") {
		t.Errorf("the answer does not say what to do: %s", got.Body)
	}
}

// A repeated submission answers 200 and gives back the first job.
//
// Answering 201 to both would tell a client that it had just created
// something, which is the belief the key exists to correct.
func TestARepeatedSubmissionIsNotASecondJob(t *testing.T) {
	handler, _ := serve(t)

	body := `{"type":"charge","idempotency_key":"order-4471"}`

	first := withKey(t, handler, "POST", "/v1/jobs", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("the first submission = %d", first.Code)
	}

	second := withKey(t, handler, "POST", "/v1/jobs", body)
	if second.Code != http.StatusOK {
		t.Fatalf("the repeat = %d, want 200, body %s", second.Code, second.Body)
	}

	var one, two struct {
		ID string `json:"id"`
	}
	json.Unmarshal(first.Body.Bytes(), &one)
	json.Unmarshal(second.Body.Bytes(), &two)
	if one.ID != two.ID {
		t.Errorf("the repeat gave job %s, want the first at %s", two.ID, one.ID)
	}

	// And the Location header still points at the job, so a client following
	// it lands in the same place either way.
	if got := second.Header().Get("Location"); got != "/v1/jobs/"+one.ID {
		t.Errorf("Location = %q", got)
	}

	if all, _ := listed(t, handler, ""); len(all) != 1 {
		t.Errorf("the table holds %d jobs, want 1", len(all))
	}
}

// The header is the one a proxy or a client library sets, and it wins over
// the body, which belongs to the application.
func TestTheIdempotencyHeaderIsAccepted(t *testing.T) {
	handler, _ := serve(t)

	headers := map[string]string{
		"X-API-Key":       key,
		"Content-Type":    "application/json",
		"Idempotency-Key": "from-the-header",
	}

	first := call(t, handler, "POST", "/v1/jobs", `{"type":"charge"}`, headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("the first submission = %d, body %s", first.Code, first.Body)
	}

	second := call(t, handler, "POST", "/v1/jobs", `{"type":"charge"}`, headers)
	if second.Code != http.StatusOK {
		t.Errorf("the repeat = %d, want 200", second.Code)
	}

	// The header wins when both are set.
	third := call(t, handler, "POST", "/v1/jobs", `{"type":"charge","idempotency_key":"from-the-body"}`, headers)
	if third.Code != http.StatusOK {
		t.Errorf("the header did not win over the body: %d", third.Code)
	}
}

func TestJobsWithoutAKeyAreNeverMerged(t *testing.T) {
	handler, _ := serve(t)

	first := submit(t, handler, `{"type":"work"}`)
	second := submit(t, handler, `{"type":"work"}`)

	if first == second {
		t.Fatal("two jobs with no key became one")
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

// Every answer carries an identifier for the request that made it.
//
// A caller that says "it failed at 14:02" has nothing to quote without one,
// and a server writing two hundred lines a second at 14:02 has nothing to
// look for.
func TestEveryAnswerCarriesARequestIdentifier(t *testing.T) {
	handler, _ := serve(t)

	first := withKey(t, handler, "GET", "/v1/queues", "")
	second := withKey(t, handler, "GET", "/v1/queues", "")

	one := first.Header().Get(reqid.Header)
	two := second.Header().Get(reqid.Header)
	if one == "" {
		t.Fatal("an answer carries no identifier")
	}
	if one == two {
		t.Errorf("two requests share the identifier %q", one)
	}
}

// A request the guard refused carries one as well.
//
// That is the one a caller is most likely to ask about, which is why the
// identifier is given out before the guard runs and not after it.
//
// Over a real connection, and not through a recorder like every other test
// here. A recorder keeps a header set after the answer was written, so it
// cannot tell a header that reached the caller from one that did not, and the
// first version of this test passed against a build that set the header after
// the handler had already answered.
func TestARefusedRequestCarriesARequestIdentifier(t *testing.T) {
	handler, _ := serve(t)

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	request, err := http.NewRequest("GET", server.URL+"/v1/queues", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set("X-API-Key", "the-wrong-key-entirely-abcdef")

	answer, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("the request failed: %v", err)
	}
	defer func() { _ = answer.Body.Close() }()

	if answer.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the request answered %d, want 401", answer.StatusCode)
	}
	if answer.Header.Get(reqid.Header) == "" {
		t.Error("a refused request carries no identifier")
	}
}

// What a caller sent is what comes back, so both sides quote one string.
func TestWhatACallerSentComesBack(t *testing.T) {
	handler, _ := serve(t)

	answer := call(t, handler, "GET", "/v1/queues", "", map[string]string{
		"X-API-Key":  key,
		reqid.Header: "trace-abc-123",
	})

	if got := answer.Header().Get(reqid.Header); got != "trace-abc-123" {
		t.Errorf("the answer carries %q, and the caller sent trace-abc-123", got)
	}
}

// An identifier a caller made up is refused when it could write a log line of
// its own.
//
// A log line is a line. A value with a newline in it puts a line of the
// caller's choosing in the log of the server, saying whatever the caller
// likes.
func TestAnIdentifierThatCouldWriteALogLineIsRefused(t *testing.T) {
	handler, _ := serve(t)

	answer := call(t, handler, "GET", "/v1/queues", "", map[string]string{
		"X-API-Key":  key,
		reqid.Header: "ok\nlevel=ERROR\tmsg=the-database-is-gone",
	})

	got := answer.Header().Get(reqid.Header)
	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("the answer carries %q, which could write a line of its own", got)
	}
	if got == "" {
		t.Error("a request with an unusable identifier got none at all")
	}
}

// A refusal leaves a line naming the request it refused.
//
// Without it the identifier on a refusal finds nothing, and a caller told to
// quote an identifier has nothing to quote it against. That was found by
// reading a refusal out of quorractl and searching the log of the server for
// what it had printed.
func TestARefusalLeavesALineNamingTheRequest(t *testing.T) {
	handler, _, written := serveWithLog(t)

	answer := call(t, handler, "GET", "/v1/jobs/8f14e45f-ceea-467a-9c37-8e8f8f8f8f8f", "", map[string]string{
		"X-API-Key": key,
	})
	if answer.Code != http.StatusNotFound {
		t.Fatalf("the request answered %d, want 404", answer.Code)
	}

	id := answer.Header().Get(reqid.Header)
	if id == "" {
		t.Fatal("the refusal carries no identifier")
	}
	if !strings.Contains(written.String(), id) {
		t.Errorf("nothing in the log names request %s:\n%s", id, written.String())
	}
	if !strings.Contains(written.String(), "request refused") {
		t.Errorf("the refusal wrote no line:\n%s", written.String())
	}
}

// An answer that worked leaves no line.
//
// A queue answering normally is the normal case, and a line for each would
// bury the ones that matter.
func TestAnAnswerThatWorkedLeavesNoLine(t *testing.T) {
	handler, _, written := serveWithLog(t)

	answer := call(t, handler, "GET", "/v1/queues", "", map[string]string{"X-API-Key": key})
	if answer.Code != http.StatusOK {
		t.Fatalf("the request answered %d, want 200", answer.Code)
	}

	if strings.Contains(written.String(), "request refused") {
		t.Errorf("an answer that worked was logged as a refusal:\n%s", written.String())
	}
}
