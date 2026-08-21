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
  <a href="https://github.com/Stiven-Gjekaj/GoQuorra/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/Stiven-Gjekaj/GoQuorra/ci.yml?label=ci&style=flat-square" alt="CI"/></a>
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
    pending --> leased: a worker takes it
    leased --> succeeded: the worker reports success
    leased --> pending: the worker reports a failure, and attempts remain
    leased --> dead: the worker reports a failure, and none remain
    leased --> pending: the lease runs out, and attempts remain
    leased --> dead: the lease runs out, and none remain
    succeeded --> [*]
    dead --> [*]
```

There are four states, and there used to be six.
`processing` is gone because the server cannot observe it: the worker holds
the job between the lease and the report, and the server hears nothing in that
window.
`failed` is gone because it was never distinguishable from `pending`.
Both were documented, and no code ever wrote either.

| State | Meaning |
| ----- | ------- |
| `pending` | Waiting. Ready once `run_at` has passed. A delayed job and a job serving a backoff both sit here. |
| `leased` | A worker holds it. The lease carries an expiry. |
| `succeeded` | A worker reported that it is done. |
| `dead` | Every attempt was used. The row stays, with the last error on it. |

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

`cmd/quorra-worker` is a demonstration with three handlers: `echo`, `sleep`,
and `fail`. `fail` exists so that the retry schedule and the dead letter queue
can be watched happening.

---

## The HTTP API

Every route under `/v1` needs the `X-API-Key` header.
The key is not accepted in the query string, because a query string is written
to the access log of every proxy in front of the server, kept in browser
history, and sent on in the `Referer` header.

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
`max_retries` of `0` means no retries, and is not the same as leaving it out.
A field the server does not know is refused rather than ignored, so
`maxRetries` gets an error instead of quietly doing nothing.

Answers `201` with `{"id", "status", "queue", "run_at"}` and a `Location`
header.

### The rest

| Route | Gives |
| ----- | ----- |
| `GET /v1/jobs/{id}` | One job. `404` when there is none, and `500` when the database is unreachable. |
| `GET /v1/jobs?limit=50` | The newest jobs first. |
| `GET /v1/queues` | A count for each queue and status. |
| `GET /healthz` | `200` while the process is running. Public. |
| `GET /readyz` | `200` while the store can be reached. Public. |
| `GET /metrics` | Prometheus. Public. |
| `GET /` | The dashboard. |

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
| `quorra_jobs_dead_total` | counter | Jobs that used every attempt |
| `quorra_leases_reclaimed_total` | counter | Leases taken back after they ran out |
| `quorra_queue_length{queue,status}` | gauge | Jobs in each queue, refreshed on a timer |
| `quorra_job_lifetime_seconds{queue,status}` | histogram | Acceptance to final state, so the waiting and the retries are in it |
| `quorra_http_request_duration_seconds` | histogram | Labelled by route pattern, not by path |

A retry and a burial are separate counters.
The previous version raised one counter for both, so the failure rate counted
a buried job twice, and `quorra_jobs_dead_total` was declared and never raised
by anything.

---

## Project structure

Two ideas carry the design.

**The deciding part is pure.**
[`internal/jobs`](internal/jobs) holds the rules a job follows.
It imports nothing outside the standard library, holds no database handle, and
reads no clock.
A table test drives every state a job reaches with nothing installed.

**Two stores answer to one suite.**
[`internal/store/storetest`](internal/store/storetest) holds twenty four
rules.
The in-memory store passes them with nothing installed and the PostgreSQL
store passes them against a real database.
A store with its own private tests agrees only with itself, and then it stands
in for a database it does not behave like.

| Area | Lines | Holds |
| ---- | ----- | ----- |
| `internal/jobs` | 234 | The states, the retry decision, the backoff. Standard library only. |
| `internal/store` | 265 | The interface, the errors, the defaults |
| `internal/store/memory` | 330 | Jobs in a map |
| `internal/store/postgres` | 532 | Jobs in PostgreSQL |
| `internal/api` | 488 | The REST routes and the dashboard |
| `internal/rpc` | 192 | The worker protocol |
| `internal/server` | 281 | Assembly, the reclaim loop, shutdown |
| `internal/config` | 294 | Reading the environment |
| `internal/metrics` | 176 | What the server publishes |
| `worker` | 456 | The package other projects import |
| `cmd` | 472 | Three binaries |
| **Total** | **3738** | 42 Go files, not counting 3187 lines of tests or 788 of generated code |

```
proto/quorra/v1/       the worker protocol
internal/quorrapb/     generated from it, and checked by CI
migrations/            the schema, embedded so the tests apply the same bytes
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

Seventy three modules in the whole graph, most of them beneath those five.

Two dependencies the previous version carried are gone.
`chi` went because Go 1.22 gave `net/http` the method and wildcard patterns it
was there for.
`cobra` went because `quorractl` is four verbs with a handful of options each,
and the standard library covers that in less code than the dependency.

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

**128 cases pass. 103 of them need nothing installed.**

The 25 that do are the store contract suite against PostgreSQL:

```
export QUORRA_TEST_DATABASE_URL="postgres://quorra:quorra@localhost:5432/quorra_test?sslmode=disable"
make test-postgres
```

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
