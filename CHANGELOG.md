<div align="center">
  <a href="README.md"><b>GoQuorra</b></a>
</div>

# Changelog

All notable changes to GoQuorra are recorded here.
The format is based on Keep a Changelog (https://keepachangelog.com), and the
project aims to follow semantic versioning.

A commit carries no version prefix and changes no version.
A version moves only when something is released.

## Unreleased

### An error says whose mistake it is

**Fixed**

- **The API decided whether a refusal was the caller's by reading the first
  seven characters of the message.** Every sentinel in the store package
  begins with those same seven characters, so a job that is not there, a
  stale lease and a job in the wrong state all read as something the caller
  had got wrong. One error used only inside the PostgreSQL store began with
  them too.

  The check in front of it did nothing. Its target was an interface holding
  only `Error`, which every error in Go satisfies, so it said yes to a
  connection failure as readily as to a refused job.

  It also meant that rewording any sentence in that package moved a status
  code, with nothing anywhere failing. The package had already learned this
  once: `ErrNotJSON` exists because the same layer used to search the text
  for the words "not JSON", and its comment says so.

  Twenty one refusals now carry one of two sentinels, and the API tests those
  instead. Every one of them was checked by putting the fault back.

- **A schedule name that is taken answers 409 rather than 400.** It was
  decided by searching the message for the words "already exists". 400 tells
  a caller never to try again, and this request would be accepted the moment
  somebody removes the schedule holding the name. It has its own sentinel,
  because a request that is not valid and a name that is in use are not the
  same answer.

- **A schedule that is not there said "no job carries that identifier".** The
  sentinel for a missing row is shared by both resources and one helper
  answered for both, so a person who asked for a schedule by name went
  looking for a job. The wording matches what a key that may not see the
  schedule is told, so the two cannot be told apart.

- **A refusal a caller reads no longer names a Go package.** Every message
  from the store package began with `store: `, and the cron and catch up
  messages began with `jobs: `. The HTTP layer handed them to the client
  unchanged. The rest of the domain package keeps its prefix: those messages
  are read at startup and by a worker, where the package name says which part
  of the configuration is wrong.

  Driven against a live server and PostgreSQL 16, before and after:

  | Request | Before | Now |
  | --- | --- | --- |
  | A job with no type | 400 `store: a job needs a type` | 400 `a job needs a type` |
  | A cron rule of three fields | 400 `store: jobs: "not a cron" has 3 fields...` | 400 `"not a cron" has 3 fields...` |
  | A catch up policy nobody knows | 400 `store: jobs: "maybe" is not a catch up policy...` | 400 `"maybe" is not a catch up policy...` |
  | A schedule name in use | 400 `store: a schedule named "q3" already exists` | 409 `a schedule named "q3" already exists` |
  | A schedule that is not there | 404 `no job carries that identifier` | 404 `no schedule carries that name` |
  | A job that is not there | 404 `no job carries that identifier` | unchanged |

**Added**

- **Four contract rules about how a store refuses.** The suite checked that
  both stores refused the same things and not that they refused them the same
  way, so a store answering "connection refused" passed. One existing rule
  tested the seven character prefix and moves to the sentinel with the rest.

  The rule about the integer columns only ever covered three numbers.
  Reverting the very first check in the validator left it passing, so a
  second rule covers the other seven refusals.

### A schedule answers the same per queue rule as a job

**Fixed**

- **A key limited to its queues could reach any schedule.** The check that
  the per queue keys added went on the create path and stopped there. The
  other five routes on this resource did not have it.

  A key holding only `invoices` could read a `payroll` schedule by name, with
  its rule, its zone and the payload it produces. It could switch that
  schedule off, which is the quietest way to stop another team's work:
  nothing fails, nothing is logged as a failure, and the jobs simply stop
  arriving. It could also remove the schedule, which takes the rule and the
  payload with it and leaves no copy anywhere.

  Read, list, enable, disable and remove now answer the rule. `404` and not
  `403`, because a schedule name is chosen by whoever made it, so a caller
  guessing names would learn from a `403` which of its guesses are real. That
  is the answer a job in another queue already gives.

  The listing narrows in the handler rather than in the store. The rule
  belongs to the caller and not to the storage, and putting it in the store
  would mean writing it twice and keeping the two versions in agreement.

  Each of the four rules was checked by putting the fault back. Two of them
  needed the check made never to hold rather than deleted: deleting it leaves
  a variable unused, the package stops compiling, and a test that does not
  run is not a test that failed.

  Driven against a live server and PostgreSQL 16. A key holding `invoices`
  was answered `404` to reading, switching off and removing a `payroll`
  schedule, saw one name in the listing where the unlimited key saw two, was
  answered `200` for its own queue's schedule, and the `payroll` schedule was
  still enabled afterwards.


### The dashboard shows what the API knows

The API serves eighteen routes and the page used four of them. Two whole
resources were invisible, and the one action that arc was built for was still
a shell loop.

**Added**

- **A workers view.** A queue filling up because no worker has asked for work
  looks exactly like a queue filling up because the work is slow. Grouped by
  worker, because one worker asking about two queues is one process. A minute
  of silence is coloured like a job that stopped.
- **A schedules view, with a switch.** A schedule that is switched off
  produces nothing, and from the jobs table that looks exactly like one that
  is working with nothing due. A switched off schedule is dimmed rather than
  hidden: somebody switched it, and hiding it would leave them looking for it.
  The next firing comes from the server, because a page that worked it out in
  a browser would answer in whatever zone the reader's machine is set to.
- **Cancel or revive everything the filter names.** Clearing a dead letter
  queue was a shell loop: the routes existed and only `quorractl` could reach
  them.

  Offered only when a status is chosen, because the filter that names every
  job is the one nobody means to act on. Bounded by what is on the page and
  asked about before it runs, so nothing moves that the reader has not seen. A
  finished status offers revive and an unfinished one offers cancel.
- **The key the page is holding, in the header.** A key limited to queues
  shows an empty listing for a queue it cannot reach, which reads exactly like
  an empty queue.

  Driven over the browser: the badge read `ops (may write)`, one worker card,
  both schedules with the enabled one showing its next firing and the switched
  off one showing `off`, no bulk button while the filter was `all`, and
  `Revive these 4` once `cancelled` was chosen. Pressing it moved four jobs
  back to pending. Zero console messages, zero policy refusals, zero failed
  requests.

**Fixed**

- **The rule about the empty row spanning the table measured the wrong
  table.** It counted the first heading row on the page, which stopped being
  the jobs table when the schedules table went above it. It names the table it
  means now, and checks both.

### A key is limited to its queues

`SECURITY.md` said twice that there are no per queue permissions, and that a
`write` key may cancel or revive a job in any queue. The named keys built the
identity that every per caller rule needs, and this is the first rule built on
it.

**Added**

- **A key may name the queues it may act on**, written after the scope with an
  at sign and joined by a plus:

      QUORRA_API_KEYS=billing:write@invoices+receipts:<secret>

  A key that names none holds every queue, so a deployment that has not
  divided anything keeps working unchanged.

  The queues go in the scope field and not a fourth one, so that the secret
  stays the whole of the rest of the entry. A secret holding a colon is
  written correctly today, and a fourth field would take that away. A queue
  whose name holds `:`, `,`, `@` or `+` cannot be named in a key, and the
  server says so at startup.

- **A limited key sees only what is in its queues.** A listing, the queue
  counts, the worker list, one job by identifier, and both bulk actions.

  `404` on the read side and `403` on the write side. A caller that names a
  queue to write to already knows the name it asked for, so there is nothing
  to hide. A caller holding a job identifier learns nothing from `404` and
  would learn from `403` that the job exists.

- **A worker key may be limited too.** Without it a worker key opened every
  queue, so a fleet run by one team could lease and finish another team's
  work. A watch on a queue the key does not hold is refused rather than
  quietly narrowed, because a worker whose watch was narrowed would wait for a
  hint that never comes and never learn why.

- **`whoami` and `quorractl whoami` say which queues a key holds.** A key that
  cannot reach a queue and does not know it reads an empty listing as an empty
  queue.

  Measured against a live server, `billing` holding two queues of three
  against `ops` holding every queue: `billing` listed 7 jobs in 2 queues where
  `ops` listed 10 in 3, counted 2 queues against 3, was answered `404` for a
  job in the third, and stopped 7 jobs with a filter naming no queue while the
  third queue kept all of its own. A worker key limited to `invoices` leased
  from it and was refused `payroll` and the default queue.

**Changed**

- **A filter may name several queues.** `Queues` narrows on top of `Queue`
  rather than replacing it, because what a caller asked for and what a caller
  is allowed are different questions. Both stores turn a filter into a
  condition in one place, so the bulk actions narrow with it. The contract
  suite holds a hundred and ten rules now.

### The two days the clock changes

**Fixed**

- **A repeat schedule fired twice on the day the clock goes back, and not at
  all on the day it goes forward.** The walk that finds the next firing added
  an hour to an instant and read the wall clock off the answer, which is the
  same thing only in a zone that never changes its clock.

  Measured before the fix, `0 2 * * *` in `Europe/Berlin`: it answered 25
  October 2026 02:00 twice, an hour apart, and it stepped from 28 March to 30
  March. Two windows means two jobs, and the idempotency key cannot stop them,
  because they are two different windows. A daily invoice run billed twice.

  The walk now goes over wall clock readings, which is what a cron rule is
  about, and converts once at the end.

  Two answers are chosen rather than inherited, and both are written down. A
  reading that happens twice names the first of the two, which keeps a daily
  schedule twenty four hours from the day before. A reading that does not
  happen names the first moment after the gap.

  Measured after, on a real server and PostgreSQL 16, a daily schedule
  catching up with `skip`: `Europe/Berlin` from 28 March to 3 September 2026
  walked 159 windows against 158 before, and `Pacific/Auckland` from 4 April
  to 3 September 2026 walked 152 against 153. One window each way.

  The store contract suite gains a rule, so both stores agree that a schedule
  marked at a reading which happens twice does not fire again. It holds a
  hundred and nine rules now.

## 1.0.0 - 2026-08-27

The first release. Everything below it happened before there was a version to
put it under, which is why one version holds a rebuild and seven arcs of work.

**What the number promises.** The JSON the HTTP API speaks, the protocol
buffer definitions the worker protocol is built from, and the exported names
in `client`, `worker` and `worker/pgtx` are the surface a caller depends on.
A change that breaks any of them moves the major number.

**What it does not promise.** Everything under `internal/` is free to change,
including the database schema, which is reached through migrations and not by
reading tables from outside. The delivery guarantee is unchanged and is not a
version question: this queue delivers at least once, and a handler has to be
safe to run twice.

### Seeing inside the worker protocol

**Added**

- **A job type dimension.** `quorra_jobs_finished_total{type,status}` and
  `quorra_job_type_lifetime_seconds{type}`. Which queue is failing is a
  question about how the work is arranged, and which type is failing is a
  question about the work, which nothing here could answer.

  Job type is the first label in this server that a caller fills in, so it is
  the first that needs a bound. Fifty types keep a row of their own and
  everything after that is counted as `other`, which undercounts nothing: a
  sum over the label is still every job. `quorra_job_types_tracked` sitting at
  fifty is how an operator finds out that `other` is holding more than it
  looks.

  Driven with sixty three types against a live server: fifty one rows, a
  tracked count of fifty, thirteen under `other`, and a sum over the label of
  ninety against sixty eight dead and twenty two succeeded.

  New metrics rather than a `type` label on the counters that already exist. A
  label added to a counter makes every panel and alert that reads it start
  summing over a dimension it does not know about.
- **The worker protocol is timed.** `quorra_grpc_request_duration_seconds` and
  `quorra_grpc_streams_total`. Every job is leased and reported over gRPC and
  that whole path was untimed, so the one number an operator had covered the
  half of the traffic that does not run the work.

  Measured on a live server: 610 `Lease` calls at `OK` and 25 at
  `Unauthenticated`, 90 `Report` calls of which 88 answered inside 5ms, and 6
  `Watch` streams, 5 of them refused.

  Streams are counted and not timed, because a watch lives as long as the
  worker does. A call the guard refused is timed like any other.
- **A request identifier.** Every answer carries `X-Request-Id`, over HTTP and
  over the worker protocol, and every line the server wrote while making that
  answer names it. A caller that sends its own keeps it, so both sides quote
  one string.

  What a caller sends is refused rather than trimmed: over 64 characters, or
  anything outside printable ASCII. A log line is a line, so a value with a
  newline in it would write a line of the caller's own choosing into the log
  of the server.

- **No binary could say which build it was.** The Dockerfile has passed
  `-X main.version` since the first image and no such variable existed, so the
  flag did nothing. Go accepts `-X` for a symbol that is not there without a
  word. The server and the worker now say it in the first line of their log,
  and `quorractl version` answers.
- **The smoke test could not read a counter that has labels.** It matched a
  bare name against a bare series, so the day `quorra_jobs_cancelled_total`
  gained a `caller` label it reported that a counter which had moved to 2 was
  not published at all. The stack job in CI failed on that line for six
  arcs of work, and the local checks never ran the script.
- **A refused request leaves a line.** Info for a 4xx and Warn for a 5xx,
  naming the request, the route and the code. Refusals only: a queue answering
  normally is the normal case, and a line for each would bury the ones that
  matter.

  This was found by driving the feature rather than by testing it. `quorractl`
  printed an identifier for a job that was not there, and searching the log of
  the server for that string found nothing, because the only line written for
  a 404 was no line at all.

**Changed**

- **The lease line names the jobs it handed over.** It said how many and never
  which, so a reader with a job that went missing had the line that accepted
  it and the line that reported on it, and nothing in between.
- **A client and `quorractl` name the request in a refusal.** The identifier
  is the one string that finds every line the server wrote while it was
  refusing, and a caller should not have to know that such a thing exists to
  end up holding it.

### A side effect that happens once

`docs/milestones.md` parked exactly once delivery and named one thing that
would be worth building: a way for a handler to be handed the transaction its
report commits in. That is built. The queue still delivers at least once, and
this changes nothing about that claim.

**Added**

- **`worker/pgtx`.** A handler is given a `pgx.Tx` on the database the queue
  is in, and the outcome of the job is recorded in that same transaction. The
  handler's writes and the record of the job commit together or neither does,
  so a side effect in that database happens effectively once.

  **It is not exactly once.** A handler that calls another service, writes to
  another database, or sends an email has the window it always had and has to
  survive running twice. What this removes is that window for one case, by
  turning two writes into one write.

  Measured against a live server. Five jobs whose handler wrote its row and
  then failed once wrote ten rows through an ordinary handler and five through
  a transaction one. Over fifty jobs that succeeded first time, an attempt
  took a median of 2.70ms through the transaction against 3.51ms through an
  ordinary report.

  Its own package, because a handler there takes a `pgx.Tx`. A consumer that
  only submits and runs jobs should not start compiling a database driver.
- **`worker.ErrAlreadyReported`.** A handler that recorded the outcome of its
  own job returns it, and the worker sends no report. Without it the second
  report is refused, because the row no longer carries the lease the worker
  holds, and the refusal reads in the log as a fault when nothing is wrong.

  `worker.Job.LeaseID` comes with it, for the same one caller. The field stays
  unexported: a handler that could read it by accident could report on its own
  job behind the worker's back.

**Fixed**

- **Two test suites emptied each other's tables.** The store contract suite
  and `worker/pgtx` both test against one real database, and go test runs
  packages at the same time. Every test that wants the database now takes one
  advisory lock, in `internal/pgtest`, so the two suites wait for each other.

  The lock and not a flag on the go test command, because a flag helps only
  the command that carries it and CI runs go test directly. Both suites still
  run at the same time: 14.3 seconds for the whole suite with the lock,
  against 41.4 seconds with `-p 1`.

### The three the record was waiting for

`docs/milestones.md` parked three features behind written conditions. All
three were asked for, and each entry now records that the gate was met by
request rather than by the condition it named, so the file keeps meaning what
it says.

**Added**

- **A worker presents a key.** The gRPC port had no authentication at all: a
  process that could reach it could lease from any queue. It takes a key in
  the call metadata now, checked on both the unary and the streaming path.

  A worker key holds the `worker` scope and nothing else. That is why the
  permissions stopped being an ordered number and became a set: leasing work
  off the queue is not more than changing a job, so a key an operator keeps in
  a shell profile cannot lease the queue empty and a worker cannot cancel
  anything.

  `QUORRA_API_KEY` now means one key that may do everything, so a deployment
  that sets one key keeps working. A deployment that sets `QUORRA_API_KEYS`
  needs a `worker` entry, and its workers need `QUORRA_API_KEY` set to that
  entry's secret.
- **Repeat schedules.** `POST /v1/schedules`, `client` and
  `quorractl schedule`. A schedule holds a five field rule, an IANA time zone
  and a catch up policy, produces jobs, and is never handed to a worker.

  `catch_up` is required and has no default, because the record called it the
  part everybody forgets and then argues about. Measured against a real server
  with a seventy two hour outage on three hourly schedules: `all` produced 72
  jobs, `skip` produced 1 and logged 71 dropped, and `none` produced 0 and
  logged 72 dropped.

  The cron parser is written here rather than taken from a library: this
  project has five direct dependencies and the standard library has no cron
  parser at all.
- **A job does not wait out a poll.** The server holds a stream open for every
  worker and sends queue names down it, and the worker waits on whichever
  comes first, the hint or its own poll.

  Measured, submitted to leased, over ten jobs with a five second poll: a
  median of 11ms. The same measurement before this, with a five hundred
  millisecond poll, was 207ms.

  A hint that is lost costs one poll interval, which is what makes this safe
  to add to a protocol whose correctness already worked without it. A job that
  becomes ready later, with a delay or after a backoff, sends no hint and is
  found by the poll.

**Fixed**

- **A schedule reported the wrong number of windows it missed.** The walk
  stopped at the bound on what is kept, so the count came back as the overflow
  of that bound. A ten day outage of an hourly schedule produced the right
  hundred jobs and reported one missed window instead of a hundred and forty.
  Found by driving the outage against a running server.
- **Every background loop refuses an interval it cannot run on.**
  `time.NewTicker` panics on a zero, and a panic in a goroutine takes the whole
  process with it.

### Reach

Recovering a large dead letter queue was a scripting exercise: no bulk revive,
and a dashboard table hard capped at twenty five rows whose note told the
reader to go and use a different tool. The two halves ship together, because
either one alone leaves the exercise.

**Added**

- **Bulk cancel and bulk revive.** `POST /v1/jobs/cancel` and
  `POST /v1/jobs/revive` take the fields the listing takes, so an operator
  narrows a listing until it shows what they mean and sends the same
  narrowing. `client.CancelMatching`, `client.ReviveMatching`, and
  `quorractl cancel -all` and `revive -all` call them.

  Measured against PostgreSQL: five hundred dead jobs revived in 513ms in
  one command, against about five seconds for the same five hundred one
  request each.

  The limit is required and there is no default, because a default would make
  the most dangerous request in this API the shortest one to write. A bulk
  action with no filter at all is refused by the tool for the same reason.
- **Bulk submit.** `POST /v1/jobs/bulk`, `client.SubmitMany` and
  `quorractl create -file`, which reads one JSON object per line. Five
  hundred jobs in 523ms.

  Each job is answered for on its own, and one bad row does not lose the
  others. Jobs are independent, so one transaction for the batch is the wrong
  shape.
- **The dashboard reads past the first page.** A button that follows the
  cursor. Every page the reader has asked for is fetched again on each
  refresh, so the rows they were reading do not disappear under them every
  five seconds.
- **One job can be opened in the dashboard**, with its payload, the facts a
  row has no room for, and every finished run of it. A search box takes an
  identifier, which is the path an operator holding one from a log line
  takes.

**Fixed**

- **The dashboard polled for ever.** `docs/milestones.md` recorded three
  faults here and left all three: a wrong key made two failing requests every
  five seconds for as long as the tab was open, a tab left in the background
  polled until it was closed, and a server that had gone away was asked at the
  same rate for ever.

  The wait doubles on a failure to a ceiling of a minute, six failures in a
  row stop it, and a hidden tab is asked for nothing. Measured over the
  browser: with a wrong key, two requests in twenty one and a half seconds
  rather than eight. Hidden for thirteen seconds, none at all.

### The schema learns three things

The jobs table held one row per job and nothing else. That is three tables
that do not exist, not three features: what each run of a job did, which
workers are out there, and which job waits for which.

**Added**

- **One row for each finished run.** `GET /v1/jobs/{id}/attempts`,
  `client.Attempts` and `quorractl history`. Each row names the worker, the
  outcome, what went wrong and how long the run took. A job that failed four
  times and then worked carried one error before this, from whichever attempt
  wrote last.

  The row is written where the job is retired, so a worker reporting and a
  lease running out write the same history through one path, and it commits
  in the same transaction as the job. Nothing is written when a run starts: a
  running attempt is already fully visible on the job, and an insert there
  would be a second write on the busiest statement in the system.

  The job gains `leased_at`, so a run has a start to measure from and
  somebody looking for a job that is stuck can see how long it has been
  going.
- **The workers the queue has heard from.** `GET /v1/workers`,
  `client.Workers` and `quorractl workers`. A row is written where a worker
  asks for work, and whether or not any job comes back: `leased_by` is
  cleared when a job ends, so a fleet with nothing to do left no trace, and
  an empty queue looked exactly like a fleet that had stopped.

  Workers nobody has seen for `QUORRA_WORKER_RETENTION` are removed, a day
  by default. A worker identifier is usually the name of a container, so a
  deployment retires every row and writes a new set.
- **A job that runs after another job.** The `after` field on a submission,
  `NewJob.After` and `quorractl create -after`. A job runs when every job it
  waits for has succeeded.

  It sits in a new state, `blocked`, and not in `pending`. Pending is a claim
  that the queue will hand the job out. Holding it back with a run time far
  in the future was the alternative and lies in a worse way: the job then
  says it runs in the year nine thousand.

  A parent that will never succeed cancels what waits for it, naming the job
  that stopped it. Cancelled and not dead, because dead means the job used
  every attempt it had and a waiting job used none.

  There is no cycle check. A job may only wait for a job that already exists,
  and a job cannot be created before itself, so the graph is acyclic by
  construction.

**Fixed**

- **A waiting job had no cancel button on the dashboard.** The buttons were
  decided from a list of the states that can be cancelled, and the list went
  stale on the first state added after it. Found by loading the page against
  a real server. The page reads the states a job never leaves now, so the
  next state added needs no edit there.
- **quorractl printed the word nil in the error column of a run that
  worked.** The field is absent on a run that finished, and printing the
  missing value put a word that reads as an error nobody can look up on every
  one of those rows.

### A caller has a name

One shared secret guarded every route, so the queue could count that forty
jobs had been cancelled and could not say by whom. That is the answer that
matters on a queue two teams share, and every kind of per caller rule needs
the same thing first.

**Added**

- **Named API keys with scopes.** `QUORRA_API_KEYS` holds a comma separated
  list of `name:scope:secret`. The scope is `read` or `write`. A read key
  gets 403 from a route that changes a job, and not 401: the key is real and
  the server knows whose it is, and 401 sends somebody to check a key that is
  working correctly. `QUORRA_API_KEY` still works and means one key named
  `default` that may write.

  The keys come from configuration and not from a table. A table needs a
  bootstrapping story for the first key and a rotation story before it is
  worth anything, and neither is worth buying at this size.

  Names tell services apart, not people. There is no user model here, and a
  key four people on a team share is a key that names the team.
- **`GET /v1/whoami`** answers the name and the scope of the key that asked.
  It needs read, because a key that changes nothing still has to be able to
  ask what it is. `quorractl whoami` and `client.Whoami` call it.
- **A job records who cancelled or revived it.** `acted_by` and `acted_at`
  name the key that acted last and the moment it acted. A job nobody has
  acted on carries neither. Only cancel and revive set them: the queue
  leasing, retrying or burying a job is not a person acting.

  The pair holds the last action and not a history. A caller that names
  nobody clears it, because keeping the name before would say that ops
  cancelled a job that ops did not cancel.
- **The two action counters carry the caller.**
  `quorra_jobs_cancelled_total` and `quorra_jobs_revived_total` are labelled
  by the key that asked. The label needs no bound: the names come from the
  server configuration and not from a request.

### Towards a release

**Fixed**

- **A number the column could not hold was the server's fault.** Priority and
  max retries are a Go int against INTEGER columns, so a value between the two
  sizes passed the validator and was refused by PostgreSQL with a message that
  did not read as the client's mistake. The API answered 500. It answers 400
  now and names the field and the range.
- **The two stores disagreed about the same submission.** The in-memory store
  has no such column, so it stored what PostgreSQL refused. Measured before
  the fix: priority 3000000000 gave 500 against PostgreSQL and 201 against
  memory, while all sixty five contract rules passed. The suite now holds a
  rule that both stores refuse it, and refuse it with a message that says
  which package refused it.

### Saying no, and finding what is stuck

Two things a person running this hits on the first bad day: a handler with no
way to say that a job will never work, and no way to tell a job that is ready
from one that is waiting out a backoff.

**Added**

- **A handler can refuse a retry.** An error wrapping `worker.ErrPermanent`,
  or one built with `worker.Permanent`, buries the job on that attempt
  whatever its retry count. A payload that names no account does not name one
  on the third attempt, and spending three more workers and three backoffs to
  reach the answer the handler already gave is waste. The producer could ask
  for this with `max_retries: 0`, and that is the wrong actor at the wrong
  moment: the producer does not know which failures are permanent.
- **The job ends in `dead`**, with the reason on the row, and not in a status
  of its own. Two statuses that described how a job arrived somewhere rather
  than where it was have already been removed from this project.
- **`quorra_jobs_refused_total`**, which is a part of `quorra_jobs_dead_total`
  and not a number beside it. A dead letter queue filling with refusals says
  the work being submitted is wrong; one filling with exhausted attempts says
  something outside is down. Those need different people.
- **A listing can be ordered by when a job runs**, with `order=soonest`, and
  narrowed by `worker` and by `due`. `due=now` is resolved by the server
  against its own clock, so the answer does not depend on two machines
  agreeing about the time.
- **Paging holds under the new order.** The cursor compares the pair of
  `run_at` and the row sequence. `run_at` is not unique: a burst of
  submissions shares one value and every job a reclaim sweep returns shares
  one, so a cursor on it alone would repeat rows or skip them. Written as a
  row comparison because PostgreSQL turns that into an index condition and
  seeks, which the `OR` it stands for cannot do.
- **An index for that order**, `jobs_due_idx` on `(run_at, seq)`. Measured on
  200000 rows: 3259 buffers and 36ms without it, 5 buffers and 0.12ms with it.
- **`quorractl list -ready`, `-soonest`, `-worker` and `-before`**, with a
  `RUNS AT` column when the answer depends on when a job runs.
- **A `ready` filter and a `Runs at` column on the dashboard.** Three pending
  jobs, one ready and two waiting, were identical in every column on that
  page. They now read "now", "in 54s" and "in 2h".

**Fixed**

- **The command line tool named an option it refused.** The last page of a
  listing printed "add -all, or -before <id>", and `-before` was never
  registered, so following the instruction answered "flag provided but not
  defined".
- **The dashboard showed a stale error instead of a result.** `last_error` is
  kept when a job succeeds, on purpose, and the Outcome cell read that field
  first. So a job that failed once and then worked displayed its old failure
  and hid what it produced. The status decides now.
- **The error a worker reports had no size limit** while the result had one
  from the start, although the error is the field that shows in every listing
  that touches the row. It is cut at two thousand characters and says that it
  was cut. Cut and not refused: refusing the report would throw away the
  outcome as well as the message and leave the job leased until the reclaimer
  took it back.
- **The worker package promised a deadline it did not set.** The
  documentation on `LeaseExpiresAt` said the handler's context ended at that
  moment. Nothing set one, and setting one would have been wrong, because the
  heartbeat pushes the lease out while the handler runs. The comment was the
  wrong side.
- **One layer decided by matching the text of another layer's error.** The
  gRPC service searched the store's message for "not JSON" to answer 400
  rather than 500, so rewording one sentence in another package would have
  silently moved every one of those answers and pointed the reader at the
  server for a mistake the worker made. It reads `store.ErrNotJSON` now.

**Changed**

- **`store.Filter` carries an `Order`**, and `Newest` is its zero value, so
  every caller that existed keeps getting what it got.
- **`metrics.JobFinished` takes the outcome** as well as the job. It reads it
  only to divide a status, never to decide one.
- **`api.Options` takes a clock**, like the store and the gRPC service, so one
  route can resolve `due=now` without the store reading one.

### Acting on the queue

The rebuild made the queue correct. This makes it usable: a dead letter queue
you can act on, a way to find one job among a month of them, a lease a slow
job can hold, a submission that is safe to repeat, a table that does not grow
for ever, somewhere to put what a job produced, and a package for the half of
the system that submits work.

**Added**

- **Cancel and revive.** A job that has not finished can be stopped, and a
  dead or cancelled one can be put back with the attempt count set to zero.
  The zero is the point: somebody clearing a dead letter queue has usually
  just fixed the thing that broke, and leaving the count where it was gives
  the job one more try and sends it straight back.
- **A cancelled status**, separate from dead. Both are endings that are not a
  success, and the difference between them is the only thing that says whether
  the queue gave up or somebody decided.
- **Filtering and paging.** `GET /v1/jobs` narrows by queue, status and type,
  and pages with a cursor rather than an offset. An offset re-reads and skips
  the rows before the page, so a job submitted while somebody is reading
  shifts every later page by one, showing a row twice and hiding another.
- **A heartbeat.** A running job asks for its lease to be pushed out three
  times per lease, so a handler slower than its lease is no longer taken away
  mid-flight. When the answer says the job is no longer this worker's, the
  handler's context is cancelled with `worker.ErrLeaseLost` as its cause,
  which is how a cancellation reaches a handler that is already running.
- **An idempotency key**, in the body or the `Idempotency-Key` header. A
  repeated submission gives back the first job and answers 200 rather than
  201. The check is `ON CONFLICT` and not a read followed by a write, because
  two submissions carrying one key arriving together is the case it exists
  for.
- **Retention.** Finished jobs can be removed once they are old enough, per
  status, in bounded batches. Every setting defaults to keeping the job for
  ever, because a queue holds the only record that a piece of work happened.
- **A result.** A handler registered with `HandleResult` returns a value as
  well as an error, and the value is stored on the job and served back. The
  server bounds it and refuses one that is too large rather than trimming it.
- **A client package.** The other half of the worker package. A refusal
  arrives as `ErrNotFound`, `ErrWrongState` or `ErrUnauthorized`, and `Each`
  walks every page for a caller who wants all of something.
- **A migration runner.** Every file in `migrations/` is applied in name
  order. Two rules are tested rather than trusted: a four digit prefix, so
  that 10 does not sort before 9, and IF NOT EXISTS on every CREATE, so that
  a second apply is not an outage.
- **Dashboard filters and actions.** A row of status buttons, and a cancel or
  revive button on each job. Only the actions the server would accept are
  offered, because a button that always answers 409 teaches the reader to
  ignore the row.
- **`quorractl cancel`, `quorractl revive`**, filters on `list`, and `-all` to
  follow the pages. The tool has tests for the first time.

**Fixed**

- **The command line tool refused an option that came after an argument.** The
  flag package stops parsing at the first token that is not an option, so
  `quorractl get 6f1c0c64 -server http://elsewhere` read the address as a
  second job identifier. Every other tool on the machine takes them in either
  order.
- **The dashboard pushed its action buttons off the side of the table**, under
  a created column holding the full date and seconds of every row and an error
  column with nothing capping it.
- **The dashboard listed a result under a heading that says a fault.** The
  result of a succeeded job and the error of a failed one share a cell,
  because a job has one or the other and never both. The heading said Last
  error, so a job that finished correctly carried the word "result"
  underneath it. The heading is now Outcome.

**Changed**

- **`Recent` became `List` with a filter.** One method instead of two, and the
  narrow one was the special case.
- **`Create` reports whether it stored anything**, so the HTTP layer can
  answer 200 or 201.

### The rebuild

The project was rebuilt from an empty tree.
The history before it is on the `archive/pre-rebuild` branch.

The reason is short: it did not compile, and it could not have worked if it
had.

**Measured on the version that was replaced**

| Finding | Evidence |
| ------- | -------- |
| The module did not build | `go build ./...` stopped at `malformed go.sum:1: wrong number of fields 10`. The file held three lines of English saying it "would be generated". |
| It would not have built after that | `internal/queue/manager.go:121` ranged into an empty body, so a variable was declared and not used. |
| gRPC could never have worked | `internal/grpc/quorra.pb.go` was headed "Code generated by protoc-gen-go. DO NOT EDIT." and was written by hand. Its messages were plain structs rather than protobuf messages, so the codec refuses every call at run time. |
| The tests could not fail | Every case skipped itself when PostgreSQL was missing, so `go test ./...` reported success having run nothing. |
| The CI badge was a picture | The README carried a static `CI-passing` image, not a workflow badge. |
| Redis did nothing | The server published to a channel on every submission. Nothing subscribed. |

**Added**

- **Leases are reclaimed.** Every lease carries an expiry, and a loop in the
  server returns expired ones to the queue through the same decision a
  reported failure goes through. Before this, a job leased by a process that
  lost power stayed leased for as long as the table lived. Measured: a worker
  stopped with `SIGKILL` while holding a job had that job back in the queue
  ten seconds later, with the row naming the worker that died.
- **A store contract suite.** Twenty four rules that both the in-memory store
  and the PostgreSQL store must pass, including one that runs eight goroutines
  over forty jobs and fails if any job is handed out twice.
- **An in-memory store**, so that the suite, the API tests and a first look at
  the server need nothing installed. 103 of the 128 cases run with no
  database.
- **A worker package other projects can import.** It mentions neither gRPC nor
  protobuf. A panic in a handler costs one job rather than the process, a
  handler's context ends when its lease does, and a stopping worker finishes
  what it is running and reports it.
- **`readyz`**, which reaches the store, beside `healthz`, which does not.
- **`deployments/k8s`**, which the old README listed and which did not exist.
- **A generated code check** in CI, which regenerates the protocol and fails
  on a difference.

**Changed**

- **`max_retries` counts retries.** A job with three retries now runs four
  times. The old comparison gave it three, so a caller asking for three
  retries received two.
- **Four states instead of six.** `processing` and `failed` were documented
  and never written by any code. A test refuses both by name.
- **One `Report` call instead of `AckJob` and `NackJob`.** The old messages
  carried a `success` field that nothing read, so a worker that set it to
  false and called `AckJob` was recorded as having succeeded. The zero value
  of the outcome is `UNSPECIFIED` and the server refuses it, because zero is
  what an older client sends.
- **`Lease` is one call and one answer.** It was a server stream that sent
  whatever was ready and closed, which is a single answer costing the client a
  receive loop.
- **Every timestamp column carries a zone.** They were `TIMESTAMP`, which
  drops it, so a server outside UTC wrote local times into the column that
  decides whether a job is ready.
- **The backoff carries jitter and cannot overflow.** Shifting by the attempt
  count turns negative at 63, which puts a job in the past and makes the queue
  spin on it for ever.
- **The module path is `github.com/Stiven-Gjekaj/GoQuorra`.** It said
  `github.com/goquorra/goquorra`, which does not resolve.

**Fixed**

- **The dashboard held the API key in its own source** and sent it in the
  query string of every request. Anybody who could open the page could read
  the key that guarded the whole API, and a query string is written to the
  access log of every proxy, kept in browser history, and sent on in the
  `Referer` header.
- **The dashboard turned a job type into markup.** It built each row by
  joining strings and putting the result into the page. The job type and the
  queue name are chosen by whoever submits a job. Every value is now written
  as text, the page carries a content policy with a fresh nonce per request,
  and a test refuses the dangerous calls by name.
- **The API key was compared with `!=`**, which returns as soon as two bytes
  differ and so reports how much of the key was right.
- **There was no limit on a request body.**
- **The configuration reader invented numbers.** It walked the characters of a
  value and kept the digits, so `-5` meant five, `1o` meant ten, and `five`
  meant the default. Every one of those started a server under a setting
  nobody chose.
- **The API key had a default**, and that same string was in the README, the
  example file and the compose stack.
- **`quorra_jobs_dead_total` was never raised.** It was declared, documented,
  and suggested as a dashboard panel. It read zero for ever.
- **One counter served both a retry and a burial**, so the failure rate
  counted a buried job twice and the burial not at all.
- **`quorra_queue_length` was never set.**
- **Every failure from the store became a 404**, so a database that had fallen
  over was reported to the client as a missing job.
- **Asking for zero retries gave three.** The field was a plain integer, where
  zero and absent are the same value.
- **The worker abandoned jobs in flight.** It passed `context.Background()` to
  every job and then closed the connection, so a job being run when the worker
  stopped was left with its lease held.
- **The worker ran a simulator.** It slept for a random time and returned
  success nine times out of ten, for every job type it was given, so a type no
  code knew about was reported as done.
- **The server called `log.Fatalf` from inside two goroutines**, which ends
  the process without running a single deferred close.
- **The HTTP server had no read timeouts.**
- **`MoveToReady` set `pending` where the status was already `pending`.**

**Removed**

- **Redis.** The server published to a channel that nothing subscribed to.
- **`chi`.** Go 1.22 gave `net/http` the method and wildcard patterns it was
  there for.
- **`cobra`.** The command line tool is four verbs, and the standard library
  covers that in less code than the dependency.
- **The `queue_stats` view and the `updated_at` trigger.** The view was a
  second place for the shape of an answer to live. The trigger overwrote the
  timestamp the program had just written, with a time from a different clock.
