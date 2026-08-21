package memory_test

import (
	"testing"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/memory"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/storetest"
)

// The memory store answers to the same suite as the PostgreSQL one.
//
// This is the whole reason the suite is a package rather than a file. A memory
// store with its own private tests agrees only with itself, and then it stands
// in for a database it does not behave like.
func TestMemoryStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T, opts store.Options) store.Store {
		return memory.New(opts)
	})
}

// A returned job must share nothing with the stored one.
//
// Handing back the stored value gives the caller a pointer into the map. An
// HTTP handler that then edits the payload it received changes what the store
// believes, and nothing anywhere reports it.
func TestAReturnedJobSharesNothingWithTheStore(t *testing.T) {
	s := memory.New(store.Options{})
	defer s.Close()

	made, err := s.Create(t.Context(), store.NewJob{Type: "work", Payload: []byte(`{"n":1}`)})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	made.Payload[2] = 'X'
	made.Type = "changed"
	made.Priority = 99

	again, err := s.Get(t.Context(), made.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(again.Payload) != `{"n":1}` {
		t.Errorf("the stored payload changed to %s when the caller edited its copy", again.Payload)
	}
	if again.Type != "work" || again.Priority != 0 {
		t.Errorf("the stored job changed to %+v when the caller edited its copy", again)
	}
}
