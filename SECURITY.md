<div align="center">
  <a href="README.md"><b>GoQuorra</b></a>
</div>

# Security Policy

## Supported versions

GoQuorra has no release yet.
Fixes go to the default branch.

## Reporting a vulnerability

Report security problems privately, and not through a public issue.

- Preferred: open a private security advisory with the "Report a
  vulnerability" button on the Security tab of this repository.
- Alternative: email the maintainer at stivenagostingjekaj@gmail.com.

Include the steps to reproduce, the affected commit, and the impact as you
understand it.
You can expect a first answer within a few days.

## The threat model

GoQuorra is a server that holds work on behalf of other programs.
Read this before you report, because two classes of finding are decisions
rather than defects.

**An API key is the whole of the authentication.**
There are no accounts.
A key has a name, a scope and the queues it may act on.
A `read` key asks questions and changes nothing.
A `write` key submits work, cancels a job and revives one.
A `worker` key leases jobs and reports on them, and does nothing else.

A key may be limited to queues, and a key that names none holds every queue.
A limited key does not see what is in another queue, does not count it, and is
answered 404 for a job in one, the same as for a job that is not there.
It is answered 403 when it names another queue to write to, because it already
knows the name it asked for and there is nothing to hide.

Run it inside a network you control, and treat a key the way you treat a
database password.

A key names a service, not a person.
`acted_by` on a job is the name of the key that acted, and a key four people
on a team share records the team.
Do not read it as a record of which person did something.

The names are configuration and the secrets are compared in constant time,
folded through SHA-256 first so that a length does not leak.
A lookup compares every key rather than stopping at the first match, so the
time it takes does not say which key was presented or how many exist.

**A worker is trusted with the jobs it is given, and with nothing else.**
The gRPC port takes a key in the call metadata, and refuses every call
without one.
The key has to hold the `worker` scope, which no `read` or `write` key holds:
a key an operator uses cannot lease the queue empty, and a worker cannot
cancel anything.

A worker still cannot submit work, and cannot report on a job it does not
hold: a report carries a lease identifier, and the server refuses one that
does not match, including an empty one.

The transport is not authenticated.
A key in the metadata is a bearer token, so anything that can read the
connection can replay it.
Run the gRPC port inside a network you control, or put TLS in front of it.
Mutual TLS is not built, and
[docs/milestones.md](docs/milestones.md) records what it would need that a
key does not: a story for a worker whose certificate expires while it holds
jobs.

**A payload is stored as it arrives.**
It is JSON in a column, readable by anybody with a database connection.
Nothing here encrypts it.
Do not put a secret in a payload.

**These are in scope:**

- A way to read or change a job without a key.
- A way for a `read` key to change anything, or for a key to act under
  another key's name.
- A way for a worker to report on a job it was not given, or to be given a job
  that another worker holds.
- A way for a key limited to queues to read, count, act on, or put work in a
  queue it does not hold, or to lease or watch one.
- A way to lease a job with a key that does not hold the `worker` scope, or to
  lease one with no key at all.
- Anything that makes the dashboard run script from the content of a job. The
  job type and the queue name are chosen by whoever submits, and both are
  shown on that page.
- SQL injection anywhere. Every query in this project uses parameters, so a
  finding here is a real defect.
- A way to make the server hold a connection, a goroutine, or memory without
  limit.
- A failure that leaves a job leased for ever, or that lets two workers run
  one job at the same time.

**These are out of scope:**

- That there is no user model and no rate limit. Both are named above. A key
  naming a service and not a person is a decision, not a defect.
- That a key which names no queue reaches every queue. That is what every key
  was before a key could be limited, and it is what a deployment gets when it
  does not divide anything. Limiting a key is the deployment's to do.
- That the gRPC port carries a bearer token over a connection you have not
  put TLS in front of. Named above.
- A denial of service that needs a `write` key. Somebody holding one can fill
  the queue, and that is what the key is for.
- Findings from an automated scanner with no working demonstration.
- A report that a payload is not encrypted at rest. Named above.
