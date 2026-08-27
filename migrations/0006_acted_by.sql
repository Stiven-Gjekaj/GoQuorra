-- Who acted on a job.
--
-- Cancel and revive are the two things a person does to a job, and the queue
-- kept no record of who did either. The counters said how many, and the log
-- line said which job, and neither said whose hand it was. On a deployment
-- with one shared key there was no answer to give, so this arrives with the
-- named keys and not before.
--
-- Two columns and not one. The name is what a person reads, and the moment is
-- what makes it a record rather than a rumour: "ops cancelled this" is worth
-- much less than "ops cancelled this on Tuesday at four".
--
-- Nullable, because every job that already exists was acted on by nobody, and
-- a default would claim otherwise.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS acted_by TEXT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS acted_at TIMESTAMPTZ;

-- The pair is set together or not at all, the same way the three lease
-- columns are. A name with no moment reads as a record and is not one.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'jobs_action_is_whole'
    ) THEN
        ALTER TABLE jobs ADD CONSTRAINT jobs_action_is_whole
            CHECK ((acted_by IS NULL) = (acted_at IS NULL));
    END IF;
END
$$;
