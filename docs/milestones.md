<div align="center">
  <a href="../README.md"><b>GoQuorra</b></a>
</div>

# Milestones

This file holds what is not built, and the things that were looked at and
deliberately left.

Each entry that was left says what would have to change to revisit it.
That sentence is the point of the file: somebody arriving later should not
spend a day rediscovering a decision that was already made.

---

## What is built

The queue works end to end.
A job submitted over HTTP is leased by a worker over gRPC, run, and reported.
A worker that dies has its jobs taken back.
Two workers never receive the same job.

Measured, on a real server against PostgreSQL 16:

| Check | Result |
| ----- | ------ |
| Test cases | 128, of which 103 need nothing installed |
| A worker stopped with SIGKILL while holding a job | The lease was taken back ten seconds later, the row named the worker that died, and the counter moved from 0 to 1 |
| A job with `max_retries: 2` whose handler always fails | Ran three times, then `dead`, with the last error on the row |
| Eight goroutines leasing forty jobs at once | No job handed out twice, against PostgreSQL and against the in-memory store |
| Direct dependencies | Five |

---

## Not built

### Pushing a job to a worker

A worker asks, and the server answers with what is ready.
An idle worker asks once a second by default, so a job submitted into an empty
queue waits up to a second before it starts.

The previous version of this project declared `LeaseJobs` as a server stream,
which looked like a push and was not: it sent whatever was ready and closed.
Making it a real push needs the server to hold a stream open for every
connected worker and to learn when a job arrives.

Two ways to learn:

- **Ask the database.** One query for each connected worker, on a short timer.
  This is the current polling, moved from the worker to the server, and it
  costs more rather than less.
- **Be told.** PostgreSQL has `LISTEN` and `NOTIFY`, and a trigger on insert
  can fire one. That is a real design. It needs a connection held open per
  server, a fallback for the notifications that are dropped when a listener is
  slow, and care that a job becoming ready after a backoff also notifies,
  which no insert trigger sees.

**What would change the answer.** A measured need for latency below a second.
Nothing in this project has one today, and the polling interval is a setting.

### Authenticating a worker

The gRPC port has no authentication.
A process that can reach it can lease from any queue.
It cannot submit work, and it cannot report on a job it does not hold, because
a report carries a lease identifier and the server refuses one that does not
match.

The shape of the fix is known: mutual TLS, or a token in the call metadata
checked by an interceptor.
The work is not the check.
It is the key distribution, the rotation, and a story for a worker whose
certificate expires while it holds jobs.

**What would change the answer.** A deployment where the workers are not
inside a network the operator controls.

### A repeat schedule

A job runs once, at or after a time.
There is no repeat rule and no calendar.

A cron style schedule is a different object from a job: it has to survive a
missed window, decide whether to catch up or skip, and hold a time zone,
because "every day at nine" moves twice a year and a queue that stores UTC
alone gets it wrong both times.

**What would change the answer.** Somebody wanting it enough to specify the
catch up behaviour, which is the part everybody forgets and then argues about.

### Removing finished jobs

A `succeeded` row stays for ever.
On a busy queue the table grows without limit, and the index that finds ready
jobs stays small only because it is partial.

Deleting is easy and deciding is not: a finished job is the only record that
the work happened, and somebody always wants it.

**What would change the answer.** A retention setting, and an answer to where
the row goes before it is deleted.

---

## Deliberately left

Both were looked at, and both were left.
The decision is made.
What is written down is the reasoning.

### Exactly once delivery

GoQuorra delivers at least once.
A worker can finish the work and lose power before it reports, and the job is
then given to another worker.

This is not a gap that effort closes.
The window is between the side effect and the acknowledgement, and no protocol
removes it.
The systems that advertise exactly once move it: they either require the side
effect to be a write to the same database as the acknowledgement, so that one
transaction covers both, or they require the consumer to deduplicate, which is
the same idempotency the caller was trying to avoid.

The honest position is at least once, said plainly, with the advice to write
handlers that can run twice.

**What would change the answer.** Nothing about this queue. A caller whose
side effect is a write to the same PostgreSQL database can already have
effectively once, by doing the work and reporting in one transaction, and that
needs an API this project does not have: a way for a handler to be handed the
transaction. That is a real feature and it is worth building. It is not the
same thing as exactly once, and it must not be described as though it were.

### An idempotency key on submission

A client that retries a submission because it did not see the answer creates a
second job.
A unique key supplied by the client, with a unique index, would remove that.

It was considered and left out of the rebuild, because it is the smaller half
of the problem.
It removes duplicates a client creates, and it does nothing about duplicates
the queue creates when a lease is taken back.
A handler still has to be safe to run twice, and once it is, the key buys much
less.

**What would change the answer.** It is cheap: one nullable column, one
partial unique index, one field on the create request. It is worth adding the
first time somebody submits from a network they do not trust. It should not be
sold as a step towards exactly once, because it is not one.

---

## Recorded so nobody investigates them twice

### Why the state machine has four states and not six

`processing` and `failed` were both documented and neither was ever written by
any code.

`processing` cannot be observed by the server.
The worker holds the job between the lease and the report, and the server
hears nothing in that window.
A status the server cannot set is a status that lies.

`failed` was never distinguishable from `pending`.
A job that fails and has attempts left goes back into the queue, which is what
pending means.

A test refuses both by name, so that somebody reading old documentation cannot
quietly put one back.

### Why no query calls NOW()

Every time in this project comes from `Options.Now` and travels as a
parameter.

The previous schema carried a trigger that set `updated_at` to `NOW()` on
every update, while the program also set that column.
The trigger overwrote what the program had written, with a time from the
database clock rather than the server clock.
Two clocks decided one column.

Passing the time costs one field on a struct and buys two things: the two
clocks cannot disagree, and a test moves time forward instead of waiting.
The contract suite tests a lease expiry in microseconds because of it.

### Why the backoff doubles in a loop rather than shifting

Shifting by the attempt count is the obvious way to double, and it turns
negative at 63.
A negative wait puts `run_at` in the past, so the job runs at once, fails, and
comes straight back, and the queue spins on that one job for as long as the
process lives.

A test drives attempt counts of 62, 63, 64, 1000 and 1048576.

### Why half of every backoff is jitter

Without it, a database that goes away for a minute sends every job that failed
in that minute back at the same instant.
The retry storm then takes the database down a second time.

The jitter is a parameter rather than a draw inside the function, so the tests
state the wait they expect instead of accepting a range.

### Why the reclaim test does not use the worker package

The worker package gives a handler a context that ends when the lease does.
A handler that respects its context therefore returns at that moment, and the
worker reports the failure itself.

That is the right behaviour, and it means the SDK never produces the state the
reclaimer exists for.
The test uses a plain gRPC client to lease a job and then abandon it, because
that is what a process losing power looks like from the server.

The first version of that test used the SDK and asserted on the reason
recorded against the job. It failed, and the failure was correct: the reason
said "context deadline exceeded", which is the worker reporting, not the
reclaimer acting.
