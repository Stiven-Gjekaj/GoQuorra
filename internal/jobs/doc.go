// Package jobs holds the rules that a job follows, and nothing else.
//
// Nothing in this package opens a database, reads the clock, or draws a random
// number. Every function takes what it needs and returns an answer. That is
// the only reason a table test can drive every state a job reaches with no
// PostgreSQL running and no container started, and it is why this package is
// the first thing to read.
//
// The storage layer applies what these functions decide. It decides nothing
// itself. Keeping the decision in one pure place means the two paths that
// retire a job, a worker that reports a failure and a lease that runs out,
// age that job in exactly the same way. When they lived apart, one of them
// did not age it at all.
package jobs
