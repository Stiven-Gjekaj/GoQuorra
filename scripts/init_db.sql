-- GoQuorra Database Schema

-- Jobs table stores all job metadata and state
CREATE TABLE IF NOT EXISTS jobs (
    id VARCHAR(36) PRIMARY KEY,
    type VARCHAR(255) NOT NULL,
    payload JSONB NOT NULL,
    queue VARCHAR(255) NOT NULL DEFAULT 'default',
    priority INT NOT NULL DEFAULT 0,
    status VARCHAR(50) NOT NULL DEFAULT 'pending',
    attempts INT NOT NULL DEFAULT 0,
    max_retries INT NOT NULL DEFAULT 3,
    last_error TEXT,
    lease_id VARCHAR(255),
    leased_at TIMESTAMP,
    leased_by VARCHAR(255),
    run_at TIMESTAMP NOT NULL DEFAULT NOW(),
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- Indexes for efficient queries
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_jobs_queue ON jobs(queue);
CREATE INDEX IF NOT EXISTS idx_jobs_run_at ON jobs(run_at);
CREATE INDEX IF NOT EXISTS idx_jobs_priority ON jobs(priority DESC);
CREATE INDEX IF NOT EXISTS idx_jobs_lease ON jobs(lease_id) WHERE lease_id IS NOT NULL;

-- Composite index for job leasing queries
CREATE INDEX IF NOT EXISTS idx_jobs_lease_query
    ON jobs(queue, status, run_at, priority DESC)
    WHERE status = 'pending';

-- Queue stats view for quick metrics
CREATE OR REPLACE VIEW queue_stats AS
SELECT
    queue,
    status,
    COUNT(*) as count
FROM jobs
GROUP BY queue, status;

-- Update timestamp trigger
CREATE OR REPLACE FUNCTION update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ language 'plpgsql';

CREATE TRIGGER update_jobs_updated_at BEFORE UPDATE ON jobs
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
