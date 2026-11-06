-- 0006: a worker requests a job; the daemon starts it outside the sandbox
-- (review C1: workers must not reach the user manager themselves).
ALTER TABLE jobs DROP CONSTRAINT jobs_state_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_state_check CHECK (state IN ('REQUESTED', 'STARTED', 'EXITED', 'FAILED_TO_START'));
ALTER TABLE jobs ALTER COLUMN state SET DEFAULT 'REQUESTED';
ALTER TABLE jobs ADD COLUMN rundir text NOT NULL DEFAULT '';
