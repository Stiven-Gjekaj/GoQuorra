// Package storetest holds the rules that every store must obey.
//
// One suite runs against every implementation. The in-memory store needs
// nothing installed, so the suite runs on any machine and in every CI job.
// The PostgreSQL store runs against a real database. Testing both against one
// suite is the point: a memory store with its own private suite becomes a
// convenient fiction that agrees with no database anybody deploys.
//
// The suite drives the clock by hand. Nothing here sleeps, so a lease expiry
// is tested in microseconds rather than in the thirty seconds it takes in
// production, and the result does not depend on how busy the machine is.
package storetest

import (
	"sync"
	"time"
)

// Clock is a clock that a test moves.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock starts a clock at a stated time.
func NewClock(at time.Time) *Clock { return &Clock{now: at} }

// Now reads the clock. The store is handed this method.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
