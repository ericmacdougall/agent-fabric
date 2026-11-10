-- goodqa#15586 F19a: the Control Room's own schema, `control`.
--
-- Two kinds of table, kept apart on purpose:
--   source tables (append-only, never rebuilt): ingested_events (mc.event/1
--     lines from the harness, stored under their own source-prefixed id) and
--     mc_chat_mirror (Mission Control's chat, mirrored read-only, origin 'mc').
--     Neither is ever written to fabric `events` or `outbox`, so fabd, the F1a
--     held-stream check and a rolled-back binary never see a foreign row.
--   projection tables (droppable and rebuildable from `events`, `outbox` and
--     the source tables at an as-of time): proj_cursor, initiative_state,
--     pulses. Dropping and rebuilding them never touches a source table.
--
-- Nothing here is granted to any role: grants for the control roles are made
-- by `fab db roles` (a database that has not run it exposes `control` to the
-- owner only). The schema is new and purely additive: no existing table,
-- column, trigger or function changes, so a binary that predates 0039 runs on
-- it unchanged. Roll forward only (the fabric_schema forward guard refuses an
-- older binary on this database, as for every migration). Safe to re-apply:
-- IF NOT EXISTS, and a drop and re-create by name for triggers.

-- Migrate applies each file in its own transaction, so SET LOCAL holds for this
-- migration only; only a CREATE SCHEMA and new tables are taken, so there is no
-- lock on a live table to wait for.
SET LOCAL lock_timeout = '5s';

CREATE SCHEMA IF NOT EXISTS control;

-- Source: events ingested from other producers (the harness dashboard feed).
-- id is "<source>:<stable id>", the producer's own id, so a replay stores one
-- row (primary key) and Fabric's own ids (prefix "fabric:") can be refused by
-- the ingest role. seq is the local arrival order the projector pages by.
CREATE TABLE IF NOT EXISTS control.ingested_events (
    seq         bigserial PRIMARY KEY,
    id          text        NOT NULL UNIQUE
                CHECK (id ~ '^[a-z][a-z0-9_-]{0,31}:[^[:cntrl:]]{1,200}$'),
    ts          timestamptz NOT NULL,
    kind        text        NOT NULL
                CHECK (kind IN ('wal','push','release','deploy','ticket','chat','owner','initiative','system')),
    source      text        NOT NULL CHECK (char_length(source) BETWEEN 1 AND 64),
    host        text        NOT NULL DEFAULT '' CHECK (char_length(host) <= 128),
    actor       text        NOT NULL DEFAULT '' CHECK (char_length(actor) <= 128),
    initiative  text        NOT NULL DEFAULT '' CHECK (char_length(initiative) <= 128),
    repo        text        NOT NULL DEFAULT '' CHECK (char_length(repo) <= 200),
    branch      text        NOT NULL DEFAULT '' CHECK (char_length(branch) <= 200),
    ref         text        NOT NULL DEFAULT '' CHECK (char_length(ref) <= 200),
    ticket      text        NOT NULL DEFAULT '' CHECK (char_length(ticket) <= 64),
    -- A short summary only, never a payload. No control character (the page
    -- renders it as text, and a terminal escape must not survive storage).
    summary     text        NOT NULL
                CHECK (char_length(summary) <= 280 AND summary !~ '[[:cntrl:]]'),
    severity    text        NOT NULL DEFAULT 'info' CHECK (char_length(severity) BETWEEN 1 AND 16),
    count       integer     NOT NULL DEFAULT 1 CHECK (count BETWEEN 1 AND 1000000),
    tenant      text        CHECK (tenant IS NULL OR char_length(tenant) BETWEEN 1 AND 64),
    received_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ingested_events_initiative_ts ON control.ingested_events (initiative, ts);

-- Source: Mission Control's chat, mirrored read-only. origin is always 'mc';
-- a row of Fabric origin is not representable here. The body is stored after
-- the ingest has redacted secrets (the ingest, not this table, enforces that).
CREATE TABLE IF NOT EXISTS control.mc_chat_mirror (
    seq         bigserial PRIMARY KEY,
    id          text        NOT NULL UNIQUE CHECK (id ~ '^[^[:cntrl:]]{1,200}$'),
    origin      text        NOT NULL DEFAULT 'mc' CHECK (origin = 'mc'),
    ts          timestamptz NOT NULL,
    sender      text        NOT NULL CHECK (char_length(sender) BETWEEN 1 AND 128),
    recipient   text        NOT NULL DEFAULT '' CHECK (char_length(recipient) <= 128),
    body        text        NOT NULL CHECK (char_length(body) <= 4000),
    received_at timestamptz NOT NULL DEFAULT now()
);

-- Projection: the projector's position in the outbox (and in each source
-- table), one row per consumer. Rebuilding resets it.
CREATE TABLE IF NOT EXISTS control.proj_cursor (
    consumer    text        PRIMARY KEY CHECK (char_length(consumer) BETWEEN 1 AND 64),
    last_id     bigint      NOT NULL DEFAULT 0 CHECK (last_id >= 0),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Projection: one current state per initiative (a stream, or an ingested
-- initiative name). Every value is a pure function of the events and an as-of
-- time; last_source_event_id names the event the last work item came from.
CREATE TABLE IF NOT EXISTS control.initiative_state (
    initiative            text        PRIMARY KEY CHECK (char_length(initiative) BETWEEN 1 AND 128),
    state                 text        NOT NULL
                          CHECK (state IN ('running','waiting','held','stalled','unowned','idle','done')),
    wait_kind             text        NOT NULL DEFAULT '' CHECK (char_length(wait_kind) <= 64),
    last_kind             text        NOT NULL DEFAULT '' CHECK (char_length(last_kind) <= 64),
    last_summary          text        NOT NULL DEFAULT ''
                          CHECK (char_length(last_summary) <= 280 AND last_summary !~ '[[:cntrl:]]'),
    last_ts               timestamptz,
    last_source_event_id  text        NOT NULL DEFAULT '' CHECK (char_length(last_source_event_id) <= 200),
    as_of                 timestamptz NOT NULL
);

-- Projection: the pulse feed, one row per source event, in arrival order.
CREATE TABLE IF NOT EXISTS control.pulses (
    id               bigserial   PRIMARY KEY,
    source_event_id  text        NOT NULL UNIQUE CHECK (char_length(source_event_id) BETWEEN 1 AND 200),
    initiative       text        NOT NULL DEFAULT '' CHECK (char_length(initiative) <= 128),
    kind             text        NOT NULL CHECK (char_length(kind) BETWEEN 1 AND 64),
    summary          text        NOT NULL DEFAULT ''
                     CHECK (char_length(summary) <= 280 AND summary !~ '[[:cntrl:]]'),
    ts               timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS pulses_ts ON control.pulses (ts, id);

-- Append-only source tables: no UPDATE, DELETE or TRUNCATE (the functions are
-- the ones 0009 and 0010 made; the transaction-local test reset in 0010 applies
-- to TRUNCATE only).
DROP TRIGGER IF EXISTS ingested_events_append_only ON control.ingested_events;
CREATE TRIGGER ingested_events_append_only BEFORE UPDATE OR DELETE ON control.ingested_events
    FOR EACH ROW EXECUTE FUNCTION public.fabric_reject_mutation();
DROP TRIGGER IF EXISTS ingested_events_no_truncate ON control.ingested_events;
CREATE TRIGGER ingested_events_no_truncate BEFORE TRUNCATE ON control.ingested_events
    FOR EACH STATEMENT EXECUTE FUNCTION public.fabric_reject_truncate();
DROP TRIGGER IF EXISTS mc_chat_mirror_append_only ON control.mc_chat_mirror;
CREATE TRIGGER mc_chat_mirror_append_only BEFORE UPDATE OR DELETE ON control.mc_chat_mirror
    FOR EACH ROW EXECUTE FUNCTION public.fabric_reject_mutation();
DROP TRIGGER IF EXISTS mc_chat_mirror_no_truncate ON control.mc_chat_mirror;
CREATE TRIGGER mc_chat_mirror_no_truncate BEFORE TRUNCATE ON control.mc_chat_mirror
    FOR EACH STATEMENT EXECUTE FUNCTION public.fabric_reject_truncate();

-- Nothing in `control` is readable or writable by PUBLIC.
REVOKE ALL ON SCHEMA control FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA control FROM PUBLIC;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA control FROM PUBLIC;
