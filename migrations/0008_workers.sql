-- The workers that have asked for work.
--
-- A queue could say how many jobs were waiting and nothing at all about
-- whether anything was there to run them. leased_by names the worker holding
-- a job and is cleared when the job ends, so a fleet with nothing to do left
-- no trace anywhere: an empty queue and a dead fleet looked the same from
-- outside, and the second one is an outage.
--
-- The row is written where a worker asks for work, which is the only moment
-- the queue hears from one. A separate heartbeat would be a second thing that
-- can be running while the first is not.

CREATE TABLE IF NOT EXISTS workers (
    -- One row for each worker and queue, and not one for each worker.
    --
    -- A worker asks for one queue at a time, so a row for the worker alone
    -- would hold whichever queue it asked about last and change on the next
    -- ask. That reads like a worker moving between queues, which is not what
    -- happened.
    id    TEXT NOT NULL,
    queue TEXT NOT NULL,

    first_seen_at TIMESTAMPTZ NOT NULL,
    last_seen_at  TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (id, queue)
);

-- The order this table is read in: what was here most recently, first.
--
-- It is also what the sweep that removes workers nobody has seen uses. A
-- worker identifier is usually the name of a container, so a deployment
-- retires every row in this table and writes a new set, and without a sweep
-- the table grows once for each worker on each release for ever.
CREATE INDEX IF NOT EXISTS workers_seen_idx ON workers (last_seen_at DESC);
