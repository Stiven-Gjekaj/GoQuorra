package jobs

import (
	"testing"
	"time"
)

// The measurement behind the month skip in Next.
//
// It is a benchmark and not a test, because the thing it protects is a cost
// and not an answer. A test that asserted a duration would fail on a busy
// machine and teach everybody to ignore it.
//
// Run it before removing that line:
//
//	go test -run XXX -bench BenchmarkNext ./internal/jobs/
//
// With the skip: about 7.9us and 41us. Without it: about 19.6ms and 49ms.
func BenchmarkNextOnAScheduleTwoYearsAway(b *testing.B) {
	c, err := ParseCron("0 0 29 2 *")
	if err != nil {
		b.Fatalf("ParseCron: %v", err)
	}
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, found := c.Next(at); !found {
			b.Fatal("the twenty ninth of February was not found")
		}
	}
}

// The worst case: a schedule that never fires, so the whole limit is walked.
func BenchmarkNextOnAScheduleThatNeverFires(b *testing.B) {
	c, err := ParseCron("0 0 30 2 *")
	if err != nil {
		b.Fatalf("ParseCron: %v", err)
	}
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, found := c.Next(at); found {
			b.Fatal("the thirtieth of February was found")
		}
	}
}
