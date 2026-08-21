<div align="center">
  <a href="README.md"><b>GoQuorra</b></a>
</div>

# Getting help

## Understand the project

- [README.md](README.md) says what it is, what it will not do, and how to run
  it.
- [docs/milestones.md](docs/milestones.md) holds the work that is not built,
  and the reasoning behind each decision that shapes it.
- [AGENTS.md](AGENTS.md) sets the rules for anybody who changes this
  repository, including an agent.

## Before you open an issue

Two things answer most questions faster than an issue does.

**Read the server log.**
It is JSON, one object per line, and it names the job.
`QUORRA_LOG_LEVEL=debug` adds a line for every lease and every report.

**Read the counters.**

    curl -s localhost:8080/metrics | grep quorra_

`quorra_jobs_dead_total` rising means handlers are failing.
`quorra_leases_reclaimed_total` rising means workers are dying, or are running
past their lease.
`quorra_queue_length{status="pending"}` rising means there are not enough
workers, or that they are all waiting out a backoff.

## Ask a question or report a problem

- Search the existing
  [issues](https://github.com/Stiven-Gjekaj/GoQuorra/issues) first.
- For a bug, open a bug report. Say what you expected, what happened, and what
  the log said.
- For something you want, open a feature request. Say what you are trying to
  do, not only what you want added.

Do not use the issue tracker for a security problem.
See [SECURITY.md](SECURITY.md) for how to report one privately.

## Contribute

See [CONTRIBUTING.md](CONTRIBUTING.md).
