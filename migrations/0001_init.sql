-- The jobs table, and nothing else.
--
-- This file is applied whole and is safe to apply twice. It is read by the
-- test suite as well as by `make db-init`, so a change here reaches both.

CREATE TABLE IF NOT EXISTS jobs (
    id               UUID PRIMARY KEY,

    -- seq gives a stable order to two jobs written in the same instant.
    -- created_at cannot do this on its own: PostgreSQL keeps a timestamp to
    -- the microsecond, and a burst of submissions lands several rows inside
    -- one. Without a tie break, "the ten newest jobs" returns a different ten
    -- each time it is asked.
    seq              BIGINT GENERATED ALWAYS AS IDENTITY,

    type             TEXT NOT NULL,
    payload          JSONB NOT NULL DEFAULT '{}'::jsonb,
    queue            TEXT NOT NULL DEFAULT 'default',
    priority         INTEGER NOT NULL DEFAULT 0,
    status           TEXT NOT NULL,
    attempts         INTEGER NOT NULL DEFAULT 0,
    max_retries      INTEGER NOT NULL DEFAULT 3,
    last_error       TEXT NOT NULL DEFAULT '',

    lease_id         UUID,
    leased_by        TEXT,
    lease_expires_at TIMESTAMPTZ,

    -- Every time column carries a zone.
    --
    -- The previous schema used TIMESTAMP, which drops it. A server running in
    -- a zone other than UTC then writes local times into the same column as a
    -- server running in UTC, and the comparison that decides whether a job is
    -- ready silently answers by an hour or more. The failure appears twice a
    -- year, moves with the daylight saving change, and looks like a queue
    -- that stalls or that runs delayed jobs early.
    run_at           TIMESTAMPTZ NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL,

    -- The database refuses a status the program does not know. A row written
    -- by a newer version, or by hand during an incident, cannot leave a job
    -- in a state that no query selects and no worker collects.
    CONSTRAINT jobs_status_known
        CHECK (status IN ('pending', 'leased', 'succeeded', 'dead')),

    -- The three lease columns are set together or not at all. A stale one of
    -- them on its own is how a reclaimer decides to take back a job that
    -- nobody holds, or how a report matches a lease that has ended.
    CONSTRAINT jobs_lease_is_whole
        CHECK (
            (lease_id IS NULL AND leased_by IS NULL AND lease_expires_at IS NULL)
            OR
            (lease_id IS NOT NULL AND leased_by IS NOT NULL AND lease_expires_at IS NOT NULL)
        ),

    -- A leased job holds a lease, and a job in any other state holds none.
    CONSTRAINT jobs_lease_matches_status
        CHECK ((status = 'leased') = (lease_id IS NOT NULL)),

    CONSTRAINT jobs_attempts_are_not_negative CHECK (attempts >= 0),
    CONSTRAINT jobs_retries_are_not_negative CHECK (max_retries >= 0)
);

-- The index the lease query uses.
--
-- The columns are in the order the query sorts by, so PostgreSQL walks the
-- index and stops at the limit rather than sorting the whole queue. status is
-- not a key column, because the partial predicate already fixes it; the
-- previous version of this index carried it as a key and paid for it on every
-- insert. Every pending job is in this index and no other job is, which on a
-- table holding a month of finished work is the difference between reading
-- thousands of rows and reading tens.
CREATE INDEX IF NOT EXISTS jobs_ready_idx
    ON jobs (queue, priority DESC, run_at, seq)
    WHERE status = 'pending';

-- The index the reclaimer uses. It holds only the jobs a worker is running,
-- which is a small number at any moment, so the sweep costs almost nothing
-- however large the table grows.
CREATE INDEX IF NOT EXISTS jobs_expiring_idx
    ON jobs (lease_expires_at)
    WHERE status = 'leased';

-- The index behind the dashboard.
CREATE INDEX IF NOT EXISTS jobs_recent_idx
    ON jobs (created_at DESC, seq DESC);

-- There is deliberately no trigger on updated_at.
--
-- The previous schema carried one that set updated_at to NOW() on every
-- UPDATE. The program also sets that column, so the trigger overwrote the
-- value the program had just written, with a time read from the database
-- clock instead of the server clock. Two clocks then decided one column, and
-- a test that moved time forward could not move this one. Every time in this
-- table is written by the program and travels as a parameter.
