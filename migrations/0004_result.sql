-- What a job produced.
--
-- A worker that computes something has nowhere to put it today, so every
-- handler that produces a value has to write it somewhere else and hand back
-- a reference. That is the right answer for a large value and a lot of
-- ceremony for a small one.
--
-- The column is nullable, because most jobs produce nothing worth keeping and
-- a NULL costs a bit rather than an empty object.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS result JSONB;
