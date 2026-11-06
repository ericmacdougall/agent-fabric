-- 0009: resource reservations (Kimi P3.1, fabric-shaped; kimi-p3-comparison item 2).
--
-- A capacity row bounds one resource (per-host worker slots, a budget in
-- fixed units). A reservation holds part of it for one attempt at its exact
-- fence, is all-or-nothing across the resources of one hold, expires by
-- Postgres time and leaves HELD exactly once (SETTLED, RELEASED or EXPIRED).
-- The Go code takes row locks in a fixed order (attempt, capacities by
-- resource, reservations by id); the triggers below are the backstop that
-- refuses at commit anything the locks failed to prevent. Deferred:
-- double-entry budget accounts, overage hard-stop, tenant-first keys.

-- (id, fence) is already unique through the primary key; the constraint lets
-- a reservation reference the exact fence.
ALTER TABLE attempts ADD CONSTRAINT attempts_id_fence_key UNIQUE (id, fence);

CREATE TABLE resource_capacities (
    resource text PRIMARY KEY CHECK (resource ~ '^[a-z0-9][a-z0-9._:-]{0,62}$'),
    unit     text NOT NULL CHECK (unit ~ '^[a-z0-9_]{1,32}$'),
    capacity bigint NOT NULL CHECK (capacity >= 0),
    held     bigint NOT NULL DEFAULT 0 CHECK (held >= 0 AND held <= capacity),
    version  bigint NOT NULL DEFAULT 0
);

