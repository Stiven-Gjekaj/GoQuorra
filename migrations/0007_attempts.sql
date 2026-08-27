-- What happened on each run of a job.
--
-- The table held one row per job and nothing else. A job that failed four
-- times and then worked carried one error, from whichever attempt wrote last,
-- and no record that the other three happened at all. Nobody could answer
-- which worker was failing, or whether a job was getting slower.
--
-- One row per finished attempt, written where the job is retired. Both paths
-- that end an attempt, a worker reporting and a lease running out, already
-- meet in applyDecision, so there is one place that writes this and not two
-- that can drift.
--
-- Nothing is written when an attempt starts. A running attempt is already
-- fully visible on the job row, which carries the worker holding it and when
-- the lease ends, and an insert on the lease path would be a second write on
-- the busiest statement in the system to record what the first one already
-- said.

-- When the current attempt started. Cleared with the rest of the lease, after
-- it is copied onto the attempt row.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS leased_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS job_attempts (
    id BIGSERIAL PRIMARY KEY,

    -- The first foreign key in this schema, and it earns its place.
    --
    -- The retention sweep removes finished jobs. Without the cascade, every
    -- sweep would leave the attempts of a removed job behind, and this table
    -- would grow for ever while the one it describes did not. The alternative
    -- is a sweep that remembers to delete from two tables, which is the kind
    -- of thing that is remembered until somebody adds a third.
    job_id UUID NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,

    -- Which run this was. 1 for the first.
    attempt INTEGER NOT NULL CHECK (attempt > 0),

    -- Empty when a lease ran out with no worker named, which happens to a job
    -- reclaimed after the row was written by an older build.
    worker TEXT NOT NULL DEFAULT '',

    -- done, failed, expired or refused. The same four the domain decides
    -- between, and the constraint holds the column to them so that a build
    -- writing a fifth is refused here rather than read back as nonsense.
    outcome TEXT NOT NULL
        CHECK (outcome IN ('done', 'failed', 'expired', 'refused')),

    error TEXT NOT NULL DEFAULT '',

    -- started_at is nullable, because a job leased by an older build carries
    -- no leased_at. A duration is then unknown, which is the true answer,
    -- and a default would make one up.
    -- attempt is not unique within a job, and cannot be.
    --
    -- Reviving a job sets its attempt count back to zero on purpose, so a
    -- job that was buried and then revived runs an attempt 1 for the second
    -- time. A unique key on the pair would refuse that write and break
    -- reviving a job that had ever failed.
    --
    -- The order of this table is the order rows were written, which is the
    -- order the runs happened. id is a BIGSERIAL, so it already says that.
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ NOT NULL
);

-- The order this table is read in: one job's attempts, oldest run first.
CREATE INDEX IF NOT EXISTS job_attempts_job_idx ON job_attempts (job_id, id);

-- A job that nobody holds holds no leased_at either.
--
-- One sided on purpose. A stale leased_at on an unleased job is not harmless:
-- it would be copied onto the next attempt's row as the moment that attempt
-- started, so a run would be timed from a lease that ended hours before. That
-- is the direction this refuses.
--
-- The other direction is left open. A job that is leased right now, by a
-- server running the build before this one, has no leased_at and cannot be
-- given a true one: updated_at moves on every heartbeat, so it says when the
-- lease was last held and not when it began. Refusing those rows would make
-- this migration fail on any deployment with work in flight, and inventing a
-- value would put a wrong duration in the history this table exists to keep.
-- An unknown start is recorded as unknown, which is what started_at being
-- nullable is for.
--
-- Dropped and written again, which is what makes it safe to apply twice.
ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_lease_start_is_clear;
ALTER TABLE jobs ADD CONSTRAINT jobs_lease_start_is_clear
    CHECK (lease_id IS NOT NULL OR leased_at IS NULL);
