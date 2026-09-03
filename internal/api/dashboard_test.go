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
	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
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
		Keys:             testKeys(t, key),
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

	// From jobs.All and not from a list written here. A list written here
	// goes stale the same way the page does, and then the test agrees with
	// the fault rather than catching it. blocked was added and this rule did
	// not see it, which is exactly what that costs.
	for _, status := range jobs.All() {
		if !strings.Contains(string(source), `"`+status.String()+`"`) {
			t.Errorf("the dashboard does not offer the %q filter", status)
		}
	}

	// And a waiting job says what it waits for. The word blocked on its own
	// is the question and not the answer.
	if !strings.Contains(string(source), "job.after") {
		t.Error("a waiting row does not say which jobs it waits for")
	}
}

// The page stops asking when nobody is watching, and slows down when the
// server is not answering.
//
// docs/milestones.md recorded three faults here and left all three: a wrong
// key made two failing requests every five seconds for as long as the tab was
// open, a tab left in the background polled until it was closed, and a server
// that had gone away was asked again at the same rate for ever.
func TestTheDashboardHasARequestLifecycle(t *testing.T) {
	source, err := os.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("cannot read the dashboard: %v", err)
	}
	page := string(source)

	// The shape that had none of the three.
	if strings.Contains(page, "setInterval(") {
		t.Error("the page polls on a fixed interval, which has no backoff and no pause")
	}

	for what, marker := range map[string]string{
		"a backoff":                 "CEILING",
		"a stop after failures":     "GIVE_UP",
		"a pause when hidden":       "document.hidden",
		"a restart when it is seen": "visibilitychange",
	} {
		if !strings.Contains(page, marker) {
			t.Errorf("the page has no %s", what)
		}
	}

	// The backoff has to grow and be bounded. One without a ceiling leaves a
	// page an hour behind after a long outage.
	if !strings.Contains(page, "Math.min(wait * 2, CEILING)") {
		t.Error("the wait does not double up to a ceiling")
	}

	// Correcting the key clears the backoff. A reader who has just fixed the
	// reason for the failures should not wait out the punishment for them.
	if !strings.Contains(page, "restart()") {
		t.Error("nothing clears the backoff, so a corrected key waits out the wait")
	}
}

// One job can be opened, with its payload and what it did.
//
// The payload is the one field a listing can never show, and it is the thing
// a person opening a job came for.
func TestTheDashboardOpensOneJob(t *testing.T) {
	source, err := os.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("cannot read the dashboard: %v", err)
	}
	page := string(source)

	if !strings.Contains(page, "/attempts") {
		t.Error("the panel never asks what the job did")
	}
	if !strings.Contains(page, "job.payload") {
		t.Error("the panel never shows the payload, which is what it exists for")
	}

	// The payload comes from whoever submitted the job, so it is the value on
	// this page most worth being careful with. textContent and never
	// innerHTML.
	if strings.Contains(page, "innerHTML") {
		t.Error("the page assigns markup, and the payload is chosen by whoever submits a job")
	}

	// A job identifier must not reach the address bar. It would be kept in
	// browser history and sent on in the Referer of anything the page fetched
	// after it.
	for _, forbidden := range []string{"history.pushState", "location.hash ="} {
		if strings.Contains(page, forbidden) {
			t.Errorf("the page puts state in the address with %s", forbidden)
		}
	}
}

// A job can be found by its identifier.
//
// Paging to a job among thousands is not finding it, and an operator holding
// an identifier from a log line has no other way in.
func TestTheDashboardFindsAJobByIdentifier(t *testing.T) {
	source, err := os.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("cannot read the dashboard: %v", err)
	}
	page := string(source)

	if !strings.Contains(page, `id="findform"`) {
		t.Error("the page has no way to look a job up")
	}
	if !strings.Contains(page, "openJob(") {
		t.Error("nothing opens a job, so the search box would go nowhere")
	}
}

// The page can be read past the first twenty five rows.
//
// A table hard capped at twenty five made finding one job among a thousand
// impossible from the page, and the note under it told the reader to go and
// use a different tool.
func TestTheDashboardReadsPastTheFirstPage(t *testing.T) {
	source, err := os.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("cannot read the dashboard: %v", err)
	}
	page := string(source)

	// It follows the cursor the server hands back, and does not ask for a
	// bigger limit instead. A reader who pressed for more twenty times would
	// otherwise ask the server for five hundred rows in one query.
	if !strings.Contains(page, "next_cursor") {
		t.Error("the page never reads the cursor, so it cannot ask for a second page")
	}
	if !strings.Contains(page, "&before=") {
		t.Error("the page never sends a cursor back, so every page it asks for is the first")
	}

	// The old note, which sent the reader to a different tool.
	if strings.Contains(page, "quorractl list -all to see the rest") {
		t.Error("the page still tells the reader to go and use another tool")
	}
}

