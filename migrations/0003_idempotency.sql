-- A key that makes a submission safe to repeat.
--
-- A client that sends a job and does not see the answer cannot tell whether
-- the server stored it. Retrying is the only thing it can do, and without a
-- key that retry makes a second job. With one, the second submission gets the
-- first job back.

ALTER TABLE jobs ADD COLUMN IF NOT EXISTS idempotency_key TEXT;

-- Partial, so that the many jobs submitted without a key do not all collide
-- on NULL and do not sit in the index at all.
--
-- The uniqueness is across the whole table and not per queue. A key is chosen
-- by the client to mean "this piece of work", and the same work landing in
-- two queues is still the same work.
CREATE UNIQUE INDEX IF NOT EXISTS jobs_idempotency_key_idx
    ON jobs (idempotency_key)
    WHERE idempotency_key IS NOT NULL;
