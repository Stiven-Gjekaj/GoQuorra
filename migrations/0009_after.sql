-- A job that runs after another job.
--
-- The only one of the three side tables this schema gains that the record had
-- never considered, so the reasoning is written out rather than referred to.
--
-- Three questions had to be answered before a row could be written.
--
-- What happens when a parent will never succeed. The child is cancelled, and
-- the parent that stopped it is named in last_error. Cancelled and not dead:
-- dead means the job used every attempt it had, and this job used none. A
-- person who fixes the parent revives the child, which is the same path a
-- person already knows.
--
-- Whether a cycle is refused at submission or found later. Neither. A job may
-- only wait for a job that already exists, because a caller has to name the
-- identifier of one, and a job cannot be created before itself. That makes
-- the graph a directed acyclic one by construction, and there is no check to
-- write and none to forget.
--
-- Where the waiting is recorded. Here and not on the job. A job may wait for
-- several, so a column would hold a list, and a list in a column cannot be
-- indexed by the query that matters: given a job that just succeeded, which
-- jobs were waiting for it.

CREATE TABLE IF NOT EXISTS job_after (
    -- The job that waits.
    job_id UUID NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,

    -- The job it waits for.
    --
    -- The cascade goes both ways for the same reason it does on the attempts:
    -- the retention sweep removes finished jobs, and a row pointing at a job
    -- that is gone would hold a child blocked for ever with nothing to
    -- explain it. Removing the row is right, because a parent that has been
    -- swept away succeeded long enough ago to be forgotten.
    after_id UUID NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,

    PRIMARY KEY (job_id, after_id)
);

-- The query that matters: a job has just succeeded, so who was waiting for
-- it. The primary key answers the other direction, which is the one a
-- submission uses.
CREATE INDEX IF NOT EXISTS job_after_parent_idx ON job_after (after_id);

-- blocked joins the states a job may be in.
--
-- Dropped and written again, which is what makes it safe to apply twice. This
-- is the second migration to do it: 0002 added cancelled the same way.
ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_status_known;
ALTER TABLE jobs ADD CONSTRAINT jobs_status_known
    CHECK (status IN ('pending', 'leased', 'succeeded', 'dead', 'cancelled', 'blocked'));

-- A blocked job holds no lease, the same way every other unleased state does.
--
-- The existing constraint says a lease is held exactly when the status is
-- leased, so it already covers this. The line is here to say that was
-- checked, and not to add a second rule saying the same thing.