-- The keyed replay table for holds: the first response, verbatim.
CREATE TABLE hold_commands (
    key            text PRIMARY KEY,
    request_sha256 text NOT NULL,
    response_bytes bytea,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE reservations (
    id          text PRIMARY KEY,
    hold_key    text NOT NULL REFERENCES hold_commands (key),
    attempt_id  text NOT NULL,
    fence       bigint NOT NULL,
    resource    text NOT NULL REFERENCES resource_capacities (resource),
    unit        text NOT NULL,
    amount      bigint NOT NULL CHECK (amount > 0),
    generation  int NOT NULL CHECK (generation >= 1),
    state       text NOT NULL DEFAULT 'HELD' CHECK (state IN ('HELD', 'SETTLED', 'RELEASED', 'EXPIRED')),
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    closed_at   timestamptz,
    FOREIGN KEY (attempt_id, fence) REFERENCES attempts (id, fence),
    UNIQUE (attempt_id, resource, generation),
    CHECK ((state = 'HELD') = (closed_at IS NULL))
);
-- Identity only: at most one live hold per attempt and resource. Not a capacity check.
CREATE UNIQUE INDEX reservations_one_live ON reservations (attempt_id, resource) WHERE state = 'HELD';
CREATE INDEX reservations_expiry_idx ON reservations (expires_at) WHERE state = 'HELD';

-- Event 1 is HELD; a terminal reservation has exactly one event 2.
CREATE TABLE reservation_events (
    reservation_id text NOT NULL REFERENCES reservations (id),
    n              smallint NOT NULL CHECK (n IN (1, 2)),
    state          text NOT NULL,
    at             timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (reservation_id, n),
    CHECK ((n = 1) = (state = 'HELD'))
);

-- Actual usage. One receipt per reservation and one reservation per
-- external receipt id within a resource.
CREATE TABLE usage_receipts (
    resource       text NOT NULL,
    receipt_id     text NOT NULL CHECK (receipt_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
    reservation_id text NOT NULL UNIQUE REFERENCES reservations (id),
    actual         bigint NOT NULL CHECK (actual >= 0),
    at             timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (resource, receipt_id)
);

-- Immutable history: events and receipts are append-only; a reservation's
-- identity never changes and its state moves HELD -> terminal once.
CREATE FUNCTION fabric_reject_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% on % is not allowed (append-only)', TG_OP, TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $$;
CREATE TRIGGER reservation_events_append_only BEFORE UPDATE OR DELETE ON reservation_events
    FOR EACH ROW EXECUTE FUNCTION fabric_reject_mutation();
CREATE TRIGGER usage_receipts_append_only BEFORE UPDATE OR DELETE ON usage_receipts
    FOR EACH ROW EXECUTE FUNCTION fabric_reject_mutation();
CREATE TRIGGER reservations_no_delete BEFORE DELETE ON reservations
    FOR EACH ROW EXECUTE FUNCTION fabric_reject_mutation();

CREATE FUNCTION fabric_reservation_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.state <> 'HELD' THEN
        RAISE EXCEPTION 'reservation % is already %', OLD.id, OLD.state USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.id <> OLD.id OR NEW.hold_key <> OLD.hold_key OR NEW.attempt_id <> OLD.attempt_id OR NEW.fence <> OLD.fence
       OR NEW.resource <> OLD.resource OR NEW.unit <> OLD.unit OR NEW.amount <> OLD.amount
       OR NEW.generation <> OLD.generation OR NEW.expires_at <> OLD.expires_at OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'reservation % identity is immutable', OLD.id USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER reservations_transition BEFORE UPDATE ON reservations
    FOR EACH ROW EXECUTE FUNCTION fabric_reservation_update();

-- At commit: held equals the sum of live holds for every touched resource,
-- and every terminal reservation has its event 2 (and a SETTLED one its receipt).
CREATE FUNCTION fabric_capacity_agrees() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    r text := NEW.resource;
    h bigint;
    s numeric;
BEGIN
    SELECT held INTO h FROM resource_capacities WHERE resource = r;
    SELECT COALESCE(sum(amount), 0) INTO s FROM reservations WHERE resource = r AND state = 'HELD';
    IF h IS DISTINCT FROM s THEN
        RAISE EXCEPTION 'capacity % held=% but live holds sum to %', r, h, s USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER resource_capacities_agree AFTER INSERT OR UPDATE ON resource_capacities
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fabric_capacity_agrees();
CREATE CONSTRAINT TRIGGER reservations_capacity_agree AFTER INSERT OR UPDATE ON reservations
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fabric_capacity_agrees();

CREATE FUNCTION fabric_reservation_history() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM reservation_events WHERE reservation_id = NEW.id AND n = 1) THEN
        RAISE EXCEPTION 'reservation % has no HELD event', NEW.id USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.state <> 'HELD' AND NOT EXISTS (SELECT 1 FROM reservation_events WHERE reservation_id = NEW.id AND n = 2 AND state = NEW.state) THEN
        RAISE EXCEPTION 'reservation % is % without its terminal event', NEW.id, NEW.state USING ERRCODE = 'check_violation';
    END IF;
    IF (NEW.state = 'SETTLED') <> EXISTS (SELECT 1 FROM usage_receipts WHERE reservation_id = NEW.id) THEN
        RAISE EXCEPTION 'reservation % state % disagrees with its usage receipt', NEW.id, NEW.state USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER reservations_history AFTER INSERT OR UPDATE ON reservations
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fabric_reservation_history();

-- A receipt or a terminal event inserted on its own must agree with the
-- reservation it names, at commit.
CREATE FUNCTION fabric_reservation_child_agrees() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    st text;
BEGIN
    SELECT state INTO st FROM reservations WHERE id = NEW.reservation_id;
    -- Nested IFs: plpgsql does not short-circuit AND, and NEW.n exists only
    -- on reservation_events.
    IF TG_TABLE_NAME = 'usage_receipts' THEN
        IF st IS DISTINCT FROM 'SETTLED' THEN
            RAISE EXCEPTION 'usage receipt for reservation % in state %', NEW.reservation_id, st USING ERRCODE = 'check_violation';
        END IF;
    ELSIF NEW.n = 2 AND st IS DISTINCT FROM NEW.state THEN
        RAISE EXCEPTION 'event % for reservation % in state %', NEW.state, NEW.reservation_id, st USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER usage_receipts_agree AFTER INSERT ON usage_receipts
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fabric_reservation_child_agrees();
CREATE CONSTRAINT TRIGGER reservation_events_agree AFTER INSERT ON reservation_events
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fabric_reservation_child_agrees();
