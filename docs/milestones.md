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
| Test cases | 529, of which 415 need nothing installed |
| Store contract rules | 108, and both stores pass all of them |
| A worker stopped with SIGKILL while holding a job | The lease was taken back ten seconds later, the row named the worker that died, and the counter moved from 0 to 1 |
| A job with `max_retries: 2` whose handler always fails | Ran three times, then `dead`, with the last error on the row |
| Eight goroutines leasing forty jobs at once | No job handed out twice, against PostgreSQL and against the in-memory store |
| Eight submissions at once under one idempotency key | Exactly one stored a job, and all eight were given the same one |
| A handler running for two and a half times its lease | Finished once, because the heartbeat held the lease. With the heartbeat off it ran twice. |
| A handler refusing a job, against one that only fails, both with `max_retries: 3` | The refused one was buried on attempt 1 and the failing one on attempt 4. `quorra_jobs_refused_total` moved to 1 inside a `quorra_jobs_dead_total` of 2 |
| Forty jobs over five `run_at` moments, paged seven at a time in the soonest order | Six pages, forty rows, no repeat and nothing missing |
| A handler that wrote a row and then failed once, five jobs of each kind | Ten rows through an ordinary handler and five through a `worker/pgtx` one |
| Sixty three job types against a label bounded at fifty | Fifty one rows, `quorra_job_types_tracked` at 50, `other` holding 13, and a sum over the label of 90 against 68 dead and 22 succeeded |
| The worker protocol, timed for the first time | 610 `Lease` calls at OK and 25 at `Unauthenticated`, 90 `Report` calls of which 88 answered inside 5ms |
| An identifier `quorractl` printed for a job that was not there | The same string found the line the server wrote, which it did not before |
| Direct dependencies | Five, unchanged from the first commit to this one |

---

## Not built

### Pushing a job to a worker, built on 27 August 2026

A worker asked, and the server answered with what was ready.
An idle worker asked once a second by default, so a job submitted into an
empty queue waited up to a second before it started.

The gate this entry set was a measured need for latency below a second.
That is not what happened: it was asked for, and this line records that so the
file keeps meaning what it says.

This entry named two ways to learn that a job arrived and picked the second.
That is what was built, and the three things it said the second one needs are
all three of them here.

**A connection held open per server.** The store hijacks one out of the pool
and `LISTEN`s on it. Hijacked and not borrowed: a `LISTEN` outlives the
statement that issued it, so a connection released back would put a listening
one into the pool and hand the next ordinary query the notifications meant for
a watcher. A contract rule caught that, seeing two hints on a channel nothing
had notified.

**A fallback for the notifications that are dropped.** Every worker still
polls, and waits on whichever comes first. That is what makes a lost hint cost
latency and nothing else, and it is why nothing anywhere treats a missing one
as a fault.

**The case this entry warned about**, a job becoming ready after a backoff,
which no insert trigger sees. It is not covered, and it does not need to be: a
job waiting out a backoff is deliberately not urgent, and the poll finds it.
The hint goes out where a job becomes ready *now*, and that is written down
rather than left to be discovered.

The notify is sent from Go and not from a trigger.
A trigger would put the rule in a second place that has to agree with the
first, and it would fire on an insert rather than on a job becoming ready,
which are not the same event.
Every hint rides the transaction that produced it, so it is delivered on
commit: a listener told about a job that was then rolled back would go and
find nothing.

Measured against a real server and PostgreSQL, submitted to leased, over ten
jobs with a five second poll: a median of 11ms, a minimum of 9 and a maximum
of 15.
The same measurement before this, with a five hundred millisecond poll, was a
median of 207ms.
With a five second poll and no push the average wait is half the interval by
arithmetic, which is two and a half seconds, and that is arithmetic rather
than a measurement.

**What is not built.** A hint for a job that becomes ready later. It needs a
timer per job or a query per tick, and both are the first option this entry
rejected: the polling moved from the worker to the server, costing more rather
than less.

### Authenticating a worker, built on 27 August 2026

The gRPC port had no authentication.
A process that could reach it could lease from any queue.

This entry said the shape of the fix was known and that the work was not the
check: it was the key distribution and the rotation.
That was right, and it is why this waited for the named keys.
A deployment that has a key distribution story for its HTTP keys has one for
its worker keys, because they are the same keys.

The gate this entry set was a deployment where the workers are not inside a
network the operator controls.
That was not what happened.
It was built because it was asked for, and this line records that so the file
keeps meaning what it says.

A token in the call metadata, checked by an interceptor on both the unary and
the streaming path.
A worker key holds the `worker` scope and nothing else, which is why the
permissions stopped being an ordered number: leasing work off the queue is not
more than changing a job, and a key an operator keeps in a shell profile must
not be able to lease the queue empty.

Mutual TLS was the other shape and is not built.
It carries the part this entry named as the real work, a story for a worker
whose certificate expires while it holds jobs, and a key in the metadata does
not: a worker that is refused stops leasing and keeps reporting on what it
holds until its own key is replaced.

