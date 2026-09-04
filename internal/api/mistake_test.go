package api

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
)

// A refusal that answers to the store's sentinel is the caller's mistake.
//
// This test is inside the package because the question is about one function
// and not about a route. Asking it over HTTP would need a store that refuses
// something, and then the rule would be about that store's wording again,
// which is the thing being removed.
//
// The error below carries no "store: " prefix on purpose. That is what makes
// the test fail against the version of this function that reads the message.
func TestASentinelRefusalIsTheCallersMistake(t *testing.T) {
	if !isClientMistake(fmt.Errorf("the priority is too large: %w", store.ErrInvalid)) {
		t.Error("an error answering to ErrInvalid does not read as the caller's mistake")
	}
}

// An error from underneath is not the caller's mistake.
//
// Without this the test passes against a function that says yes to
// everything, and everything the database refuses would answer 400.
func TestAnErrorFromUnderneathIsNotTheCallersMistake(t *testing.T) {
	if isClientMistake(errors.New("connection refused")) {
		t.Error("an error from underneath reads as the caller's mistake")
	}
}
