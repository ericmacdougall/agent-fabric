-- 0014: red-team F2 on f54d526 and the reservation clock backstops (Kimi
-- reservation-clock tranche, case 8). Roll-forward only.

-- A hold receipt is written once: the row is inserted with its key and
-- request hash, and the same transaction fills response_bytes once. After
-- that it can be neither changed nor deleted, so a replay always returns the
-- first decision.
CREATE FUNCTION fabric_hold_command_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'DELETE on hold_commands is not allowed (append-only)' USING ERRCODE = 'restrict_violation';
    END IF;
    IF OLD.response_bytes IS NOT NULL OR NEW.response_bytes IS NULL
       OR NEW.key <> OLD.key OR NEW.request_sha256 <> OLD.request_sha256 OR NEW.created_at <> OLD.created_at
       OR OLD.xmin::text <> (txid_current() % 4294967296)::text THEN
        RAISE EXCEPTION 'hold receipt % is written once, by the transaction that inserted it', OLD.key USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER hold_commands_write_once BEFORE UPDATE OR DELETE ON hold_commands
    FOR EACH ROW EXECUTE FUNCTION fabric_hold_command_update();

-- Reservation times are checked against the wall clock when the trigger
-- runs, after any lock wait; now() is the transaction start. A new row's
-- decision time lies inside its own transaction and it is not yet expired;
-- a settlement happens before expires_at and an expiry at or after it.
CREATE FUNCTION fabric_reservation_clock() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    t timestamptz := clock_timestamp();
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.created_at < now() OR NEW.created_at > t THEN
            RAISE EXCEPTION 'reservation % decision time % is outside its transaction', NEW.id, NEW.created_at USING ERRCODE = 'check_violation';
        END IF;
        IF NEW.expires_at <= t THEN
            RAISE EXCEPTION 'reservation % is born expired', NEW.id USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.state = 'SETTLED' AND OLD.expires_at <= t THEN
        RAISE EXCEPTION 'reservation % expired before settlement', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.state = 'EXPIRED' AND OLD.expires_at > t THEN
        RAISE EXCEPTION 'reservation % expired before expires_at', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.closed_at IS NOT NULL AND (NEW.closed_at < now() OR NEW.closed_at > t) THEN
        RAISE EXCEPTION 'reservation % closed_at is outside its transaction', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER reservations_clock BEFORE INSERT OR UPDATE ON reservations
    FOR EACH ROW EXECUTE FUNCTION fabric_reservation_clock();
