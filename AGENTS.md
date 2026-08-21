# AGENTS.md

These are the rules for an agent that does work in this repository.
The rules apply to all changes.
They also apply to a change that you start without a request.

## Who writes a commit

- A human is the writer of each commit. An agent is not.
- Set `git config user.name` and `git config user.email` one time.
  Then do not change them.
- Do not add your name to a commit message, a pull request, or a review
  comment.
- Do not add a `Co-Authored-By` line.
- Do not add a link to your session.
- Do not add a footer that says that a tool made the text.
- Make these changes in the configuration of the tool.
  Do not remove the text manually each time.
- Reason: a commit shows that a human read the code.
  That human answers questions about the code six months later.
  An agent cannot do this.

## How to write

- Write all text for this repository in ASD-STE100 Simplified Technical
  English.
  This includes the source code, the comments, the documentation, the commit
  messages, the examples, and the pull requests.
- Use short sentences.
  Use the active voice.
  Use the present tense.
  Write one instruction in one sentence.
- Do not use an em-dash.
- Do not use an emoji.

## Commits

- Each commit has one change only.
- Split a feature into many commits.
  One commit does one step of the feature.
  Do not put a full feature into one commit.
- Reason: a reader examines one step at a time.
  A reader also removes one step and keeps the other steps.
  A commit that holds a full feature stops both of these actions.
  Six months later, a reader finds the one step that caused a fault.
  That reader cannot find it in a commit that changed twenty files.
- Put the code and its tests in the same commit.
- Put the documentation in a different commit.
- If you change a name in many files, put that change alone in one commit.
  Do not change what the code does in that commit.
- Write the subject line in the present tense.
  The subject line tells what the change does.
- Do not put a version number in the subject line.
- A commit does not change the version.
- Do not open a pull request if the human does not ask for it.

### An example of granular commits

These commits are correct.
Each commit does one step.
The steps are in the same feature group.

    feat: add the four states a job can be in
    feat: decide what happens to a job that stops running
    feat: keep the jobs in PostgreSQL

This commit is not correct.
It holds the full feature.

    feat: integrate the full queue feature

## How to work

- Run the code.
  Do not only say that the code will operate correctly.
- Run `make verify` before you say that a change is finished.
- Tell the human when the results do not agree with your statement.
- Read the open issues before you add a feature.

## Go in this repository

- Run `gofmt -s` on everything. `make fmt` does it.
- `go vet` has to pass. It is part of `make verify`.
- Run the tests with the race detector. Every part of this project answers
  several callers at once, and a data race shows up as a job that ran twice,
  months later, with nothing in the logs to explain it.
- Return an error. Do not call `log.Fatal` anywhere except in `main`, and call
  it in one place there, so that every deferred close runs first.
- Wrap an error with `%w` when the caller may want to test it, and give the
  package a sentinel error for a condition the caller has to tell apart.
  `store.ErrNotFound` exists because the layer above it has to answer 404 to a
  missing job and 500 to a broken database.
- Do not read the clock inside a function that decides something. Take the
  time as a parameter, or take a `func() time.Time`. A test then states the
  moment instead of waiting for it.
- Do not add a dependency without saying in the commit message why the
  standard library does not do the job.

## Keep the deciding part pure

- `internal/jobs` holds the rules a job follows. It imports nothing outside
  the standard library, holds no database handle, and reads no clock.
- A table test drives every state a job reaches with nothing installed. That
  is the only reason `go test ./...` says something about this project rather
  than about whether PostgreSQL is running on the machine.
- Put a new rule there. Do not put it in a SQL statement, and do not put it in
  an HTTP handler.
- Reason: the two paths that retire a job, a worker reporting a failure and a
  lease running out, have to age that job identically. They did not when they
  lived apart. One of them did not age it at all.

## What to measure

- Measure the thing that you tell the human.
  Do not measure something near it.
- Reason: a counter that is declared is not a counter that moves.
  `quorra_jobs_dead_total` existed in this project, was documented, was
  suggested as a dashboard panel, and no code anywhere raised it. It read zero
  for ever, and the panel looked healthy.
- Reason: a test that runs is not a test that can fail.
  This project reported a passing suite for a module that did not compile,
  because every test skipped itself when the database was missing.
- Give the number that you can prove.
  Do not give a number that you calculated from a part.

## What a test can hold on to

- A test asks the code a question.
  Build the state that a test needs inside the test.
- A test has to be able to fail. Put the fault back and watch it fail before
  you trust a new test.
- Assert a property, not a byte. JSONB does not return the bytes it was given:
  it reorders the keys and drops the spaces. A test that compares the bytes
  passes against the in-memory store and fails against PostgreSQL, for a
  reason that has nothing to do with either being wrong.
- A store test goes in `internal/store/storetest`, so that both stores answer
  to it. A store with its own private tests agrees only with itself, and then
  it stands in for a database it does not behave like.
- Drive the protocol over a real connection. `bufconn` costs milliseconds.
  Calling a service method directly tests the Go and skips the codec, and the
  codec is where the worst defect in this project's history lived.
- Do not sleep for a fixed time to wait for something. Poll the condition. A
  sleep long enough to be reliable on a loaded machine makes every run slow,
  and a short one makes the suite flake.

## Generated code

- `internal/quorrapb` is generated from `proto/`. Do not edit it.
- Run `make proto` after you change the protocol, and commit the result.
- `make proto-check` regenerates and fails on a difference. CI runs it.
- Reason: this repository once carried two files headed "Code generated by
  protoc-gen-go. DO NOT EDIT." that were written by hand. Their messages were
  plain structs rather than protobuf messages, so gRPC refused every call at
  run time. The code compiled and read correctly in review.

## What to keep

- Look in a directory before you delete it.
- Put each thing that the human chooses into the repository.
  Do not keep it only in a working directory.
