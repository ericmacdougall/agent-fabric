-- 0004: host jobs and the waits projection (DESIGN.md 3.4, AC-2).
-- A job is owned by the host, lives in its own transient unit, and its exit
-- is an event that wakes the mission. A wait is one row per mission while
-- open; the scheduler maintains it from the outbox.
CREATE TABLE jobs (
    id          text PRIMARY KEY,
    mission_id  text NOT NULL REFERENCES missions (id),
    attempt_id  text NOT NULL,
    fence       bigint NOT NULL,
    argv        jsonb NOT NULL,
    state       text NOT NULL DEFAULT 'STARTED' CHECK (state IN ('STARTED', 'EXITED', 'FAILED_TO_START')),
    unit        text NOT NULL DEFAULT '',
    pid         int,
    exit_code   int,
    log_tail    text NOT NULL DEFAULT '',
    woken       boolean NOT NULL DEFAULT false,
    started_at  timestamptz NOT NULL DEFAULT now(),
    ended_at    timestamptz
);
CREATE INDEX jobs_exited_unwoken_idx ON jobs (ended_at) WHERE state = 'EXITED' AND NOT woken;

CREATE TABLE waits (
    mission_id  text PRIMARY KEY REFERENCES missions (id),
    kind        text NOT NULL,
    key         text NOT NULL,
    since       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sched_counters (
    name   text PRIMARY KEY,
    value  bigint NOT NULL DEFAULT 0
);
