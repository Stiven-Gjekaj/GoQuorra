## Summary

Describe what this pull request changes, and why.

## Related issue

Link the issue that this addresses, if there is one (for example,
"Closes #12").

## Changes

-

## Testing

Say how you checked the change. These should pass:

- [ ] `make verify`
- [ ] `make test-postgres`, if you touched storage

Say what you measured, and how. A number you can prove beats a number you
calculated from a part.

## Checklist

- [ ] Each commit holds one change. A feature is many commits, not one.
- [ ] The code and its tests are in the same commit.
- [ ] The documentation is in its own commit.
- [ ] Every subject line is in the present tense, and carries no version
      number.
- [ ] All text uses Simplified Technical English. No em-dashes, and no emoji.
- [ ] A new rule about a job is in `internal/jobs`, and not in a SQL statement
      or an HTTP handler.
- [ ] A new rule about storage is in `internal/store/storetest`, and both
      stores pass it.
- [ ] A new test was checked by putting the fault back and watching it fail.
- [ ] `internal/quorrapb` was regenerated with `make proto`, if the protocol
      changed. It was not edited by hand.
- [ ] Any new dependency is explained: say why the standard library does not
      do the job.
