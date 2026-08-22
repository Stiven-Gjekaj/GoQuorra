-- Let the table hold a cancelled job.
--
-- The status check in 0001 lists the four states that existed then. A check
-- constraint cannot be altered, so it is dropped and written again. Both
-- statements are safe to apply twice: the drop names IF EXISTS, and the add
-- is skipped when a constraint of that name is already there.

ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_status_known;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'jobs_status_known'
    ) THEN
        ALTER TABLE jobs ADD CONSTRAINT jobs_status_known
            CHECK (status IN ('pending', 'leased', 'succeeded', 'dead', 'cancelled'));
    END IF;
END
$$;

-- The index behind the dead letter queue and the cancelled list.
--
-- Reading "every dead job in this queue" walked the whole table before this.
-- On a table holding a month of finished work that is the difference between
-- a page that opens and one that times out. It is partial, so it costs
-- nothing on the jobs that are still moving.
CREATE INDEX IF NOT EXISTS jobs_finished_idx
    ON jobs (queue, status, seq DESC)
    WHERE status IN ('dead', 'cancelled');
