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
