package api_test

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/api"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/memory"
)

// The dashboard carries no API key.
//
// The old page had the key printed into its source, so anybody who could open
// the dashboard could read the key that guarded the whole API.
func TestTheDashboardCarriesNoKey(t *testing.T) {
	handler, _ := serve(t)

	got := call(t, handler, "GET", "/", "", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("GET / = %d", got.Code)
	}
	if strings.Contains(got.Body.String(), key) {
		t.Error("the API key is printed in the dashboard source")
	}
	if strings.Contains(got.Body.String(), "api_key=") {
		t.Error("the dashboard puts a key in a query string")
	}
}

// The page must never build a row out of strings.
//
// This reads the source rather than the rendered page, because the fault it
// guards against happens in the browser and a Go test cannot run the
// JavaScript. It is still worth having: the old dashboard joined strings and
// assigned the result straight into the page as markup, and two of the values
// in each row, the job type and the queue name, are chosen by whoever submits
// a job. Anybody who could reach the submission endpoint could put a script
// into this page and have it run in the reader's browser, with their key.
//
// The forbidden calls are spelled with a separator below so that this comment
// and this list are the only places the words appear, and the check cannot
// trip over the text that explains it.
//
// Every value is written with textContent instead. A test that refuses the
// dangerous call by name is what stops that coming back on a tired afternoon.
func TestTheDashboardBuildsNoHTMLFromData(t *testing.T) {
	source, err := os.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("cannot read the dashboard: %v", err)
	}

	forbidden := []string{"inner" + "HTML", "outer" + "HTML", "insertAdjacent" + "HTML", "document." + "write", "eval("}
	for _, call := range forbidden {
		if strings.Contains(string(source), call) {
			t.Errorf("the dashboard uses %s, which turns a job type into markup", call)
		}
	}

	// And the safe call really is being used, so that the check above cannot
	// pass by the page having stopped rendering anything at all.
	if !strings.Contains(string(source), "textContent") {
		t.Error("the dashboard sets no textContent, so it is not rendering the data safely")
	}
}

// The policy names this page's own script and nothing else.
//
// It is the second line of defence. The page builds every row with
// textContent, so there is nothing for a policy to stop today. It is here for
// the day somebody adds one careless innerHTML.
func TestTheDashboardCarriesAPolicy(t *testing.T) {
	handler, _ := serve(t)

	got := call(t, handler, "GET", "/", "", nil)
	policy := got.Header().Get("Content-Security-Policy")
	if policy == "" {
		t.Fatal("the dashboard carries no Content-Security-Policy")
	}

	if strings.Contains(policy, "unsafe-inline") || strings.Contains(policy, "unsafe-eval") {
		t.Errorf("the policy allows what it exists to forbid: %s", policy)
	}
	for _, want := range []string{"default-src 'none'", "script-src 'nonce-", "frame-ancestors 'none'"} {
		if !strings.Contains(policy, want) {
			t.Errorf("the policy does not hold %q: %s", want, policy)
		}
	}
}

// A nonce that repeats is not a nonce.
//
// Reusing one lets a page injected from elsewhere carry a script tag with the
// value that worked last time, which is the whole thing the nonce prevents.
func TestTheNonceChangesOnEveryRequest(t *testing.T) {
	handler, _ := serve(t)

	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		policy := call(t, handler, "GET", "/", "", nil).Header().Get("Content-Security-Policy")
		if seen[policy] {
			t.Fatalf("the same nonce was served twice: %s", policy)
		}
		seen[policy] = true
	}
}

// The JSON that feeds the page escapes the characters that open a tag.
//
// This is measured rather than assumed. Go's encoder replaces < > and & with
// escapes by default, so a job type holding a script tag arrives at the
// browser as text even before the page decides what to do with it. Knowing
// that is what makes the textContent rule the second line rather than the
// only one.
func TestAJobTypeHoldingMarkupComesBackEscaped(t *testing.T) {
	handler, _ := serve(t)

	made := withKey(t, handler, "POST", "/v1/jobs",
		`{"type":"<script>alert(1)</script>","queue":"<img src=x onerror=alert(2)>"}`)
	if made.Code != http.StatusCreated {
		t.Fatalf("POST = %d, body %s", made.Code, made.Body)
	}

	listed := withKey(t, handler, "GET", "/v1/jobs", "").Body.String()
	if strings.Contains(listed, "<script>") || strings.Contains(listed, "<img") {
		t.Errorf("the answer holds unescaped markup: %s", listed)
	}
	if !strings.Contains(listed, `\u003cscript\u003e`) {
		t.Errorf("the markup was not escaped the way this test expects: %s", listed)
	}
}

