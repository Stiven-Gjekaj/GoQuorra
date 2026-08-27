-- A rule that produces jobs.
--
-- A schedule is not a job. It holds a rule and a time zone, it produces jobs,
-- and it is never handed to a worker. Making it a job with a flag would mean
-- every listing, every filter and every count had to know which jobs were
-- real, and the one that forgot would hand a schedule to a worker.

CREATE TABLE IF NOT EXISTS schedules (
    id   UUID PRIMARY KEY,

    -- The name an operator reads and types. Unique, because a schedule is
    -- something somebody refers to by name in a change request, and two
    -- called "nightly" is a conversation that goes wrong.
    name TEXT NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 255),

    -- The five field rule, as written. Parsed in Go and stored as text: a
    -- rule kept as a set of columns would be a second spelling that has to
    -- agree with the first.
    cron TEXT NOT NULL,

    -- An IANA name and never an offset. "Every day at nine" moves twice a
    -- year, and an offset is what the zone was on the day it was written
    -- down.
    timezone TEXT NOT NULL DEFAULT 'UTC',

    -- skip, all or none. The part the record said everybody forgets and then
    -- argues about, so the column has no default that hides the decision:
    -- the layer above fills it in and says what it chose.
    catch_up TEXT NOT NULL CHECK (catch_up IN ('skip', 'all', 'none')),

    -- What each firing submits. The same fields a submission takes, because a
    -- schedule that could ask for something a caller could not would be a
    -- second way to make a job.
    job_type    TEXT NOT NULL CHECK (length(job_type) BETWEEN 1 AND 255),
    payload     JSONB NOT NULL DEFAULT '{}',
    queue       TEXT NOT NULL DEFAULT 'default',
    priority    INTEGER NOT NULL DEFAULT 0,
    max_retries INTEGER,

    -- A schedule that is switched off keeps its history and produces nothing.
    -- Deleting it would be the other way to stop it, and then the reason it
    -- existed goes with it.
    enabled BOOLEAN NOT NULL DEFAULT TRUE,

    -- The window this schedule last produced a job for, and not the moment
    -- the loop noticed. Nullable for a schedule that has never fired, which
    -- is what stops the first tick catching up from the year one.
    last_fired_at TIMESTAMPTZ,

    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

-- The order the producing loop reads this in: every schedule that is on.
--
-- A partial index, because a deployment that switches a schedule off leaves
-- it here for ever and the loop never looks at it again.
CREATE INDEX IF NOT EXISTS schedules_enabled_idx
    ON schedules (name) WHERE enabled;

-- Which schedule produced a job.
--
-- On the job and not in a side table. It is one identifier per job, it is
-- read by anybody looking at a job and asking where it came from, and a
-- side table would be a join for a column.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS schedule_id UUID;

-- No foreign key here, deliberately, and it is worth saying why when the
-- table beside it has two.
--
-- A schedule that is deleted should not take its jobs with it, and should not
-- be held back by them either. The jobs it produced are work that happened,
-- and the identifier on them is a record of where they came from rather than
-- a pointer to something that has to still exist.
