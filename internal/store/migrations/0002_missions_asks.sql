-- 0002: mission metadata for the queue import and the asks inbox (DESIGN.md 3.11).
ALTER TABLE missions
    ADD COLUMN name       text NOT NULL DEFAULT '',
    ADD COLUMN priority   int  NOT NULL DEFAULT 0,   -- queue position; lower runs first
    ADD COLUMN host_needs text NOT NULL DEFAULT '',  -- e.g. heavy, browser
    ADD COLUMN source     text NOT NULL DEFAULT '';  -- provenance of the mission (R12)

-- Asks have no expiry column by design (R13). source is mandatory (R12).
CREATE TABLE asks (
    id          text PRIMARY KEY,
    mission_id  text REFERENCES missions (id),
    kind        text NOT NULL CHECK (kind IN ('decision', 'permission', 'credential-link')),
    question    text NOT NULL,
    options     jsonb NOT NULL DEFAULT '[]'::jsonb,
    recommended text NOT NULL DEFAULT '',
    source      text NOT NULL CHECK (source <> ''),
    state       text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'answered', 'withdrawn')),
    answer      text,
    answered_by text,
    answered_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX asks_open_idx ON asks (created_at) WHERE state = 'open';