// Every job that has not finished can be cancelled from the page.
//
// Decided from what is finished rather than from a list of what can be
// cancelled. The list was there first and went stale on the first state added
// after it: a waiting job could be cancelled by every other route and had no
// button here. Found by loading the page, not by a test.
func TestEveryUnfinishedJobCanBeCancelledFromThePage(t *testing.T) {
	source, err := os.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("cannot read the dashboard: %v", err)
	}
	page := string(source)

	// The list of states the page treats as finished has to be exactly the
	// ones the domain calls terminal.
	for _, status := range jobs.All() {
		listed := strings.Contains(page, `const FINISHED = [`) &&
			strings.Contains(finishedList(page), `"`+status.String()+`"`)
		if listed != status.Terminal() {
			t.Errorf("the page lists %q as finished: %v, and the domain says %v",
				status, listed, status.Terminal())
		}
	}

	// And the button is not decided by naming the states that can be
	// cancelled, which is the shape that went stale.
	if strings.Contains(page, `job.status === "pending" || job.status === "leased"`) {
		t.Error("the cancel button is decided from a list of states, which goes stale")
	}
}

// finishedList gives the text of the FINISHED array in the page.
func finishedList(page string) string {
	start := strings.Index(page, "const FINISHED = [")
	if start < 0 {
		return ""
	}
	end := strings.Index(page[start:], "]")
	if end < 0 {
		return ""
	}
	return page[start : start+end]
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
	// only shows when there is nothing to list.
	//
	// Every table on the page, and not the first one. This read the first
	// <thead> it found, which was the jobs table until the schedules table
	// was put above it, and then it measured one table against the other.
	for _, table := range []string{"jobs", "schedules"} {
		columns := headingsOf(t, page, table)
		if !strings.Contains(page, "colSpan = "+strconv.Itoa(columns)) {
			t.Errorf("the %s table has %d columns and no empty row spans that many", table, columns)
		}
	}
}

// headingsOf counts the columns of the table whose body carries an id.
func headingsOf(t *testing.T, page, id string) int {
	t.Helper()

	body := strings.Index(page, `<tbody id="`+id+`">`)
	if body < 0 {
		t.Fatalf("there is no table bodied %q", id)
	}

	head := strings.LastIndex(page[:body], "<thead>")
	shut := strings.LastIndex(page[:body], "</thead>")
	if head < 0 || shut < head {
		t.Fatalf("the table bodied %q has no heading row", id)
	}
	return strings.Count(page[head:shut], "<th>")
}

// A cancelled job says who cancelled it.
//
// On the status cell and not in a column of its own: only cancel and revive
// record a name, so a column would be empty on almost every row, and this
// table is already nine wide.
func TestTheStatusCellCarriesTheCallerThatActed(t *testing.T) {
	source, err := os.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("cannot read the dashboard: %v", err)
	}
	page := string(source)

	if !strings.Contains(page, "job.acted_by") {
		t.Error("the page never reads acted_by, so a cancelled job does not say who stopped it")
	}
	if !strings.Contains(page, "job.acted_at") {
		t.Error("the page never reads acted_at, so the name has no moment beside it")
	}

	// A tenth column would be the other way to do this, and it is the one
	// that was not chosen.
	if strings.Contains(page, `"ACTED BY"`) || strings.Contains(page, "<th>Acted by</th>") {
		t.Error("the page adds a column that is empty on almost every row")
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

// The page says which key it is holding.
//
// A key limited to queues shows an empty listing for a queue it cannot reach,
// and that reads exactly like an empty queue. The reader has to be able to
// see which caller the page is.
func TestTheDashboardNamesTheKeyInUse(t *testing.T) {
	source, err := os.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("cannot read the dashboard: %v", err)
	}
	page := string(source)

	if !strings.Contains(page, "/v1/whoami") {
		t.Error("the dashboard never asks who it is")
	}
	if !strings.Contains(page, `id="who"`) {
		t.Error("the dashboard has nowhere to say who it is")
	}

	// Asked again when the key changes, and not on every refresh. A page
	// that asked every five seconds would put a request on an answer that
	// changes only when somebody types a different key.
	if !strings.Contains(page, "askedAbout") {
		t.Error("the dashboard asks who it is on every refresh")
	}
}

