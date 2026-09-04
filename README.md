<div align="center">

<img src="https://raw.githubusercontent.com/Stiven-Gjekaj/GoQuorra/main/internal/api/logo.svg" alt="GoQuorra" width="112">

### A job queue for Go, with the parts that usually go missing

_A worker that dies does not take its job with it_

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.24-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Go 1.24"/>
  <img src="https://img.shields.io/badge/PostgreSQL-16-4169E1?style=for-the-badge&logo=postgresql&logoColor=white" alt="PostgreSQL 16"/>
  <img src="https://img.shields.io/badge/Prometheus-metrics-E6522C?style=for-the-badge&logo=prometheus&logoColor=white" alt="Prometheus metrics"/>
</p>

<p align="center">
  <a href="https://github.com/Stiven-Gjekaj/GoQuorra/actions/workflows/ci.yml?query=branch%3Amain"><img src="https://img.shields.io/github/actions/workflow/status/Stiven-Gjekaj/GoQuorra/ci.yml?branch=main&label=ci&style=flat-square" alt="CI on main"/></a>
  <img src="https://img.shields.io/badge/license-MIT-green?style=flat-square" alt="MIT License"/>
</p>

<p align="center">
  <a href="#quick-start"><b>Quick start</b></a> |
  <a href="#how-it-works"><b>How it works</b></a> |
  <a href="#writing-a-worker"><b>Writing a worker</b></a> |
  <a href="#the-http-api"><b>API</b></a> |
  <a href="#project-structure"><b>Structure</b></a>
</p>

</div>

---

## Overview

GoQuorra runs background work.
A client submits a job over HTTP.
A worker leases it over gRPC, does it, and says what happened.
PostgreSQL holds the job the whole time.

Most of a job queue is easy.
The parts that are not are the ones that decide whether it can be trusted:
what happens when a worker dies holding a job, whether two workers can be
given the same job, what a retry limit counts, and whether the numbers on the
dashboard are real.

This project is written around those four.

---

## What it does not do

These limits are decisions.
Meeting one should read as a decision and not as something unfinished.

**Delivery is at least once, and never exactly once.**
A worker can finish the work and lose power before it reports.
The job is then given to another worker and the work happens twice.
No queue avoids this.
The ones that claim to are moving the same window somewhere else, usually into
a transaction that your own code has to join.
Write handlers that can run twice.
[docs/milestones.md](docs/milestones.md) records what would narrow the window
and what it would cost.

**A job is not pushed to a worker.**
A worker asks, and the server answers with what is ready.
An idle worker asks once a second by default, so a job submitted into an empty
queue waits up to that long before it starts.
A stream that stays open and pushes a job the moment it arrives is a real
design and it is not this one.

**There is no scheduling language.**
A job runs once, at or after a time you name.
There is no repeat rule and no calendar.

**Nothing is removed unless you ask.**
A queue that deletes its own history by default takes the only record that a
piece of work happened.
Retention is off until somebody sets it, so the table grows until then.

**Nothing here encrypts a payload.**
A payload sits in the database as JSON that anybody with a database connection
can read.

---

## Quick start

**With nothing installed at all.**
The server can keep its jobs in memory, which is the fastest way to see the
whole thing work.

```
go run ./cmd/quorra-server
```

with these set:

```
export QUORRA_STORE=memory
export QUORRA_API_KEY="$(openssl rand -hex 32)"
```

Then in a second terminal:

```
export QUORRA_API_KEY=<the same key>
go run ./cmd/quorra-worker
```

And in a third:

```
export QUORRA_API_KEY=<the same key>
go run ./cmd/quorractl create -type echo -payload '{"hello":"world"}'
go run ./cmd/quorractl list
```

Nothing in memory mode survives a restart, and the server says so at startup.

**With the whole stack.**

```
make dev
```

That builds the image and starts PostgreSQL, a server, and two workers.
The dashboard is at `http://localhost:8080`.
It asks for the API key, which the compose file sets to
`local-development-key-not-for-anywhere-else`.

**Against your own PostgreSQL.**

```
export DATABASE_URL="postgres://quorra:quorra@localhost:5432/quorra?sslmode=disable"
make db-init
export QUORRA_API_KEY="$(openssl rand -hex 32)"
make build
./bin/quorra-server
```

There is no default API key.
The server refuses to start without one.

