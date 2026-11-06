-- 0001_core: missions, slots, commands (idempotent replay), events + outbox,
-- attempts with fences and DB-time leases, stale observations.
-- Roll-forward only (DESIGN.md 5.5: no automatic down migration).

CREATE TABLE slots (
    id   int PRIMARY KEY CHECK (id = 1),
    cap  int NOT NULL CHECK (cap >= 0)
);
INSERT INTO slots (id, cap) VALUES (1, 4);

CREATE TABLE missions (
    id                   text PRIMARY KEY,
    ticket               text NOT NULL,
    repo                 text NOT NULL DEFAULT '',
    branch               text NOT NULL DEFAULT '',
    seq                  bigint NOT NULL DEFAULT 0,
    intent_revision      bigint NOT NULL DEFAULT 0,
    attempt_fence        bigint NOT NULL DEFAULT 0,
    candidate_generation bigint NOT NULL DEFAULT 0,
    execution            text NOT NULL DEFAULT 'IDLE',
    candidate            text NOT NULL DEFAULT 'NONE',
    delivery             text NOT NULL DEFAULT 'NOT_READY',
    terminal             text NOT NULL DEFAULT 'NONE',
    state                jsonb NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX missions_execution_idx ON missions (execution);

-- One row per (actor, mission, command_id): the idempotency scope. The first
-- stored response is what every same-key retry gets; a different request hash
-- under the same key is IDEMPOTENCY_CONFLICT and is never stored.
CREATE TABLE commands (
    actor          text NOT NULL,
    mission_id     text NOT NULL REFERENCES missions (id),
    command_id     text NOT NULL,
    kind           text NOT NULL,
    request_sha256 text NOT NULL,
    status         text NOT NULL,
    response       jsonb NOT NULL,
    first_seq      bigint,
    last_seq       bigint,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (actor, mission_id, command_id)
);

-- Gap-free per-mission sequence; the mission row lock serialises writers.
CREATE TABLE events (
    mission_id  text NOT NULL REFERENCES missions (id),
    seq         bigint NOT NULL,
    kind        text NOT NULL,
    fence       bigint,
    actor       text NOT NULL,
    command_id  text NOT NULL,
    payload     jsonb NOT NULL,
    at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (mission_id, seq)
);

-- Transactional outbox: one row per event, consumed in id order by named
-- consumers with idempotent cursors. LISTEN/NOTIFY is only a wake hint.
CREATE TABLE outbox (
    id          bigserial PRIMARY KEY,
    mission_id  text NOT NULL,
    seq         bigint NOT NULL,
    kind        text NOT NULL,
    payload     jsonb NOT NULL,
    at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (mission_id, seq)
);
CREATE TABLE outbox_cursors (
    consumer    text PRIMARY KEY,
    last_id     bigint NOT NULL DEFAULT 0,
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Attempts: derived from ATTEMPT_* events in the same transaction, so the
-- scheduler can query leases by DB time without replaying missions.
CREATE TABLE attempts (
    id          text PRIMARY KEY,
    mission_id  text NOT NULL REFERENCES missions (id),
    fence       bigint NOT NULL,
    revision    bigint NOT NULL,
    generation  bigint NOT NULL,
    host        text NOT NULL DEFAULT '',
    role        text NOT NULL DEFAULT '',
    state       text NOT NULL,
    lease_until timestamptz,
    started_at  timestamptz NOT NULL DEFAULT now(),
    ended_at    timestamptz,
    UNIQUE (mission_id, fence)
);
CREATE INDEX attempts_lease_idx ON attempts (lease_until) WHERE state IN ('LEASED', 'PREPARING', 'RUNNING');

-- Late messages with a dead fence are kept as observations, never applied.
CREATE TABLE stale_observations (
    id            bigserial PRIMARY KEY,
    mission_id    text NOT NULL,
    attempt_id    text NOT NULL DEFAULT '',
    fence         bigint NOT NULL,
    current_fence bigint NOT NULL,
    actor         text NOT NULL,
    command_id    text NOT NULL,
    kind          text NOT NULL,
    payload       jsonb NOT NULL,
    at            timestamptz NOT NULL DEFAULT now()
);
