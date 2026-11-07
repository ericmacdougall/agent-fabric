-- 0020: review N2 on aecf3fa. Roll-forward only.
--
-- The operator exit for a QUARANTINED effect: an append-only audit row
-- (who, why, evidence) and the effect moved to RESOLVED_APPLIED or
-- RESOLVED_NOT_APPLIED in the same transaction (Broker.ResolveQuarantined).
-- The same table audits an abandon (Broker.Abandon, main ruling on the
-- review nit): a never-dispatched intent whose attempt ended, to
-- NOT_APPLIED. The runner role gets no write on this table
-- (Store.ApplyRoles) and its effects guard (0019) has neither step.
CREATE TABLE effect_resolutions (
    id         bigserial PRIMARY KEY,
    effect_id  text NOT NULL REFERENCES effects (id),
    from_state text NOT NULL,
    to_state   text NOT NULL,
    reason     text NOT NULL CHECK (reason <> ''),
    evidence   text NOT NULL CHECK (evidence <> ''),
    actor      text NOT NULL CHECK (actor <> ''),
    db_user    text NOT NULL,
    xact       bigint NOT NULL,
    at         timestamptz NOT NULL,
    CHECK ((from_state = 'QUARANTINED' AND to_state IN ('RESOLVED_APPLIED', 'RESOLVED_NOT_APPLIED'))
        OR (from_state IN ('INTENT', 'APPROVAL_REQUIRED', 'APPROVED') AND to_state = 'NOT_APPLIED'))
);
-- A quarantined effect resolves once; an abandoned one may be retried and
-- abandoned again, each with its own row.
CREATE UNIQUE INDEX effect_resolutions_one_idx ON effect_resolutions (effect_id) WHERE from_state = 'QUARANTINED';

-- Who, when and which transaction are set by the database, not the writer:
-- the effects guard accepts only an audit row from its own transaction.
CREATE FUNCTION fabric_effect_resolutions_stamp() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    NEW.db_user := current_user;
    NEW.xact := txid_current();
    NEW.at := now();
    RETURN NEW;
END $$;
CREATE TRIGGER effect_resolutions_stamp BEFORE INSERT ON effect_resolutions
    FOR EACH ROW EXECUTE FUNCTION fabric_effect_resolutions_stamp();

CREATE FUNCTION fabric_effect_resolutions_append_only() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    RAISE EXCEPTION 'effect_resolutions is append-only' USING ERRCODE = 'insufficient_privilege';
END $$;
CREATE TRIGGER effect_resolutions_append_only BEFORE UPDATE OR DELETE ON effect_resolutions
    FOR EACH ROW EXECUTE FUNCTION fabric_effect_resolutions_append_only();