// The mark is served, and it is the file in the repository.
//
// Comparing the bytes rather than checking for a 200 is what says the route
// serves this project's mark and not an empty response with the right
// content type.
func TestTheLogoIsServed(t *testing.T) {
	handler, _ := serve(t)

	got := call(t, handler, "GET", "/logo.svg", "", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("GET /logo.svg = %d", got.Code)
	}
	if want := "image/svg+xml; charset=utf-8"; got.Header().Get("Content-Type") != want {
		t.Errorf("content type = %q, want %q", got.Header().Get("Content-Type"), want)
	}

	onDisk, err := os.ReadFile("logo.svg")
	if err != nil {
		t.Fatalf("cannot read the logo: %v", err)
	}
	if len(onDisk) == 0 {
		t.Fatal("logo.svg is empty")
	}
	if got.Body.String() != string(onDisk) {
		t.Error("the served mark is not the file in the repository")
	}
}

// The policy must allow the page to load its own mark.
//
// This is the test worth having. default-src 'none' blocks an image as
// readily as a script, so a policy written without img-src leaves a broken
// picture in the corner of the page and a console message that nobody
// watching a queue would ever think to look for. Nothing fails, nothing is
// logged on the server, and the only symptom is a gap.
func TestThePolicyAllowsThePageToLoadItsOwnMark(t *testing.T) {
	handler, _ := serve(t)

	page := call(t, handler, "GET", "/", "", nil)
	policy := page.Header().Get("Content-Security-Policy")

	if !strings.Contains(policy, "img-src 'self'") {
		t.Fatalf("the policy blocks the page's own mark: %s", policy)
	}

	// And the page really does ask for it, so the directive above is not
	// permission for something nothing uses.
	source, err := os.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("cannot read the dashboard: %v", err)
	}
	if !strings.Contains(string(source), `src="/logo.svg"`) {
		t.Error("the dashboard does not show the mark")
	}
	if !strings.Contains(string(source), `href="/logo.svg"`) {
		t.Error("the dashboard sets no icon for the browser tab")
	}
}

// A server with the dashboard turned off serves nothing it does not use.
func TestTheLogoGoesWithTheDashboard(t *testing.T) {
	backing := memory.New(store.Options{})
	t.Cleanup(func() { _ = backing.Close() })

	handler := api.New(api.Options{
		Store:            backing,
		Metrics:          metrics.New(),
		Log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		APIKey:           key,
		DashboardEnabled: false,
	}).Handler()

	for _, path := range []string{"/", "/dashboard", "/logo.svg"} {
		if got := call(t, handler, "GET", path, "", nil); got.Code != http.StatusNotFound {
			t.Errorf("GET %s with the dashboard off = %d, want 404", path, got.Code)
		}
	}
}

// The page must carry no inline style attribute.
//
// The policy names a nonce for style, and a nonce does not apply to an
// attribute, so the browser refuses an inline style and the rule simply does
// not happen. Nothing fails and nothing is logged on the server. The first
// version of this page laid its header out with one, and the control it was
// meant to push to the right edge sat in the middle of the row instead, which
// looked like a choice.
func TestTheDashboardStylesNothingInline(t *testing.T) {
	source, err := os.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("cannot read the dashboard: %v", err)
	}

	if strings.Contains(string(source), "style=\"") {
		t.Error("the dashboard uses a style attribute, which the policy on this page refuses")
	}
}

// The filter row offers every status the server can hold.
//
// Built from a list rather than from whatever is in the table at the moment,
// so a status with no jobs in it is still a button somebody can press. A
// filter row that only shows what is already visible is no use for finding
// the thing that is not.
func TestTheDashboardOffersEveryStatus(t *testing.T) {
	source, err := os.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("cannot read the dashboard: %v", err)
	}

	for _, status := range []string{"pending", "leased", "succeeded", "dead", "cancelled"} {
		if !strings.Contains(string(source), `"`+status+`"`) {
			t.Errorf("the dashboard does not offer the %q filter", status)
		}
	}
}

// The dashboard acts on a job through the same routes everything else uses.
func TestTheDashboardUsesTheRealVerbs(t *testing.T) {
	source, err := os.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("cannot read the dashboard: %v", err)
	}

	if !strings.Contains(string(source), `method: "POST"`) {
		t.Error("the dashboard has no way to act on a job")
	}
	if !strings.Contains(string(source), "/v1/jobs/") {
		t.Error("the dashboard does not call the job routes")
	}
	// Encoded, because a job identifier reaches this from the server and a
	// path built by joining strings is a path somebody can escape.
	if !strings.Contains(string(source), "encodeURIComponent") {
		t.Error("the dashboard builds a path without encoding what it puts in it")
	}
}