`QUORRA_API_KEY` is the short form and means one key named `default` that may
write, which is why `quorractl whoami` answers `default (may write)` above.
Set `QUORRA_API_KEYS` instead to give each caller its own name and scope. See
[The HTTP API](#the-http-api).

---

## How it works

### The path a job takes

```
a client submits a job over HTTP
  -> the row is written with status=pending and a time it may run
  -> a worker asks its queue for work over gRPC
  -> the server leases it: status=leased, a lease identifier, an expiry,
     and one more attempt counted
  -> the worker runs the handler and reports
  -> success ends the job; a failure sends it back with a wait, or buries it
```

### The states

```mermaid
stateDiagram-v2
    [*] --> pending: submitted
    [*] --> blocked: submitted after another job
    blocked --> pending: every job it waits for succeeded
    blocked --> cancelled: a job it waits for cannot succeed
    pending --> leased: a worker takes it
    leased --> succeeded: the worker reports success
    leased --> pending: the worker reports a failure, and attempts remain
    leased --> dead: the worker reports a failure, and none remain
    leased --> pending: the lease runs out, and attempts remain
    leased --> dead: the lease runs out, and none remain
    blocked --> cancelled: a person stops it
    pending --> cancelled: a person stops it
    leased --> cancelled: a person stops it
    dead --> pending: a person revives it
    cancelled --> pending: a person revives it
    cancelled --> blocked: a person revives it, and it still waits
    succeeded --> [*]
    dead --> [*]
    cancelled --> [*]
```

There are six states.
Two of an earlier six went, and two have arrived since, and both halves are
worth keeping: a state removed for a reason still holds that reason against
being added back.

`processing` is gone because the server cannot observe it: the worker holds
the job between the lease and the report, and the server hears nothing in that
window.
`failed` is gone because it was never distinguishable from `pending`.
Both were documented, and no code ever wrote either.

| State | Meaning |
| ----- | ------- |
| `blocked` | Waiting for another job. Not `pending`, because `pending` is a claim that the queue will hand the job out. |
| `pending` | Waiting for a worker. Ready once `run_at` has passed. A delayed job and a job serving a backoff both sit here. |
| `leased` | A worker holds it. The lease carries an expiry. |
| `succeeded` | A worker reported that it is done. |
| `dead` | Every attempt was used. The row stays, with the last error on it. |
| `cancelled` | A person stopped it, or a job it waits for cannot succeed. Separate from `dead`, because the difference between them is the only thing that says whether the queue gave up or somebody decided. |

`blocked` was added rather than holding a waiting job back with a `run_at` far
in the future.
That needs no new state and lies in a worse way: the job then says it runs in
the year nine thousand, which is what `order=soonest` and the ready filter
would show.

### A job does not wait out a poll

A worker asks for work, and the server also tells it when there may be some.
The worker waits on whichever comes first.

Measured against PostgreSQL, submitted to leased, over ten jobs with a five
second poll: a median of 11ms, a minimum of 9 and a maximum of 15.
With a five second poll and nothing telling the worker, the average wait is
half the interval.

The poll is what makes this correct and the hint is what makes it quick.
A hint that is lost costs one poll interval, so a worker whose stream never
connects behaves exactly as it did before this existed.
Nothing anywhere treats a missing hint as a fault.

A hint goes out where a job becomes ready **now**: a submission with no delay,
a revive, a job released by the job it was waiting for, and a schedule firing.
A job that becomes ready **later**, which is one with a delay or one waiting
out a backoff, sends none and is found by the poll.
That is deliberate: a job serving a backoff is not urgent, and waking every
worker for it would cost more than the wait.

The hint carries a queue name and never a job.
Handing a job down that stream would make it a second way to lease one, with
its own rules about leases and its own way to go wrong.

### Two workers never get the same job

The lease is one statement.
It selects the ready rows `FOR UPDATE SKIP LOCKED`, marks them, and returns
them.
A row another worker has locked is stepped over rather than waited for, so
two workers asking at the same instant take different jobs and neither
blocks.

A test in the store contract suite runs eight goroutines over forty jobs and
fails if any job is handed out twice.
It runs against PostgreSQL and against the in-memory store.

### A worker that dies does not take its job with it

Every lease carries an expiry.
A loop in the server looks for leases that have passed it and returns those
jobs to the queue, through the same decision a reported failure goes through,
so a crash and a clean failure age a job identically.

Measured on a real server, with a worker stopped by `SIGKILL` while it held a
job and a lease of six seconds:

| Moment | The row said |
| ------ | ------------ |
| While the worker held it | `leased`, 1 attempt, `leased_by=doomed-worker` |
| Ten seconds after the kill | `pending`, 1 attempt, no lease, `last_error` naming the worker that died |

`quorra_leases_reclaimed_total` moved from 0 to 1, and the server logged
`took back expired leases count=1`.

This is the part the previous version did not have.
A job leased by a process that then lost power stayed leased for as long as
the table lived.

### What a retry limit counts

`max_retries` counts retries after the first attempt.
A job with `max_retries: 3` runs four times.

The previous version compared the attempt count against that number, so a
caller who asked for three retries received two.
Every single step of it looked correct, which is why the test counts the runs
of a whole life instead.

The wait doubles from `QUORRA_RETRY_BASE` and stops at `QUORRA_RETRY_MAX`.
Half of each wait is jitter.
Without the jitter, a database that goes away for a minute sends every job
that failed in that minute back at the same instant, and the retry storm takes
the database down a second time.

### A handler can refuse a retry

Some failures do not go away.
A payload that names no account does not name one on the third attempt, and an
upstream with no record of an identifier will not grow one while the job
waits.

A handler says so by wrapping `worker.ErrPermanent`:

```go
w.Handle("charge", func(ctx context.Context, job worker.Job) error {
	account, err := accounts.Find(ctx, id)
	if errors.Is(err, accounts.ErrNoSuchAccount) {
		return worker.Permanent(fmt.Errorf("no account %q: %w", id, err))
	}
	if err != nil {
		return err // A timeout. Try again.
	}
	return charge(ctx, account)
})
```

The job goes to the dead letter queue on that attempt, whatever its retry
count, with the reason on the row.
It can be revived like any other dead job once the thing that was wrong is
fixed.

Use it when the job is the problem.
Do not use it for a timeout, a refused connection or a rate limit, which are
the failures retrying exists for.
A handler that panics is retried, because a panic on one payload says nothing
about the next attempt.

The producer could already ask for this with `max_retries: 0`, and that is the
wrong actor at the wrong moment: the producer does not know which failures are
permanent, and the handler is the only thing that does.

### Running a job on a repeat

```sh
quorractl schedule add \
  -name nightly -cron "0 3 * * *" -timezone Europe/Berlin \
  -catch-up skip -type report
```

A schedule is not a job.
It holds a rule, a zone and a catch up policy, it produces jobs, and it is
never handed to a worker.

**The time zone is an IANA name and never an offset.**
"Every day at nine" moves twice a year, and an offset is what the zone was on
the day it was written down.
Measured: `0 3 * * *` in `Europe/Berlin` fires at 01:00 UTC in August, and the
same rule in `UTC` fires at 03:00.

**`catch-up` is required and has no default.**
A server down from Friday to Monday misses seventy two windows of an hourly
schedule, and there is no answer that is right for every schedule.

| Policy | Does |
| ------ | ---- |
| `skip` | Fires once, at the most recent missed window. What almost every schedule means: a report that runs every hour does not want seventy two reports. |
| `all` | Fires every missed window, oldest first, bounded at a hundred. For a schedule where each firing does different work, keyed on its window. |
| `none` | Fires nothing until the next scheduled moment. For a schedule where a late firing is worse than a missed one. |

Measured against a real server, with a seventy two hour outage on three hourly
schedules: `all` produced 72 jobs, `skip` produced 1 and logged 71 dropped,
and `none` produced 0 and logged 72 dropped.
A ten day outage on the `all` one produced the hundred the bound allows and
reported 140 dropped.

A firing carries the window it belongs to and not the moment the loop woke up,
so a handler keyed on an hour gets that hour.
Each one is submitted under an idempotency key built from the schedule and the
window, so two servers running the loop produce one job.

The rule is five fields: minute, hour, day of month, month, day of week.
Numbers only, and no seconds field: a queue that hands a job to a worker over
a network cannot honour a second.
The two day columns are an `OR` when both name specific days, which is what
every cron does and what nobody believes without being told.

**A rule is read as a clock on a wall, and the clock changes twice a year.**
Two days a year need an answer, and both answers are chosen rather than
inherited.

On the day the clock goes back, a reading happens twice.
`0 2 * * *` in `Europe/Berlin` on 25 October 2026 could mean 02:00 CEST or
02:00 CET, an hour apart.
It fires once, at the first of the two, which keeps a daily schedule twenty
four hours from the day before.

On the day the clock goes forward, a reading does not happen at all.
02:00 does not exist in `Europe/Berlin` on 29 March 2026.
It fires once, at the first moment that does exist, which is 03:00.
A day missing once a year is found in the ledger and not in the log.

Measured against a real server, a daily schedule catching up with `skip`:

| Rule and zone | Span | Windows |
| ---- | ---- | ---- |
| `0 2 * * *` `Europe/Berlin` | 28 March to 3 September 2026 | 159, and 158 before this was fixed |
| `30 2 * * *` `Pacific/Auckland` | 4 April to 3 September 2026 | 152, and 153 before |

One window each way. The zone that put its clock forward gained the day it
used to step over, and the zone that put its clock back lost the firing it
used to do twice.

```sh
quorractl schedule list
quorractl schedule off nightly     # keeps it, produces nothing
quorractl schedule remove nightly  # the jobs it produced are kept
```

### Clearing a dead letter queue

Recovering after fixing what broke is the most common thing an operator does
to a queue.

```sh
quorractl revive -all -status dead -queue billing -limit 1000
```

One request and one transaction, rather than one of each per job.
Measured against PostgreSQL: five hundred dead jobs revived in 513ms in one
command, and `quorra_jobs_revived_total{caller="ops"}` moved to 500.
The same five hundred through one request each takes about five seconds.

`cancel -all` is the same shape.
Both take the filters `list` takes, so an operator narrows a listing until it
shows what they mean and repeats the options.

Two things are refused on purpose.
`-limit` is required, because a default would make the most dangerous command
in this tool the shortest one to type.
`-all` with no filter at all is refused, because moving every job the limit
allows is a real thing to want after a bad deployment and is not a thing to do
by leaving an option out.

A job the filter names that the action does not apply to is skipped rather
than refused.
A bulk action against a moving queue will always race something, and failing
the whole batch for it would make the operation useless.

### Submitting many jobs at once

```sh
quorractl create -file jobs.ndjson
```

One JSON object per line, or a dash for standard input.
Not one JSON array: a file of a million jobs read as an array has to be held
whole before the first one can be checked, and what a queue is fed from is
almost always a log or an export, which is already one record per line.

Measured: five hundred jobs in 523ms, against about five seconds for the same
five hundred one request at a time.

Each job is stored on its own and the answer says what happened to each.
One transaction for the batch would mean one bad payload losing the nine
hundred and ninety nine good ones, and jobs are independent.
The identifiers come first, one per line, so the output feeds a pipe the same
way one submission does, and every refusal names the job it came from.

### What a job did, run by run

The jobs table holds one row for each job, so a job that failed four times and
then worked carried one error, from whichever attempt wrote last, and no
record that the other three happened.

One row is written for each finished run.

```
$ quorractl history 7b8602a0-8007-406c-a060-0161355465ca
RUN  WORKER               OUTCOME    TOOK       ERROR
1    mailer-3             failed     4ms        this handler always fails, on purpose
2    mailer-3             failed     3ms        this handler always fails, on purpose
3    mailer-3             failed     2ms        this handler always fails, on purpose
```

A lease that ran out is a finished run too, and is the one nobody reported.

```
RUN  WORKER               OUTCOME    TOOK       ERROR
1    mailer-3             expired    34.216s    the lease held by mailer-3 ran out before it reported
```

That number is measured: a worker holding a job was killed with `SIGKILL`, the
lease was thirty seconds and the reclaim sweep runs every ten.

The row is written where the job is retired, so a worker reporting and a lease
running out write the same history through one path, and it commits in the
same transaction as the job.
Nothing is written when a run starts: a running attempt is already fully
visible on the job, which names the worker holding it and says when the lease
began, and an insert on the lease path would be a second write on the busiest
statement in the system to record what the first one already said.

A revived job keeps what it did before, and holds two runs numbered 1.
Reviving sets the attempt count back to zero on purpose, so the order of the
list is what says which run came first.

### Is anything out there

`leased_by` names the worker holding a job and is cleared when the job ends,
so a fleet with nothing to do left no trace anywhere.
An empty queue and a fleet that has stopped looked the same from outside, and
only one of those two is fine.

```
$ quorractl workers
WORKER                   QUEUE            IDLE         FIRST SEEN
mailer-3                 default          0s           12:16:55
```

A row is written where a worker asks for work, and whether or not any job
comes back.
The ask that finds nothing is the ask that matters.

There is one row for each worker and queue.
A worker asks about one queue at a time, so a row for the worker alone would
hold whichever queue it asked about last and change on the next ask, which
reads like a worker moving between queues.

Workers nobody has seen for `QUORRA_WORKER_RETENTION` are removed, a day by
default.
A worker identifier is usually the name of a container, so a deployment
retires every row in that table and writes a new set.

### Finding the job that is stuck

A job in `pending` with a `run_at` two hours out looks exactly like one that
is ready this second, so a queue that is not moving gives up nothing.

Four questions, and what answers each:

| The question | Ask for |
| ------------ | ------- |
| What would run right now? | `?due=now` |
| What is waiting out a backoff? | `?due=now` and read what is missing, or `?order=soonest` |
| What is `worker-7` holding? | `?worker=worker-7` |
| What is at the front of the line? | `?order=soonest` |

`quorractl list` spells the same things `-ready`, `-soonest` and `-worker`,
and shows a `RUNS AT` column when the answer depends on it.
The dashboard has a `ready` button beside the status filters and a `Runs at`
column.

`due` also takes a moment in RFC 3339, which is how to ask what the queue
looked like at a time that is not now.
The server resolves `now` against its own clock, so the answer does not depend
on two machines agreeing about the time.

### A slow job keeps its lease

A worker asks the server to push its lease out three times per lease while a
handler is running.
A third, so that two heartbeats can be lost to a slow network before the lease
actually runs out.

Without it, a handler slower than the lease it was given was doomed: the
reclaimer took the job at the expiry and handed it to somebody else while the
first worker was still running it.

The refusal is the other half.
When the answer says the job is no longer this worker's, because it was
cancelled or because the lease ran out, the context given to the handler is
cancelled with `worker.ErrLeaseLost` as its cause.
Nothing reaches into a running handler, which is the only way this could work.

### Acting on a job

A dead letter queue nobody can act on is a list of regrets.

| Action | What it does |
| ------ | ------------ |
| Cancel | Stops a job that has not finished. A job a worker is holding loses its lease, so that worker's next heartbeat fails and its handler stops. |
| Revive | Puts a dead or cancelled job back in the queue with the attempt count set to zero. |

The zero matters. Somebody clearing a dead letter queue has usually just fixed
the thing that broke and wants the job to have the full set of tries again.
Leaving the count where it was gives it one more, which looks like it worked
until the queue fills up again an hour later.

A job that succeeded cannot be revived.
Running it again is a new piece of work and deserves a new identifier that the
caller can follow.

Both actions record the key that asked, on the job as `acted_by` and
`acted_at`, and on the two counters as a `caller` label.
The dashboard marks a row that carries one with a star beside the status and
puts the name and the moment on the cell.
`quorractl cancel` and `quorractl revive` print it on the line they answer
with, so an operator with two keys in a shell profile finds out at once that
the action went down under the wrong one.

### Running a job after another job

A job may name jobs it waits for.
It runs when every one of them has succeeded.

```sh
extract=$(quorractl create -type extract)
load=$(quorractl create -type load -after "$extract" | head -1)
quorractl create -type report -after "$load"
```

Three things had to be decided before a row could be written, and they are
worth reading before using this.

**A job that will never succeed cancels what waits for it**, and the reason
names the job that stopped it.
`cancelled` and not `dead`: `dead` means the job used every attempt it had,
and a waiting job used none.
A person who fixes the parent revives the child, which is a path they already
know.
Reviving the parent does not release the children on its own, because
cancelled is a state a person leaves.

**A cycle is impossible rather than refused.**
A job may only wait for a job that already exists, because a caller has to
name an identifier, and a job cannot be created before itself.
There is no cycle check anywhere, and there is none to forget.

**A revived job goes back to waiting when it still waits.**
Sending it to `pending` would run it before the job it was submitted to
follow, which is the one thing this exists to stop.
A job whose parent is still dead cannot be revived until that parent is, and
the refusal names it.

The list is bounded at sixty four.
Every job in it is read when the job is submitted and again whenever one of
them ends, so an unbounded list is an unbounded amount of work on a path a
caller controls.

### Submitting the same job twice

A client that sends a job and does not see the answer cannot tell whether the
server stored it.
Retrying is the only thing it can do.

Send an `idempotency_key`, in the body or in the `Idempotency-Key` header, and
the second submission gives back the first job and answers `200` instead of
`201`.

The check is the database's, not the code's.
Reading for an existing key and inserting afterwards lets two submissions
carrying one key both find nothing and both insert, which is the exact case
the key exists to prevent.

### Keeping the table from growing for ever

Nothing is ever removed unless you ask.
`QUORRA_RETAIN_SUCCEEDED`, `QUORRA_RETAIN_DEAD` and `QUORRA_RETAIN_CANCELLED`
each default to keeping the job for ever.

That default is deliberate.
A queue holds the only record that a piece of work happened, and a default
that quietly removed it would take that record from every deployment that
upgraded without reading the notes.

They are separate because the jobs are not alike.
A succeeded job is noise after a week.
A dead one is evidence.

---

### The dashboard

At `/`, and it asks for the API key rather than carrying one.
The key stays in the tab and is never written into the page or into a query
string.

| To do this | Press |
| ---------- | ----- |
| See only one status | A status button, including `blocked` and `ready` |
| See past the first twenty five rows | `Show 25 more` |
| Open one job, with its payload and every run of it | The identifier in its row |
| Find a job from an identifier in a log line | The `Find` box |
| Stop or revive a job | The buttons in its row |
| Stop or revive everything the filter names | The button under the filters |
| Switch a repeat schedule on or off | The button in its row |
| See which key the page is holding | The badge in the header |

The page shows the workers as well as the jobs.
A queue that is filling up because no worker has asked for work looks exactly
like one that is filling up because the work is slow, and the cards are the
difference.
A worker that asked about two queues is one card, because it is one process.

It shows the repeat schedules too.
A schedule that is switched off produces nothing, and from the jobs table that
looks exactly like a schedule that is working with nothing due.
A switched off one is dimmed rather than hidden: somebody switched it, and
hiding it would leave them looking for it.

**The bulk action is offered only when a status is chosen.**
The filter that names every job is the one nobody means to act on.
It is bounded by what is on the page, and it asks before it runs, so nothing
moves that the reader has not seen.
A finished status offers `Revive` and an unfinished one offers `Cancel`.

Clearing a dead letter queue was a shell loop before this.
The routes existed and only `quorractl` could reach them.

Opening a job is the only place the payload is shown.
A listing cannot carry it: the payload of one job can be larger than a page of
rows, and every row would have to carry one.

The page asks the server every five seconds while it is being watched.
A failure doubles the wait to a ceiling of a minute, six failures in a row
stop it, and a tab that is hidden is asked for nothing.
Correcting the key clears the wait and refreshes at once.
Measured over the browser: with a wrong key, two requests in twenty one and a
half seconds rather than eight.

Driven over the browser again after the workers, the schedules and the bulk
action were added: the badge read `ops (may write)`, one worker card, both
schedules with the enabled one showing its next firing and the switched off
one showing `off`, no bulk button while the filter was `all`, and
`Revive these 4` once `cancelled` was chosen. Pressing it moved four jobs
back to pending. Zero console messages, zero policy refusals, zero failed
requests.

A job identifier never reaches the address bar.
It would be kept in browser history and sent on in the `Referer` of anything
the page fetched afterwards, which is the reason the key is not in a query
string either.

---

## Writing a worker

A real worker is your program.
It imports one package and registers handlers.

```go
package main

import (
	"context"
	"log"

	"github.com/Stiven-Gjekaj/GoQuorra/worker"
)

func main() {
	w, err := worker.New(worker.Config{
		ServerAddr: "localhost:50051",
		Queues:     []string{"email"},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()

	w.Handle("email_send", func(ctx context.Context, job worker.Job) error {
		var mail struct {
			To      string `json:"to"`
			Subject string `json:"subject"`
		}
		if err := job.Decode(&mail); err != nil {
			return err
		}
		return send(ctx, mail.To, mail.Subject)
	})

	if err := w.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
```

Returning `nil` finishes the job.
Returning an error sends it back, or buries it when no attempts remain, and
the text of the error is kept on the row.

Four things the package does for you:

- **A panic costs one job.** A handler is your code, and letting a panic
  through would end the process and lose every other job in flight, each of
  which would then wait out its lease.
- **The handler's context ends when the lease does.** Past that moment the
  server has given the job to somebody else, so anything the handler goes on
  to do is work being done twice.
- **A stopping worker finishes what it is running and reports it.** The report
  is sent on a context of its own, because the one that ran the job has ended
  by then.
- **An unknown job type is a failure that names the type.** The usual cause is
  a deployment where the producer knows a job type and the workers do not yet.

### Keeping what a job produced

`HandleResult` registers a handler that returns a value as well as an error.
What it returns is stored on the job and served back by the API.

```go
w.HandleResult("count_rows", func(ctx context.Context, job worker.Job) (any, error) {
	n, err := count(ctx)
	return map[string]int{"rows": n}, err
})
```

Keep it small.
The server refuses a result past its limit rather than trimming one, because
half a JSON document is not a smaller result: put a large value where it
belongs and return a reference to it.

`cmd/quorra-worker` is a demonstration with three handlers: `echo`, `sleep`,
and `fail`. `fail` exists so that the retry schedule and the dead letter queue
can be watched happening.

### A side effect that happens once

GoQuorra delivers at least once.
A handler can finish its work and lose power before it reports, and the job is
then given to another worker.
That window is between the side effect and the acknowledgement of it, and no
protocol removes it.

For one case it can be made empty.
A handler whose side effect is a write to the same PostgreSQL database the
queue is in can do that write and record the outcome of the job in one
transaction, so the two commit together or neither does.

[`worker/pgtx`](worker/pgtx) is that API.

```go
runner, err := pgtx.New(pool)
if err != nil {
	return err
}

w.HandleResult("charge", runner.Handle(
	func(ctx context.Context, tx pgx.Tx, job worker.Job) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO charges (job_id, amount) VALUES ($1, $2)`, job.ID, 1250)
		return err
	}))