Measured against a running server: a worker with no key does not start, and a
worker holding a `write` key is refused with
`PermissionDenied: the key "ops" may write, and the worker protocol needs
worker`.

### A repeat schedule, built on 27 August 2026

A job ran once, at or after a time.
There was no repeat rule and no calendar.

The gate this entry set was somebody wanting it enough to specify the catch up
behaviour.
That is what happened: it was asked for, and the behaviour was written down
before any code was, which is the order this entry asked for.

A schedule is a different object from a job.
It holds a rule, a zone and a catch up policy, it produces jobs, and it is
never handed to a worker.

**The catch up policy**, which is the part this entry named.
The case is a server down from Friday to Monday and a schedule that should
have fired seventy two times.
There is no answer that is right for every schedule, so there is no default
and the field is required:

| Policy | Does |
| ------ | ---- |
| `skip` | Fires once, at the most recent missed window, and forgets the rest. What almost every schedule means: a report that runs every hour does not want seventy two reports. |
| `all` | Fires every missed window, oldest first, bounded at a hundred. For a schedule where each firing does different work, keyed on the window. |
| `none` | Fires nothing until the next scheduled moment. For a schedule where a late firing is worse than a missed one. |

Measured against a real server and PostgreSQL, with an outage of seventy two
hours set on three hourly schedules: `all` produced 72 jobs, `skip` produced 1
and logged 71 dropped, and `none` produced 0 and logged 72 dropped.

The bound is real too.
A ten day outage, which is 240 windows, produced the hundred jobs the bound
allows and reported 140 dropped.
Enqueueing nine thousand jobs because somebody restored an old backup is not
recovery.

**The time zone** is an IANA name and never an offset, because an offset is
what the zone was on the day it was written down.
Measured: `0 3 * * *` in `Europe/Berlin` fires at 01:00 UTC in August and the
same rule in `UTC` fires at 03:00, which is the two hours a queue storing UTC
alone gets wrong.

**The cron parser is written here** rather than taken from a library.
This project has five direct dependencies and a rule about adding a sixth, and
the standard library has no cron parser at all.
Five fields and not six, because a queue that hands a job to a worker over a
network cannot honour a seconds field.

**What is not built.** A calendar with holidays, and a rule that says "the
last working day of the month".
Both need a source of truth about days that this project does not have and
should not invent.

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
needed an API this project did not have: a way for a handler to be handed the
transaction. This entry called that a real feature worth building.

Built on 2026-08-27, as `worker/pgtx`, because it was asked for. A handler
there is given a `pgx.Tx`, and the outcome of the job is recorded in that same
transaction, so the handler's writes and the record of the job commit together
or neither does.

**The queue still delivers at least once, and the package says so at the top
of its own documentation.** The window is still there for a handler that calls
another service, writes to another database, or sends an email. What the
package does is make that window empty for one case, by turning two writes
into one write. It is not exactly once and it must not be described as though
it were.

Measured against a live server. Five jobs whose handler wrote its row and then
failed once wrote ten rows through an ordinary handler, one for the failed
attempt and one for the successful one, and five rows through a transaction
one. Over fifty jobs that succeeded first time, an attempt took a median of
2.70ms through the transaction against 3.51ms through an ordinary report,
which is the gRPC round trip the transaction path does not make. Speed is not
the reason to use it.

The advice for every other handler is unchanged: write it so that running
twice is safe.

### An idempotency key on submission

Built. It was left out of the rebuild and the entry that recorded that said it
was cheap and worth adding the first time somebody submitted from a network
they did not trust. It is one nullable column, one partial unique index and
one field, exactly as estimated.

The warning in that entry still stands and is worth repeating here, because
the feature now exists and can be misread. **It is not a step towards exactly
once.** It removes the duplicates a client creates and none of the ones the
queue creates when a lease is taken back. A handler still has to be safe to
run twice.

---

## Recorded so nobody investigates them twice

### The list of migrations has a limit, and here it is

Every file in `migrations/` is applied in name order, and each is written to
be safe to apply twice. There is no table recording what has run.

This entry said that works while every change is additive, and named the first
change that is not as the trigger. That wording was looser than the scheme
needs, and `migrations/0002_cancel.sql` already showed it: that file drops a
check constraint and writes it again, which is not additive, and it is fine
because both statements are safe to apply twice.

The property the scheme depends on is repeatability, not additivity. So the
real trigger is a change that cannot be made repeatable: a column dropped
while live code still reads it, or a backfill that does something different
the second time. There are ten files now, six of them added after this entry
was written, and none of them reaches that line: they add tables and columns
and touch no existing value.

**What would change the answer.** The first change that cannot be written to
be safe to apply twice. Do not reach for a tool before then: the list is three
functions and a test, and a migration framework is a dependency, a state table
and a failure mode.

### A cursor on a time alone is wrong, and passes a careless test

