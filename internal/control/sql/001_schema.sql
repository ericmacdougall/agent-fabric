-- goodqa#15586 F19a: the Control Room's own schema, `control`, version 1.
--
-- Created by `fab control bootstrap`, run once by the database owner, never at
-- runtime and never through the numbered migrations: it does not touch
-- fabric_schema, so the schema level, the binary-only rollback window and
-- deploy-rollback.sh are unchanged (spec AC-13, NEG-14). Its own ledger,
-- control.bootstrap_ledger, records the version and the sha256 of this file;
-- bootstrap refuses to run over a schema made from a different definition.
--
-- Two kinds of table, kept apart on purpose:
--   source tables (append-only, never rebuilt): ingested_events (mc.event/1
--     lines from the harness, stored under their own source-prefixed id) and
--     mc_chat_mirror (Mission Control's chat, mirrored read-only, origin 'mc').
--     Neither is ever written to fabric `events` or `outbox`.
--   projection tables (droppable and rebuildable from `events`, `outbox` and
--     the source tables at an as-of time): proj_cursor, initiative_state,
--     pulses.
--
-- Nothing here is granted to any role; the control roles' grants are made
-- separately. Everything lives in schema control (the bootstrap creates it).

-- The append-only functions are this schema's own, with no test exemption
-- (unlike public.fabric_reject_truncate, which a role can disarm with a plain
-- SET): a role holding TRUNCATE must still be refused.
CREATE FUNCTION control.reject_mutation() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog AS $$
BEGIN
    RAISE EXCEPTION '% on % is not allowed (append-only)', TG_OP, TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $$;

-- Source: events ingested from other producers. id is "<source>:<stable id>",
-- the producer's own id, and source must equal that prefix and may never be
-- 'fabric': Fabric's own events come from the outbox and never from this table.
-- seq is an identity the projector pages by (a producer cannot choose it).
CREATE TABLE control.ingested_events (
    seq         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    id          text        NOT NULL UNIQUE
                CHECK (id ~ '^[a-z][a-z0-9_-]{0,31}:[^[:cntrl:]]{1,200}$'),
    ts          timestamptz NOT NULL,
    kind        text        NOT NULL
                CHECK (kind IN ('wal','push','release','deploy','ticket','chat','owner','initiative','system')),
    source      text        NOT NULL CHECK (char_length(source) BETWEEN 1 AND 64),
    host        text        NOT NULL DEFAULT '' CHECK (char_length(host) <= 128 AND host !~ '[[:cntrl:]]'),
    actor       text        NOT NULL DEFAULT '' CHECK (char_length(actor) <= 128 AND actor !~ '[[:cntrl:]]'),
    initiative  text        NOT NULL DEFAULT '' CHECK (char_length(initiative) <= 128 AND initiative !~ '[[:cntrl:]]'),
    repo        text        NOT NULL DEFAULT '' CHECK (char_length(repo) <= 200 AND repo !~ '[[:cntrl:]]'),
    branch      text        NOT NULL DEFAULT '' CHECK (char_length(branch) <= 200 AND branch !~ '[[:cntrl:]]'),
    ref         text        NOT NULL DEFAULT '' CHECK (char_length(ref) <= 200 AND ref !~ '[[:cntrl:]]'),
    ticket      text        NOT NULL DEFAULT '' CHECK (char_length(ticket) <= 64 AND ticket !~ '[[:cntrl:]]'),
    -- A short summary only, never a payload. No control character (a terminal
    -- escape must not survive storage; the page still escapes every field).
    summary     text        NOT NULL
                CHECK (char_length(summary) <= 280 AND summary !~ '[[:cntrl:]]'),
    severity    text        NOT NULL DEFAULT 'info' CHECK (char_length(severity) BETWEEN 1 AND 16 AND severity !~ '[[:cntrl:]]'),
    count       integer     NOT NULL DEFAULT 1 CHECK (count BETWEEN 1 AND 1000000),
    tenant      text        CHECK (tenant IS NULL OR (char_length(tenant) BETWEEN 1 AND 64 AND tenant !~ '[[:cntrl:]]')),
    received_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ingested_events_source_is_id_prefix CHECK (source = split_part(id, ':', 1) AND source <> 'fabric')
);
CREATE INDEX ingested_events_initiative_ts ON control.ingested_events (initiative, ts);

-- Source: Mission Control's chat, mirrored read-only. origin is always 'mc'; a
-- row of Fabric origin is not representable here. The body is stored after the
-- ingest has redacted secrets (the ingest enforces that, not this table); only
-- line feeds are allowed among control characters.
CREATE TABLE control.mc_chat_mirror (
    seq         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    id          text        NOT NULL UNIQUE CHECK (id ~ '^[^[:cntrl:]]{1,200}$'),
    origin      text        NOT NULL DEFAULT 'mc' CHECK (origin = 'mc'),
    ts          timestamptz NOT NULL,
    sender      text        NOT NULL CHECK (char_length(sender) BETWEEN 1 AND 128 AND sender !~ '[[:cntrl:]]'),
    recipient   text        NOT NULL DEFAULT '' CHECK (char_length(recipient) <= 128 AND recipient !~ '[[:cntrl:]]'),
    body        text        NOT NULL CHECK (char_length(body) <= 4000 AND translate(body, E'\n', '') !~ '[[:cntrl:]]'),
    received_at timestamptz NOT NULL DEFAULT now()
);

-- Projection: the projector's position in the outbox (and in each source
-- table), one row per consumer. A rebuild resets it.
CREATE TABLE control.proj_cursor (
    consumer    text        PRIMARY KEY CHECK (char_length(consumer) BETWEEN 1 AND 64),
    last_id     bigint      NOT NULL DEFAULT 0 CHECK (last_id >= 0),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Projection: one current state per initiative (a stream, or an ingested
-- initiative name). Every value is a pure function of the events and an as-of
-- time; last_source_event_id names the event the last work item came from.
CREATE TABLE control.initiative_state (
    initiative            text        PRIMARY KEY CHECK (char_length(initiative) BETWEEN 1 AND 128 AND initiative !~ '[[:cntrl:]]'),
    state                 text        NOT NULL
                          CHECK (state IN ('running','waiting','held','stalled','unowned','idle','done')),
    wait_kind             text        NOT NULL DEFAULT '' CHECK (char_length(wait_kind) <= 64 AND wait_kind !~ '[[:cntrl:]]'),
    last_kind             text        NOT NULL DEFAULT '' CHECK (char_length(last_kind) <= 64 AND last_kind !~ '[[:cntrl:]]'),
    last_summary          text        NOT NULL DEFAULT ''
                          CHECK (char_length(last_summary) <= 280 AND last_summary !~ '[[:cntrl:]]'),
    last_ts               timestamptz,
    last_source_event_id  text        NOT NULL DEFAULT '' CHECK (char_length(last_source_event_id) <= 200 AND last_source_event_id !~ '[[:cntrl:]]'),
    as_of                 timestamptz NOT NULL
);

-- Projection: the pulse feed, one row per source event, in arrival order.
CREATE TABLE control.pulses (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_event_id  text        NOT NULL UNIQUE CHECK (char_length(source_event_id) BETWEEN 1 AND 200 AND source_event_id !~ '[[:cntrl:]]'),
    initiative       text        NOT NULL DEFAULT '' CHECK (char_length(initiative) <= 128 AND initiative !~ '[[:cntrl:]]'),
    kind             text        NOT NULL CHECK (char_length(kind) BETWEEN 1 AND 64 AND kind !~ '[[:cntrl:]]'),
    summary          text        NOT NULL DEFAULT ''
                     CHECK (char_length(summary) <= 280 AND summary !~ '[[:cntrl:]]'),
    ts               timestamptz NOT NULL
);
CREATE INDEX pulses_ts ON control.pulses (ts, id);

-- Append-only source tables: no UPDATE, DELETE or TRUNCATE.
CREATE TRIGGER ingested_events_append_only BEFORE UPDATE OR DELETE ON control.ingested_events
    FOR EACH ROW EXECUTE FUNCTION control.reject_mutation();
CREATE TRIGGER ingested_events_no_truncate BEFORE TRUNCATE ON control.ingested_events
    FOR EACH STATEMENT EXECUTE FUNCTION control.reject_mutation();
CREATE TRIGGER mc_chat_mirror_append_only BEFORE UPDATE OR DELETE ON control.mc_chat_mirror
    FOR EACH ROW EXECUTE FUNCTION control.reject_mutation();
CREATE TRIGGER mc_chat_mirror_no_truncate BEFORE TRUNCATE ON control.mc_chat_mirror
    FOR EACH STATEMENT EXECUTE FUNCTION control.reject_mutation();

-- Nothing in `control` is readable or writable by PUBLIC.
REVOKE ALL ON SCHEMA control FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA control FROM PUBLIC;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA control FROM PUBLIC;