-- Outcome guard for every role (review O1 on 717bb09). The broker's own
-- moves stay open; what it settled stays settled:
--   - no effect is born resolved;
--   - QUARANTINED leaves only to RESOLVED_* with its audit row;
--   - INTENT, APPROVAL_REQUIRED or APPROVED becomes NOT_APPLIED only by an
--     audited abandon of a never-dispatched effect;
--   - CONFIRMED and RESOLVED_* never change state;
--   - FAILED_DEFINITE and NOT_APPLIED change only by Retry (to INTENT or
--     APPROVED);
--   - effects are never deleted.
-- Known limit: the app role still records readback outcomes (UNKNOWN or
-- DISPATCHED to a settled state) by a plain write; the reconciler is the app.
CREATE FUNCTION fabric_effects_outcome_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'effect % may not be deleted', OLD.id USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.state IN ('RESOLVED_APPLIED', 'RESOLVED_NOT_APPLIED') THEN
            RAISE EXCEPTION 'effect % may not be inserted %', NEW.id, NEW.state USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.state IS NOT DISTINCT FROM OLD.state THEN
        RETURN NEW;
    END IF;
    IF NEW.state IN ('RESOLVED_APPLIED', 'RESOLVED_NOT_APPLIED') THEN
        IF OLD.state <> 'QUARANTINED' OR NOT EXISTS (
            SELECT 1 FROM public.effect_resolutions WHERE effect_id = NEW.id AND to_state = NEW.state
                AND xact = txid_current()) THEN
            RAISE EXCEPTION 'effect % may enter % only from QUARANTINED with an audited resolution', NEW.id, NEW.state
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    -- An abandon: never dispatched, with its audit row from this transaction.
    IF OLD.state IN ('INTENT', 'APPROVAL_REQUIRED', 'APPROVED') AND NEW.state = 'NOT_APPLIED' THEN
        IF OLD.dispatches <> 0 OR NEW.dispatches <> 0 OR NOT EXISTS (
            SELECT 1 FROM public.effect_resolutions WHERE effect_id = NEW.id AND from_state = OLD.state
                AND to_state = 'NOT_APPLIED' AND xact = txid_current()) THEN
            RAISE EXCEPTION 'effect % may be abandoned only if never dispatched and with an audited reason (fab effect abandon)', NEW.id
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.state = 'QUARANTINED' THEN
        RAISE EXCEPTION 'quarantined effect % leaves only by an audited resolution (fab effect resolve)', NEW.id
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.state IN ('CONFIRMED', 'RESOLVED_APPLIED', 'RESOLVED_NOT_APPLIED') THEN
        RAISE EXCEPTION 'effect % is settled (%)', NEW.id, OLD.state USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.state IN ('FAILED_DEFINITE', 'NOT_APPLIED') AND NEW.state NOT IN ('INTENT', 'APPROVED') THEN
        RAISE EXCEPTION 'effect % is % and changes only by retry, not to %', NEW.id, OLD.state, NEW.state
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
-- Row triggers fire in name order: the zz_ names keep the runner guard
-- (0017-0019) first, so a runner refusal stays 42501.
CREATE TRIGGER effects_zz_outcome_guard BEFORE INSERT OR UPDATE OR DELETE ON effects
    FOR EACH ROW EXECUTE FUNCTION fabric_effects_outcome_guard();

-- Review O3 on 717bb09: a new intent and the move to DISPATCHED hold the
-- attempt row (FOR SHARE) and need it live with the effect's fence. Ending
-- the attempt then waits for this transaction, and COMPLETE_MISSION, which
-- needs no live attempt, counts the committed row. SECURITY DEFINER: the
-- runner has no UPDATE on attempts, which a locking read requires.
CREATE FUNCTION fabric_effects_hold_attempt() RETURNS trigger LANGUAGE plpgsql
SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    st text;
    fe bigint;
BEGIN
    IF (TG_OP = 'INSERT' AND NEW.state IN ('INTENT', 'APPROVAL_REQUIRED'))
       OR (TG_OP = 'UPDATE' AND NEW.state = 'DISPATCHED' AND OLD.state IS DISTINCT FROM 'DISPATCHED') THEN
        SELECT a.state, a.fence INTO st, fe FROM public.attempts a WHERE a.id = NEW.attempt_id FOR SHARE;
        IF st IS NULL OR st NOT IN ('ADMITTED', 'LEASED', 'PREPARING', 'RUNNING') OR fe <> NEW.fence THEN
            RAISE EXCEPTION 'effect % needs a live attempt % at fence % (attempt is %, fence %)',
                NEW.id, NEW.attempt_id, NEW.fence, coalesce(st, 'missing'), fe USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION fabric_effects_hold_attempt() FROM PUBLIC;
CREATE TRIGGER effects_zz_hold_attempt BEFORE INSERT OR UPDATE ON effects
    FOR EACH ROW EXECUTE FUNCTION fabric_effects_hold_attempt();

-- Review O4: the audit and the effects are append-only against TRUNCATE too
-- (the tests' reset opts in, 0010).
CREATE TRIGGER effect_resolutions_no_truncate BEFORE TRUNCATE ON effect_resolutions
    FOR EACH STATEMENT EXECUTE FUNCTION fabric_reject_truncate();
CREATE TRIGGER effects_no_truncate BEFORE TRUNCATE ON effects
    FOR EACH STATEMENT EXECUTE FUNCTION fabric_reject_truncate();