// A column that shows a result is not headed with a word that means a fault.
//
// The result of a succeeded job hangs off the same cell as the error of a
// failed one, because a job has one or the other and never both. That is
// worth the saved column, but only while the heading covers both. The
// heading said Last error, so a job that finished correctly was listed with
// the word "result" underneath it, which reads as the name of what went
// wrong.
//
// Nothing reports this. The server answers 200, the page draws, and the row
// is merely wrong.
func TestTheDashboardDoesNotCallAResultAnError(t *testing.T) {
	source, err := os.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("cannot read the dashboard: %v", err)
	}
	page := string(source)

	// The cell carries a result when there is no error to show.
	if !strings.Contains(page, `last.textContent = "result"`) {
		t.Skip("the dashboard no longer puts a result in that cell, so there is nothing to head")
	}

	if strings.Contains(page, "<th>Last error</th>") {
		t.Error("the dashboard writes a result into a column headed Last error")
	}
	if !strings.Contains(page, "<th>Outcome</th>") {
		t.Error("the dashboard has no Outcome column for the error or the result")
	}
}

// The dashboard offers a way to ask what is ready, and it is not a status.
//
// ready answers a different question from the five statuses, so it builds a
// different query: what the queue would hand out now, in the order it would
// hand it out. A list of ready jobs in submission order does not say what
// runs next, which is the only reason to ask.
func TestTheDashboardCanAskWhatIsReady(t *testing.T) {
	source, err := os.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("cannot read the dashboard: %v", err)
	}
	page := string(source)

	if !strings.Contains(page, `"ready"`) {
		t.Error("the dashboard does not offer the ready filter")
	}
	if !strings.Contains(page, "due=now") {
		t.Error("the ready filter does not ask the server what is due now")
	}
	if !strings.Contains(page, "order=soonest") {
		t.Error("the ready filter does not ask for the order the queue works in")
	}

	// Pending as well as due. A job that has stopped keeps the run_at of its
	// last attempt, so asking only what is due lists every job that has ever
	// run, which is the opposite of what the button says.
	if !strings.Contains(page, "status=pending&due=now") {
		t.Error("the ready filter asks what is due without asking for pending, so it lists finished jobs")
	}

	// ready must not be one of the statuses. The filter row builds the
	// buttons from that list, and a value in it would be sent as
	// status=ready, which the server refuses.
	if strings.Contains(page, `const STATUSES = ["pending", "leased", "succeeded", "dead", "cancelled", "ready"]`) {
		t.Error("ready was added to the statuses, and the server refuses status=ready")
	}
}

// Every row says when its job runs.
//
// A pending job with a run_at two hours out is identical to one that is ready
// this second in every other column, so without this the page cannot answer
// the question the ready filter exists for.
func TestTheDashboardShowsWhenAJobRuns(t *testing.T) {
	source, err := os.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("cannot read the dashboard: %v", err)
	}
	page := string(source)

	if !strings.Contains(page, "<th>Runs at</th>") {
		t.Error("the table has no column for when a job runs")
	}
	if !strings.Contains(page, "job.run_at") {
		t.Error("nothing reads run_at, so the column would be empty")
	}

	// The column count and the empty-state row have to agree, or the empty
	// message stops spanning the table and the layout breaks in a way that
	// only shows when there are no jobs.
	columns := strings.Count(page[strings.Index(page, "<thead>"):strings.Index(page, "</thead>")], "<th>")
	if !strings.Contains(page, "td.colSpan = "+strconv.Itoa(columns)) {
		t.Errorf("the table has %d columns and the empty row spans a different number", columns)
	}
}

// A job that failed and then succeeded shows what it produced, not the
// failure before it.
//
// last_error is kept when a job succeeds, on purpose: the record that an
// attempt failed is worth having. The Outcome cell used to prefer it, so a
// job that failed once and then worked displayed its old error and hid its
// result. Nothing reports this. The server answers 200, the page draws, and
// the row says the wrong thing.
func TestTheOutcomeCellFollowsTheStatusAndNotTheFields(t *testing.T) {
	source, err := os.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("cannot read the dashboard: %v", err)
	}
	page := string(source)

	// The old shape, which reads last_error first and only falls back to the
	// result when there is no error at all.
	if strings.Contains(page, `cell(row, job.last_error || "")`) {
		t.Error("the outcome cell prefers last_error, so a job that failed and then succeeded hides its result")
	}
	if strings.Contains(page, "!job.last_error && job.result") {
		t.Error("the result is shown only when there is no error, which is the same fault")
	}

	// What decides is the status.
	if !strings.Contains(page, `job.status === "succeeded"`) {
		t.Error("the outcome cell does not read the status, so it cannot tell the two cases apart")
	}
}