Paging in the soonest order compares the pair `(run_at, seq)` and not `run_at`
alone.
`run_at` is not unique.
A burst of submissions shares one value, and every job a reclaim sweep returns
shares one.
A cursor on it alone either repeats that whole group on the next page or skips
the rest of it.

The trap is in the test and not in the code.
Jobs created one after another each get their own `run_at`, so a paging test
written the obvious way passes against the broken cursor.
The rule in the contract suite does not move the clock, so nine jobs share one
moment, and it fails against a `run_at` cursor in both stores.

It is written as a row comparison and not as the `OR` that means the same
thing, and that is measured rather than preferred.
PostgreSQL turns the row form into an index condition on `jobs_due_idx` and
seeks.
It cannot do that with the `OR`: on 200000 rows the `OR` form read 955 rows to
return 25 while the row form read 25.

**What would change the answer.** A planner that stops using the row form.
Check with `EXPLAIN` before rewriting it, because the spelled out form looks
like the safer one and is not.

### A comment can be the wrong side of a disagreement

`worker.Job.LeaseExpiresAt` was documented as the moment the handler's context
ends.
Nothing set a deadline anywhere in that package, so the code and the comment
disagreed, and the obvious fix is the wrong one.

The heartbeat pushes the lease out while a handler runs, so the moment the
handler was handed goes stale almost at once.
A deadline at it stops work that is being kept alive on purpose.
Writing that deadline fails both the test for it and
`TestASlowHandlerKeepsItsJob`, which is the test the heartbeat exists for.

The comment was corrected, not the code.
When a comment and the code disagree, the comment is not automatically the one
telling the truth about what should happen.

### The server refuses a lease under a second

`rpc.DefaultLimits` sets a minimum of one second, and a worker asking for less
gets a second anyway.

This is recorded because it silently defeated a test. The first version of the
slow handler test asked for a 300ms lease and ran a 900ms handler, expecting
the reclaimer to take the job away without a heartbeat. The lease it actually
held was one second, so the handler finished inside it and the test passed
with the feature turned off.

Any test about a lease running out has to use numbers above the minimum.

### command in Kubernetes and command in Compose are different things

`command:` in a Kubernetes manifest replaces the image's entrypoint.
`command:` in a Compose file replaces its CMD.

The image holds three binaries, so the caller has to choose one. With an
ENTRYPOINT set, the Compose services asking for the worker ran the server with
the path of the worker as an argument, and the server ignored the argument and
started. The stack came up with no workers at all, jobs were accepted, and
nothing ever leased them.

The image uses CMD alone now, so both orchestrators select the same way, and
both binaries refuse an argument they do not understand so the same mistake
stops the container instead of starting the wrong program.

### A nonce does not apply to a style attribute

The dashboard carries a content policy naming a nonce for script and for
style. A nonce applies to a `<style>` element and not to a `style=` attribute,
so an inline style is refused by the browser and simply does not happen.

The header of the page laid itself out with one, and the control it was meant
to push to the right edge sat in the middle of the row instead, which read as
a choice. Nothing failed, nothing reached the server log, and the only sign
was a line in a console nobody had open.

A test refuses a style attribute anywhere in the page. The same applies to any
inline event handler, which is why every listener there is added in script.

### The flag package stops at the first argument

`quorractl get 6f1c0c64 -server http://elsewhere` used to read the address as
a second job identifier, because Go's flag package stops parsing at the first
token that is not an option. That is documented behaviour and a surprise to
everybody, because every other tool on the machine takes them in either order.

The tool moves the options in front before parsing, asking the flag set
whether each one swallows the token after it. It writes a `--` between the two
halves, always: the first version dropped a separator the caller had typed,
and the argument it was protecting was then read as an option.

### The dashboard polled for ever, and now it does not

The page reloaded every five seconds on a fixed interval with no backoff, no
check for whether the tab was visible, and no stop when the key was wrong.
A wrong key made two failing requests every five seconds for as long as the
tab was open, and a tab left in the background polled until it was closed.

This entry said it was real and left, and named what fixing it would take: a
backoff, a pause when hidden, and a stop after a run of failures.
It now has all three.
The wait doubles on a failure to a ceiling of a minute, the page stops after
six failures in a row, and a hidden tab is asked for nothing.

Measured over the browser.
With a wrong key: one request in the first second and a half, and two in
twenty one and a half, with the wait at twenty seconds.
Hidden for thirteen seconds: no requests at all, and two the moment the tab
was seen again.
Correcting the key put the wait back to five seconds and refreshed at once,
because somebody who has just fixed the reason for the failures should not
serve the punishment for them.

**What was needed to change the answer.** Nothing measured.
The request was made, and the entry that recorded the cost is what made it
easy to say yes to.

### Why `processing` and `failed` are not states

This entry was headed "why the state machine has four states and not six",
which was true when it was written and is not now: `cancelled` and `blocked`
arrived afterwards and there are six. The count was never the point, and a
heading that carries one goes stale every time a state is added. The reasoning
is about two particular names.

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
