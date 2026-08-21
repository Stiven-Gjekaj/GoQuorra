package api_test

import (
	"net/http"
	"os"
	"strings"
	"testing"
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