```

The pool has to be the one the queue is in.
A pool pointed at another database compiles, runs, and gives exactly the at
least once behaviour this package exists to improve on, with no sign that it
had.

**It is not exactly once, and it must not be described as one.**
A handler that calls another service, writes to another database, or sends an
email has the same window it always had, and has to be written to survive
running twice.

Measured against a live server.
Five jobs whose handler wrote its row and then failed once wrote ten rows
through an ordinary handler, one for the failed attempt and one for the
successful one, and five rows through a transaction one.

It is its own package because a handler here takes a `pgx.Tx`.
A consumer that only submits and runs jobs should not start compiling a
database driver to do it.

---

## Submitting work

A producer imports [`client`](client), the other half of the worker package.

```go
c, err := client.New(client.Config{
	Server: "http://localhost:8080",
	APIKey: os.Getenv("QUORRA_API_KEY"),
})
if err != nil {
	return err
}

job, err := c.Submit(ctx, client.NewJob{
	Type:    "email_send",
	Payload: mail,
	Key:     "welcome-" + user.ID,
})
```

Nothing in that package holds a type from inside this repository, so a caller
depends on it without depending on how the server stores anything.

A refusal arrives as an error the caller can test: `client.ErrNotFound`,
`client.ErrWrongState`, `client.ErrUnauthorized` and `client.ErrNameTaken`,
each wrapping the sentence the server wrote.
`ErrWrongState` is the one worth having, because a caller that waits and asks
again may succeed.

The package reaches every route under `/v1` that takes a key, and a test says
so by asking the router rather than by holding its own list.

| To do this | Call |
| ---------- | ---- |
| Submit one job, or many at once | `Submit`, `SubmitMany` |
| Read one job, or page through them | `Get`, `List`, `Each` |
| Read what a job did on each attempt | `Attempts` |
| Stop or restart one job | `Cancel`, `Revive` |
| Stop or restart everything a filter names | `CancelMatching`, `ReviveMatching` |
| Count what is in each queue | `Queues`, and `Waiting` over the answer |
| Ask whether any worker is out there | `Workers` |
| Manage a repeat schedule | `CreateSchedule`, `Schedules`, `Schedule`, `EnableSchedule`, `DisableSchedule`, `DeleteSchedule` |
| Ask which key this client holds | `Whoami` |

`Waiting` adds pending and blocked together.
A queue of a thousand blocked jobs is not idle, and counting pending alone
says that it is.

`Whoami` answers the queues the key holds as well as its scope.
A key limited to queues reads an empty listing for one it cannot reach, which
looks exactly like an empty queue, and `Identity.MayUse` is how a producer
tells the two apart.

---

## The HTTP API

Every route under `/v1` needs the `X-API-Key` header.
The key is not accepted in the query string, because a query string is written
to the access log of every proxy in front of the server, kept in browser
history, and sent on in the `Referer` header.

Each key has a name, a scope and the queues it may act on.
Set them in `QUORRA_API_KEYS`, as a comma separated list of
`name:scope:secret`:

```sh
export QUORRA_API_KEYS="ops:write:$(openssl rand -hex 32),reports:read:$(openssl rand -hex 32)"
```

The scope is `read`, `write`, `worker` or `all`, or several joined by a plus.

| Scope | May |
| ----- | --- |
| `read` | Ask questions and change nothing |
| `write` | Everything `read` may, and submit, cancel and revive |
| `worker` | Lease a job and report on it, over gRPC, and nothing else |
| `all` | All three |

`worker` is not more than `write` and it is not less.
It is a different door.
A key an operator keeps in a shell profile must not be able to lease the queue
empty, and a worker must not be able to cancel anything, so the two do not
contain each other.

A key that may only read gets `403` from a route that changes a job, and not
`401`: the key is real and the server knows whose it is, and `401` would send
somebody to check a key that is working correctly.
The worker protocol answers the same way, as `PermissionDenied` rather than
`Unauthenticated`.

`QUORRA_API_KEY` still works and means one key named `default` that may do
everything, because a deployment that sets one key is saying it does not want
to divide anything yet.

**A key may be limited to queues.**
Write them after the scope, with an at sign, joined by a plus:

```sh
export QUORRA_API_KEYS="billing:write@invoices+receipts:$(openssl rand -hex 32)"
```

A key that names no queue holds every queue, so nothing changes for a
deployment that has not divided anything.

The queues go in the scope field and not in a fourth one, so that the secret
stays the whole of the rest of the entry.
A secret holding a colon is written correctly, and a fourth field would take
that away.
A queue whose name holds `:`, `,`, `@` or `+` cannot be named in a key, and
the server says so at startup rather than holding a queue nobody meant.

| The key asks to | And gets |
| ---- | ---- |
| List, count, or read one job in a queue it does not hold | Nothing. A listing skips it, the counts skip it, and one job by identifier is `404`. |
| Cancel or revive one job in a queue it does not hold | `404`, the same as a job that is not there. |
| Cancel or revive by filter | Only the jobs in the queues it holds. |
| Submit into a queue it does not hold | `403`, naming the queues it holds. |
| Lease or watch a queue it does not hold | `PermissionDenied`. |

`404` on the read side and `403` on the write side, on purpose.
A caller that names a queue to write to already knows the name it asked for,
so there is nothing to hide, and being told is the only useful answer.
A caller holding a job identifier learns nothing from `404`, and would learn
from `403` that the job exists.

Measured against a live server, with `billing` holding two queues of three and
`ops` holding every queue:

| Asked | `billing` | `ops` |
| ---- | ---- | ---- |
| List every job | 7 jobs, in 2 queues | 10 jobs, in 3 queues |
| Count the queues | 2 | 3 |
| Read one job in the third queue | `404` | `200` |
| Cancel by filter, no queue named | 7 stopped, the third queue untouched | every one |

A worker key limited to `invoices` leased from it, and was refused `payroll`
and the default queue with `PermissionDenied`.

`quorractl whoami` says which queues a key holds:

```
$ quorractl whoami
billing (may write, on invoices, receipts)
```

The name is what the server records against a cancel or a revive.
Names tell services apart, not people.
There is no user model here, and a key shared by four people on a team is a
key that names the team.

### `POST /v1/jobs`

```json
{
  "type": "email_send",
  "payload": { "to": "user@example.com" },
  "queue": "default",
  "priority": 0,
  "delay_seconds": 0,
  "max_retries": 3
}
```

`type` is required. Everything else has a default.
`after` names jobs this one waits for, and every one has to already exist.
`max_retries` of `0` means no retries, and is not the same as leaving it out.
A field the server does not know is refused rather than ignored, so
`maxRetries` gets an error instead of quietly doing nothing.

Answers `201` with `{"id", "status", "queue", "run_at"}` and a `Location`
header, or `200` with the same shape when an `idempotency_key` has been used
before.

### The rest

| Route | Gives |
| ----- | ----- |
| `GET /v1/jobs/{id}` | One job. `404` when there is none, and `500` when the database is unreachable. |
| `GET /v1/jobs` | Jobs, newest first. Narrowed by `queue`, `status`, `type`, `worker`, `due` and `limit`, ordered by `order`, and paged with `before`. |
| `POST /v1/jobs/{id}/cancel` | Stops a job that has not finished. `409` when it already has. |
| `POST /v1/jobs/{id}/revive` | Puts a dead or cancelled job back with a fresh set of attempts. `409` for any other state. |
| `GET /v1/queues` | A count for each queue and status. |
| `GET /v1/whoami` | The name and the scope of the key that asked. Needs `read`, because a key that changes nothing still has to be able to ask what it is. |
| `GET /v1/jobs/{id}/attempts` | What the job did, one row for each finished run. `200` with an empty list for a job that has not run, and `404` only when the job is not there. |
| `GET /v1/workers` | The workers the queue has heard from, most recently first, with how long each has been quiet. |
| `POST /v1/jobs/bulk` | Submits many jobs in one request. Answers for each on its own, so one bad row does not lose the others. |
| `POST /v1/jobs/cancel` | Stops every job a filter names, up to a required `limit`. |
| `POST /v1/jobs/revive` | Puts back every job a filter names, up to a required `limit`. |
| `POST /v1/schedules` | Stores a repeat schedule. `catch_up` is required. |
| `GET /v1/schedules` | The schedules, with when each one fires next. |
| `GET /v1/schedules/{name}` | One schedule. |
| `POST /v1/schedules/{name}/enable` | Switches it on. |
| `POST /v1/schedules/{name}/disable` | Switches it off. It keeps its history and produces nothing. |
| `DELETE /v1/schedules/{name}` | Removes it. The jobs it produced are kept. |
| `GET /healthz` | `200` while the process is running. Public. |
| `GET /readyz` | `200` while the store can be reached. Public. |
| `GET /metrics` | Prometheus. Public. |
| `GET /` | The dashboard. |

Paging is a cursor and not an offset.
An offset re-reads and skips every row before the page, so a job submitted
while somebody is reading shifts every later page by one, which shows them a
row twice and hides another entirely.
Take `next_cursor` from a page and pass it back as `before`.
A short page carries no cursor, because a short page is the end.

`order=soonest` gives the job that runs first, first, which is the order the
queue itself works in.
Paging still holds under it, because the cursor compares the pair of `run_at`
and the row sequence rather than `run_at` alone.
`run_at` is not unique: a burst of submissions shares one value and every job
a reclaim sweep returns shares one, so a cursor on it alone would repeat rows
or skip them.

A job that has been cancelled or revived carries `acted_by` and `acted_at`.
They name the key that acted last, and the moment it acted.
A job nobody has acted on carries neither.
Only cancel and revive set them: the queue leasing, retrying or burying a job
is not a person acting, and recording the queue as an actor would make the
field useless for the question it exists to answer.

A job in the wrong state answers `409` and not `400`.
The request is well formed and would be correct against the same job a moment
later, so a client that retries once the job moves is behaving sensibly, and
`400` tells it never to try again.

`healthz` and `readyz` are different on purpose.
Point a liveness probe at `healthz` and a readiness probe at `readyz`.
A liveness probe that reaches the database restarts every replica when the
database goes away for a minute, which is the one thing guaranteed to make an
outage worse.

---

## What it publishes

| Metric | Type | Holds |
| ------ | ---- | ----- |
| `quorra_jobs_created_total` | counter | Jobs accepted from a client |
| `quorra_jobs_leased_total` | counter | Times a job has been handed to a worker |
| `quorra_jobs_succeeded_total` | counter | Jobs a worker reported as finished |
| `quorra_jobs_retried_total` | counter | Failures that sent a job back |
| `quorra_jobs_dead_total` | counter | Jobs in the dead letter queue, however they got there |
| `quorra_jobs_refused_total` | counter | Jobs a handler refused, a part of the line above |
| `quorra_leases_reclaimed_total` | counter | Leases taken back after they ran out |
| `quorra_jobs_cancelled_total{caller}` | counter | Jobs stopped by a person, by the key that asked |
| `quorra_jobs_revived_total{caller}` | counter | Jobs put back in the queue by a person, by the key that asked |
| `quorra_schedule_firings_total{schedule}` | counter | Jobs produced by a repeat schedule |
| `quorra_jobs_removed_total{status}` | counter | Jobs taken out by the retention sweep |
| `quorra_queue_length{queue,status}` | gauge | Jobs in each queue, refreshed on a timer |
| `quorra_job_lifetime_seconds{queue,status}` | histogram | Acceptance to final state, so the waiting and the retries are in it |
| `quorra_jobs_finished_total{type,status}` | counter | Jobs that reached a final state, by job type |
| `quorra_job_type_lifetime_seconds{type}` | histogram | The same measurement as the lifetime above, by job type |
| `quorra_job_types_tracked` | gauge | Job types with a row of their own |
| `quorra_http_request_duration_seconds` | histogram | Labelled by route pattern, not by path |
| `quorra_grpc_request_duration_seconds{method,code}` | histogram | Calls on the worker protocol that answer once |
| `quorra_grpc_streams_total{method,code}` | counter | Streams on the worker protocol that ended |

`quorra_jobs_refused_total` is a part of `quorra_jobs_dead_total` and not a
number beside it, so the two divide.
A dead letter queue filling with refusals says the work being submitted is
wrong, and one filling with exhausted attempts says something outside is down.
Those need different people.

A retry and a burial are separate counters.
The previous version raised one counter for both, so the failure rate counted
a buried job twice, and `quorra_jobs_dead_total` was declared and never raised
by anything.

Cancelling and reviving are counted apart from everything else, because both
are a person acting.
A rise in either says something about the operators rather than about the
work, and folding them into the job counters would hide that.

Both carry the name of the key that asked.
One number for the whole deployment says that somebody cancelled forty jobs
this morning, and on a queue two teams share the only part worth acting on is
which team.
The label is safe to add without a bound: the names come from the server
configuration and not from a request, so it has as many values as the
deployment has keys.
A key that did not name itself is counted as `unknown`, because an empty
label value reads as a fault in the exporter.

### The job type label has a bound, and the others do not need one

Which queue is failing is a question about how the work is arranged.
Which type is failing is a question about the work.
`quorra_jobs_finished_total` answers the second one.

Job type is the first label here that a caller fills in.
Every distinct value is a time series the metrics store keeps for as long as
its retention says, so a caller that puts an identifier in a job type takes
down the metrics store, and every dashboard with it, without meaning to.

So fifty types keep a row of their own and everything after that is counted as
`other`.
Nothing is dropped and nothing undercounts: a sum over the label is still
every job.
Driven against a live server with sixty three types, the counter held fifty
one rows, `quorra_job_types_tracked` read 50, `other` held 13, and the sum
over the label was 90 against 68 dead and 22 succeeded.

`quorra_job_types_tracked` sitting at fifty is how an operator finds out that
`other` is holding more than it looks.

A type that already has a row keeps it.
Folding one into `other` partway through a day stops a series for no reason a
reader of the dashboard can see.

### The worker protocol is timed

Every job is leased and reported over gRPC, and that whole path was untimed
until `quorra_grpc_request_duration_seconds`.
The HTTP histogram beside it has been published since the first release, so
the one number an operator had covered the half of the traffic that does not
run the work.

Streams are counted rather than timed.
A watch lives as long as the worker does, so the time it was open says how
long the worker ran and not how fast this server is, and hours in the same
histogram as a lease leaves the quantiles meaning nothing.

A call the guard refused is timed like any other, because a refusal that took
a second is worth seeing.
Measured on a live server: 610 `Lease` calls at `OK` and 25 at
`Unauthenticated`, 90 `Report` calls of which 88 answered inside 5ms, and 6
`Watch` streams, 5 of them refused.

---

## Finding one request in the log

Every answer carries an `X-Request-Id` header, and every line the server wrote
while making that answer names it.

```
$ curl -si -X POST localhost:8080/v1/jobs -H 'X-API-Key: ...' -d '{"type":"echo"}'
HTTP/1.1 201 Created
X-Request-Id: 9acf4dc0-b82a-4995-a07d-2b3314b2847e
```

```
{"msg":"job accepted","request":"9acf4dc0-...","job":"54bea554-...","type":"echo"}
```

A caller that sends its own identifier keeps it, so both sides quote one
string.
The same header works on the worker protocol, where it travels as gRPC
metadata.

What a caller sends is refused rather than trimmed: anything longer than 64
characters, and anything outside printable ASCII.
A log line is a line, so a value with a newline in it would write a line of
the caller's own choosing into the log of the server.
An identifier the server rewrote is worse than a fresh one, because it no
longer matches what the caller kept and the caller does not know it changed.

The job identifier is what joins one request to the next.
The line that accepted a job names it, the line that leased it names it, and
the line that reported on it names it:

```
{"msg":"job accepted","request":"trace-from-the-caller-1","job":"a7d0a242-..."}
{"msg":"leased jobs","request":"c1a33fea-...","worker":"mailer-3","count":2,"jobs":["a7d0a242-...","54bea554-..."]}
{"msg":"recorded a report","request":"ddcc6411-...","job":"54bea554-...","outcome":"done"}
```

The lease line said how many and never which until this release, so a reader
with a job that went missing had the line that accepted it and the line that
reported on it, and nothing in between.

A refused request leaves a line of its own, so that the identifier a caller is
told to quote is quotable against something:

```
$ quorractl get 8f14e45f-ceea-467a-9c37-8e8f8f8f8f8f
quorractl: the server refused this: no job carries that identifier (request 660a05cf-...)
```

```
{"level":"INFO","msg":"request refused","request":"660a05cf-...","method":"GET","route":"GET /v1/jobs/{id}","code":404}
```

Refusals only.
A queue answering normally is the normal case and a line for each would bury
the ones that matter, while a queue refusing constantly is itself worth
seeing.

---

## Project structure

Two ideas carry the design.

**The deciding part is pure.**
[`internal/jobs`](internal/jobs) holds the rules a job follows.
It imports nothing outside the standard library, holds no database handle, and
reads no clock.
A table test drives every state a job reaches with nothing installed.

**Two stores answer to one suite.**
[`internal/store/storetest`](internal/store/storetest) holds one hundred and
eight rules.
The in-memory store passes them with nothing installed and the PostgreSQL
store passes them against a real database.
A store with its own private tests agrees only with itself, and then it stands
in for a database it does not behave like.

| Area | Lines | Holds |
| ---- | ----- | ----- |
| `internal/jobs` | 823 | The states, the retry decision, the backoff, the cron rule. Standard library only. |
| `internal/store` | 942 | The interface, the errors, the defaults |
| `internal/store/memory` | 1247 | Jobs in a map |
| `internal/store/postgres` | 1925 | Jobs in PostgreSQL |
| `internal/api` | 1279 | The REST routes and the dashboard |
| `internal/rpc` | 665 | The worker protocol, the guard and the timing |
| `internal/server` | 577 | Assembly, the background loops, shutdown |
| `internal/config` | 499 | Reading the environment |
| `internal/metrics` | 422 | What the server publishes |
| `internal/auth` | 243 | The keys and what each may do |
| `internal/reqid` | 83 | The identifier every request carries |
| `worker` | 873 | The package a consumer imports |
| `worker/pgtx` | 151 | A handler in the transaction its report commits in |
| `client` | 733 | The package a producer imports |
| `cmd` | 1241 | Three binaries |
| **Total** | **11703** | 45 Go files, not counting 9811 lines of tests, 3294 of test support or 1339 of generated code |

`internal/store/storetest` is 3153 lines of that test support and
`internal/pgtest` is 141.
They are counted apart because they run no server: one is the suite both
stores answer to and the other is how a suite reaches a real database without
walking into another suite.

```
proto/quorra/v1/       the worker protocol
internal/quorrapb/     generated from it, and checked by CI
migrations/            ten files, applied in name order and embedded so the
                       tests apply the same bytes an operator reads
