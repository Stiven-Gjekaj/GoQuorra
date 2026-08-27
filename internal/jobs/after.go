package jobs

// AfterState says what a job waiting for other jobs should be.
//
// The whole rule for waiting lives here, in one function with no clock, no
// database handle and no knowledge of how the jobs are stored. Both stores
// call it on the two paths that need it: when the job is submitted, and when
// one of the jobs it waits for stops moving.
//
// Writing it in SQL instead was the alternative. It would have been two
// statements that had to agree, in two stores, and the version before this
// project's rebuild is a record of what happens to a rule kept in two places.
func AfterState(parents []Status) Status {
	// A job that waits for nothing is ready. This is the common case, and it
	// is the answer that keeps every existing caller working: a submission
	// that names no parent gets exactly what it got before.
	if len(parents) == 0 {
		return Pending
	}

	blocked := false
	for _, parent := range parents {
		switch {
		case parent == Succeeded:
			// This one is done. Keep looking.
		case parent.Terminal():
			// Dead or cancelled. This job will never run, and the answer does
			// not depend on the others: one parent that cannot succeed is
			// enough, and looking at the rest would only delay the same
			// answer.
			return Cancelled
		default:
			// Waiting, running, or itself waiting for something. Not an
			// answer yet, because a later parent may be dead, and a job that
			// can never run should not sit blocked until somebody notices.
			blocked = true
		}
	}

	if blocked {
		return Blocked
	}
	return Pending
}
