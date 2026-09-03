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
// With the skip: about 7.5us and 24.5us. Without it: about 15.4ms and 37.4ms.
//
// "Without it" means the month is still tested and the walk steps a minute at
// a time. Deleting the block instead stops the month column being tested at
// all, and both benchmarks then answer with a date in March, quickly and
// wrongly.
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
