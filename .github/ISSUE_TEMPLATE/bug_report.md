---
name: Bug report
about: Report something that does not work as expected
title: ""
labels: bug
assignees: ""
---

## What happened

A clear description of the problem.

## What you expected

## How to reproduce

The smallest set of steps that shows it. A job payload that causes it helps,
with anything private taken out.

## Which part

- [ ] The HTTP API
- [ ] The worker protocol or the `worker` package
- [ ] The dashboard
- [ ] Storage, or a job ending up in the wrong state
- [ ] The command line tool
- [ ] Something else

## Your setup

- GoQuorra commit:
- Go version:
- Store (`postgres` or `memory`), and the PostgreSQL version if it applies:
- How it runs (compose, Kubernetes, a binary):

## The server log

The log is JSON, one object per line, and it names the job.
Set `QUORRA_LOG_LEVEL=debug` and reproduce it again, then paste the lines
around the problem.

## The counters

    curl -s localhost:8080/metrics | grep quorra_

Paste the output. Most problems in this project show up here first:
`quorra_leases_reclaimed_total` rising means workers are dying or running past
their lease, and `quorra_jobs_dead_total` rising means handlers are failing.

## The job

If one job is wrong, paste its row:

    curl -s localhost:8080/v1/jobs/<id> -H "X-API-Key: <key>"

Take the payload out if it holds anything private.
