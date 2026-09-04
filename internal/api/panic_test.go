package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
)

// A handler that panics answers 500 in the shape every other answer has.
//
// The recover exists so that one bad request does not end the process and
// take every other request in flight with it. Nothing proved that it answers,
// and it wrote a JSON body through http.Error, which labels the answer
// text/plain. A client decoding every answer could not read the one answer it
// is least able to guess at.
//
// This test is inside the package because the panic has to come from a
// handler, and no route here panics on purpose.
func TestAHandlerThatPanicsAnswersInTheUsualShape(t *testing.T) {
	a := New(Options{
		Metrics: metrics.New(),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:     time.Now,
	})

	handler := a.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("the handler fell over")
	}))

	got := httptest.NewRecorder()
	handler.ServeHTTP(got, httptest.NewRequest("GET", "/v1/jobs", nil))

	if got.Code != http.StatusInternalServerError {
		t.Fatalf("a handler that panicked answered %d, want 500", got.Code)
	}
	if kind := got.Header().Get("Content-Type"); kind != "application/json; charset=utf-8" {
		t.Errorf("the answer carries the content type %q", kind)
	}

	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer %q is not JSON: %v", got.Body.String(), err)
	}
	if body.Error == "" {
		t.Errorf("the answer carries no reason: %s", got.Body.String())
	}

	// The panic itself must not carry into the answer. It is written to the
	// log, where the reader is somebody who runs this, and not to a caller
	// who may be anybody.
	if body.Error == "the handler fell over" {
		t.Error("the answer repeats what the handler panicked with")
	}
}
