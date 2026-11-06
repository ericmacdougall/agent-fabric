-- 0010: reservation backstops the red-team showed missing (review F4).
-- Each refuses at the database a write the Go code never makes; together with
-- 0009 they hold even for a forged transaction that keeps held = sum(live).
-- Roll-forward only. Constraints validate existing rows: rows written by the
-- Go code since 0009 satisfy all of them.

-- A usage receipt belongs to the resource of the reservation it settles.
ALTER TABLE reservations ADD CONSTRAINT reservations_resource_id_key UNIQUE (resource, id);
ALTER TABLE usage_receipts ADD CONSTRAINT usage_receipts_reservation_resource_fkey
    FOREIGN KEY (resource, reservation_id) REFERENCES reservations (resource, id);

-- A reservation is counted in its capacity's unit. With the foreign key a
-- capacity cannot change unit while any reservation names it.
ALTER TABLE resource_capacities ADD CONSTRAINT resource_capacities_resource_unit_key UNIQUE (resource, unit);
ALTER TABLE reservations ADD CONSTRAINT reservations_resource_unit_fkey
    FOREIGN KEY (resource, unit) REFERENCES resource_capacities (resource, unit);

-- The TTL is 1 second to 24 hours from the decision (created_at).
ALTER TABLE reservations ADD CONSTRAINT reservations_ttl_bounds
    CHECK (expires_at >= created_at + interval '1 second' AND expires_at <= created_at + interval '24 hours');

-- A reservation is born HELD, for an attempt that is live at that fence.
CREATE FUNCTION fabric_reservation_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    st text;
BEGIN
    IF NEW.state <> 'HELD' OR NEW.closed_at IS NOT NULL THEN
        RAISE EXCEPTION 'reservation % inserted in state %', NEW.id, NEW.state USING ERRCODE = 'check_violation';
    END IF;
    SELECT state INTO st FROM attempts WHERE id = NEW.attempt_id AND fence = NEW.fence;
    IF st IS NULL OR st NOT IN ('LEASED', 'PREPARING', 'RUNNING') THEN
        RAISE EXCEPTION 'reservation % for attempt % at fence % in state %', NEW.id, NEW.attempt_id, NEW.fence, st
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER reservations_insert BEFORE INSERT ON reservations
    FOR EACH ROW EXECUTE FUNCTION fabric_reservation_insert();

-- Append-only also means no TRUNCATE (a row trigger never sees one). The
-- transaction-local setting fabric.allow_truncate = 'on' is the tests' reset
-- (Store.Truncate). It is a guard against accidents, not against a role that
-- can SET it or drop triggers: production should connect as a role that does
-- not own these tables, is not a superuser and has no TRUNCATE privilege.
CREATE FUNCTION fabric_reject_truncate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF current_setting('fabric.allow_truncate', true) = 'on' THEN
        RETURN NULL;
    END IF;
    RAISE EXCEPTION 'TRUNCATE on % is not allowed (append-only)', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $$;
CREATE TRIGGER reservation_events_no_truncate BEFORE TRUNCATE ON reservation_events
    FOR EACH STATEMENT EXECUTE FUNCTION fabric_reject_truncate();
CREATE TRIGGER usage_receipts_no_truncate BEFORE TRUNCATE ON usage_receipts
    FOR EACH STATEMENT EXECUTE FUNCTION fabric_reject_truncate();
CREATE TRIGGER reservations_no_truncate BEFORE TRUNCATE ON reservations
    FOR EACH STATEMENT EXECUTE FUNCTION fabric_reject_truncate();
CREATE TRIGGER hold_commands_no_truncate BEFORE TRUNCATE ON hold_commands
    FOR EACH STATEMENT EXECUTE FUNCTION fabric_reject_truncate();