// The page shows the workers the queue has heard from.
//
// A queue filling up because no worker has asked for work looks exactly like
// a queue filling up because the work is slow. GET /v1/workers answers that,
// and the page could not ask it.
func TestTheDashboardShowsTheWorkers(t *testing.T) {
	source, err := os.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("cannot read the dashboard: %v", err)
	}
	page := string(source)

	if !strings.Contains(page, "/v1/workers") {
		t.Error("the dashboard never asks which workers are out there")
	}
	if !strings.Contains(page, `id="workers"`) {
		t.Error("the dashboard has nowhere to show the workers")
	}

	// Grouped by worker. One worker asking about two queues is one process,
	// and a reader counting rows would otherwise count it twice.
	if !strings.Contains(page, "byWorker") {
		t.Error("the workers are not grouped by worker")
	}

	// A worker nobody has heard from is the thing the list is for, so it is
	// marked rather than left to be read.
	if !strings.Contains(page, "idle_seconds") {
		t.Error("the page does not say how long ago a worker asked")
	}
}

// The page shows the repeat schedules, and can switch one.
//
// A schedule that is switched off produces nothing, and from the jobs table
// that looks exactly like a schedule that is working and has nothing due.
func TestTheDashboardShowsTheSchedules(t *testing.T) {
	source, err := os.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("cannot read the dashboard: %v", err)
	}
	page := string(source)

	if !strings.Contains(page, "/v1/schedules") {
		t.Error("the dashboard never asks for the schedules")
	}
	if !strings.Contains(page, `id="schedules"`) {
		t.Error("the dashboard has nowhere to show the schedules")
	}

	// Both verbs, because a switch that only goes one way is half a switch.
	for _, verb := range []string{"enable", "disable"} {
		if !strings.Contains(page, `"/`+verb) && !strings.Contains(page, `"`+verb+`"`) {
			t.Errorf("the dashboard cannot %s a schedule", verb)
		}
	}

	// The next firing comes from the server. A page that worked it out in a
	// browser would answer in whatever zone the reader's machine is set to.
	if !strings.Contains(page, "next_firing_at") {
		t.Error("the page does not say when a schedule fires next")
	}
	if strings.Contains(page, "ParseCron") || strings.Contains(page, "cronNext") {
		t.Error("the page works out the next firing itself")
	}
}

// The page can act on everything the filter names.
//
// Clearing a dead letter queue was a shell loop, because the only route to
// the bulk actions was quorractl. The arc that built them said the point was
// an operator who does not need a terminal.
func TestTheDashboardCanActOnAWholeFilter(t *testing.T) {
	source, err := os.ReadFile("dashboard.html")
	if err != nil {
		t.Fatalf("cannot read the dashboard: %v", err)
	}
	page := string(source)

	for _, route := range []string{"/v1/jobs/\" + verb", "renderBulk"} {
		if !strings.Contains(page, route) {
			t.Errorf("the dashboard cannot act on a whole filter: %q is missing", route)
		}
	}

	// Only when a status is chosen. The filter that names every job is the
	// one nobody means to act on, and cancelling all of them must not be one
	// press away from a page somebody opened to look at it.
	//
	// What is checked here is that no action is named for the two filters
	// that are not a status. A first version looked for the lookup itself,
	// and passed against a build that had added a fallback to it, because
	// the line it searched for was still there with more after it. Whether
	// the button appears is behaviour, and the page is loaded in a browser
	// at the end of this arc to see it.
	table := page[strings.Index(page, "const BULK = {"):]
	table = table[:strings.Index(table, "};")]
	for _, notAStatus := range []string{`"":`, "ready:", `"ready":`} {
		if strings.Contains(table, notAStatus) {
			t.Errorf("the bulk action is named for %q, which is not a status", notAStatus)
		}
	}
	if !strings.Contains(page, "if (!action") {
		t.Error("the bulk action is offered when the filter names none")
	}

	// A finished status offers revive and an unfinished one offers cancel.
	// Offering both would offer one that answers an error whatever is
	// pressed.
	for status, want := range map[string]string{
		"pending": "cancel", "leased": "cancel", "blocked": "cancel",
		"dead": "revive", "cancelled": "revive",
	} {
		at := strings.Index(page, status+": [\"")
		if at < 0 {
			t.Errorf("no bulk action is named for %q", status)
			continue
		}
		if !strings.HasPrefix(page[at:], status+`: ["`+want+`"`) {
			t.Errorf("%q does not offer %q", status, want)
		}
	}

	// Bounded by what is on the page, so nothing moves that the reader has
	// not seen, and asked about before it runs.
	if !strings.Contains(page, "limit: shown") {
		t.Error("the bulk action is not bounded by what the page shows")
	}
	if !strings.Contains(page, "window.confirm") {
		t.Error("the bulk action runs without asking")
	}
}