deployments/           the compose stack and the Kubernetes manifests
scripts/               generation, the link check, the smoke test
```

---

## Dependencies

Five, directly.

| Module | For |
| ------ | --- |
| `github.com/jackc/pgx/v5` | PostgreSQL. `lib/pq` says in its own README that it is in maintenance mode and points here. pgx also cancels a query on the wire when the context ends. |
| `google.golang.org/grpc` | The worker protocol |
| `google.golang.org/protobuf` | The messages on it |
| `github.com/prometheus/client_golang` | The metrics page |
| `github.com/google/uuid` | Identifiers |

Five, after adding a job lifecycle, filtering, heartbeats, idempotency,
retention, results, a client package, named keys, attempt history, a worker
registry, job relationships, repeat schedules, push instead of poll, and a
handler that runs in the transaction its report commits in. None of those
needed a new one.

The cron parser is the clearest case. The standard library has none, and
writing a five field parser here cost less than a sixth dependency and its
upgrade path.

Seventy three modules in the whole graph, most of them beneath those five.

Two dependencies the previous version carried are gone.
`chi` went because Go 1.22 gave `net/http` the method and wildcard patterns it
was there for.
`cobra` went because `quorractl` was four verbs with a handful of options
each, and the standard library covers that in less code than the dependency.
It is ten verbs now and the answer has not changed.

Redis went as well.
The old server published a message to a channel on every submission, and
nothing anywhere subscribed to it.

---

## Testing

```
make verify
```

That runs the formatting check, `go vet`, the build, the tests under the race
detector, the generated code check, and the documentation links.

**529 cases pass. 415 of them need nothing installed.**

The other 114 need a database. One hundred and eight are the store contract
suite, five are `worker/pgtx`, which is one transaction covering a handler's
writes and the record of the job and so has nothing to test without a real
one, and the last is the test that holds the contract suite:

```
export QUORRA_TEST_DATABASE_URL="postgres://quorra:quorra@localhost:5432/quorra_test?sslmode=disable"
make test-postgres
```

A database of its own, and `quorra_test` rather than the `quorra` the compose
stack uses.
The suite empties every table between cases, so a server pointed at the same
database loses its jobs, and its lease sweep lands in the middle of a case
about a lease.

`make test-postgres` sets `QUORRA_TEST_REQUIRE_POSTGRES`, which turns a skip
into a failure.
CI sets it too, and then counts the cases that ran and prints the number.

That pairing exists because of what it replaced.
The old suite skipped every test when the database was missing, so
`go test ./...` reported success having run nothing, on a module that did not
compile.

Four rules shape these tests, and each one comes from a way a test can lie.

**A test has to be able to fail.**
The retry limit test was checked by putting the old comparison back and
watching three of its four cases fail.
The removed states test was checked by putting `processing` back into `Valid`.

**A test states its own state.**
Nothing reads a value out of a configuration file that somebody edits.

**A test asserts a property, not a byte.**
JSONB reorders keys and drops spaces, so a payload is compared by what it
means.
A byte comparison passes against the memory store and fails against
PostgreSQL, for a reason that has nothing to do with either being wrong.

**The protocol is driven over a real connection.**
`bufconn` costs milliseconds.
Calling a service method directly tests the Go and skips the codec, and the
codec is where the worst defect in this project's history lived: two files
headed "Code generated by protoc-gen-go. DO NOT EDIT." were written by hand,
their messages were plain structs, and gRPC refused every call at run time.
CI regenerates the code and fails on a difference.

---

## Deployment

The [Dockerfile](Dockerfile) builds one image holding all three binaries.
It runs as a user that is not root and carries no default key.

[`deployments/docker-compose.yml`](deployments/docker-compose.yml) runs the
whole stack for development, and CI uses the same file to submit a job and
wait for a worker to finish it.

[`deployments/k8s`](deployments/k8s) holds manifests, and
[its own page](deployments/k8s/README.md) says plainly what they do not do:
they run no database, they apply no schema, and their resource limits are not
measurements of anything.

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md), follow the
[Code of Conduct](CODE_OF_CONDUCT.md), and read [SUPPORT.md](SUPPORT.md) if
you need help.
[AGENTS.md](AGENTS.md) sets the rules for commits and for writing, and it
applies to a person as well as to an agent.
The [changelog](CHANGELOG.md) records what changed.

[docs/milestones.md](docs/milestones.md) holds the work that is not built, and
the things that were looked at and deliberately left, with what would have to
change to revisit each one.

---

## License

Released under the MIT License.
See [LICENSE](LICENSE) for the full text, and [TERMS.md](TERMS.md) for the
project terms.

The mark in [internal/api/logo.svg](internal/api/logo.svg) was drawn for this
project. Nothing in it comes from an icon set, so there is no third party
licence to carry for it, and the file says in a comment what each part of it
means. It sits beside the server code because the dashboard serves it from
there, and go:embed cannot reach a parent directory: one file in an odd place
beats two copies that drift.
