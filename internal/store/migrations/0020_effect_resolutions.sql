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
--   - an effect is born a new intent (INTENT or APPROVAL_REQUIRED);
--   - columns outside an allowlist never change, and state moves follow an
--     allow-list of the broker's own steps;
--   - QUARANTINED leaves only to RESOLVED_* with its audit row;
--   - INTENT, APPROVAL_REQUIRED or APPROVED becomes NOT_APPLIED only by an
--     audited abandon of a never-dispatched effect;
--   - Retry keeps an approval: INTENT without one, APPROVED with one;
--   - effects are never deleted.
-- Known limit: the app role is the reconciler, so RECONCILING to a settled
-- state records whatever readback the app reports.
CREATE FUNCTION fabric_effects_outcome_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'effect % may not be deleted', OLD.id USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF TG_OP = 'INSERT' THEN
        -- Review Q4 on 3f38cc3: born in the initial state, never sent.
        IF NEW.state NOT IN ('INTENT', 'APPROVAL_REQUIRED') OR NEW.dispatches <> 0 OR NEW.receipt IS NOT NULL
           OR NEW.approval_sha IS NOT NULL OR NEW.approved_by IS NOT NULL THEN
            RAISE EXCEPTION 'effect % may be inserted only as a new intent (got %, dispatches %)', NEW.id, NEW.state, NEW.dispatches
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    -- Review Q1/Q2 on 3f38cc3 (main ruling): every column is immutable after
    -- insert except this allowlist, so a column added later is frozen too.
    IF (to_jsonb(NEW) - ARRAY['state', 'dispatches', 'receipt', 'approval_sha', 'approved_by', 'last_error', 'updated_at'])
       IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['state', 'dispatches', 'receipt', 'approval_sha', 'approved_by', 'last_error', 'updated_at']) THEN
        RAISE EXCEPTION 'effect % changed an immutable column', NEW.id USING ERRCODE = 'check_violation';
    END IF;
    -- dispatches: exactly +1, and only on the move into DISPATCHED.
    IF OLD.state IN ('INTENT', 'APPROVED') AND NEW.state = 'DISPATCHED' THEN
        IF NEW.dispatches <> OLD.dispatches + 1 THEN
            RAISE EXCEPTION 'effect % dispatch must count one (% -> %)', NEW.id, OLD.dispatches, NEW.dispatches
                USING ERRCODE = 'check_violation';
        END IF;
    ELSIF NEW.dispatches <> OLD.dispatches THEN
        RAISE EXCEPTION 'effect % dispatch count changes only by a dispatch', NEW.id USING ERRCODE = 'check_violation';
    END IF;
    -- The approval is set once, by approve, bound to the request hash.
    IF (NEW.approval_sha, NEW.approved_by) IS DISTINCT FROM (OLD.approval_sha, OLD.approved_by)
       AND NOT (OLD.state = 'APPROVAL_REQUIRED' AND NEW.state = 'APPROVED' AND OLD.approval_sha IS NULL
                AND NEW.approval_sha = NEW.request_sha AND coalesce(NEW.approved_by, '') <> '') THEN
        RAISE EXCEPTION 'effect % approval changes only by approve', NEW.id USING ERRCODE = 'check_violation';
    END IF;
    -- The receipt is written while the outcome is open, then frozen.
    IF NEW.receipt IS DISTINCT FROM OLD.receipt AND OLD.state NOT IN ('DISPATCHED', 'UNKNOWN', 'RECONCILING') THEN
        RAISE EXCEPTION 'effect % receipt is settled (%)', NEW.id, OLD.state USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.state IS NOT DISTINCT FROM OLD.state THEN
        -- Review S6: the diagnostic note is frozen with the outcome.
        IF NEW.last_error IS DISTINCT FROM OLD.last_error
           AND OLD.state IN ('CONFIRMED', 'FAILED_DEFINITE', 'NOT_APPLIED', 'QUARANTINED', 'RESOLVED_APPLIED', 'RESOLVED_NOT_APPLIED') THEN
            RAISE EXCEPTION 'effect % is % and its record is frozen', NEW.id, OLD.state USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    -- Review S1-S5 on 4ae0565: state moves are an allow-list mirroring the
    -- broker (Submit, Approve, Dispatch, settle, Reconcile, Retry, resolve,
    -- abandon). Anything it does not name is refused for every role.
    IF NOT (CASE OLD.state
        WHEN 'INTENT'            THEN NEW.state IN ('DISPATCHED', 'FAILED_DEFINITE', 'NOT_APPLIED')
        WHEN 'APPROVAL_REQUIRED' THEN NEW.state IN ('APPROVED', 'NOT_APPLIED')
        WHEN 'APPROVED'          THEN NEW.state IN ('DISPATCHED', 'FAILED_DEFINITE', 'NOT_APPLIED')
        WHEN 'DISPATCHED'        THEN NEW.state IN ('CONFIRMED', 'FAILED_DEFINITE', 'UNKNOWN')
        WHEN 'UNKNOWN'           THEN NEW.state = 'RECONCILING'
        WHEN 'RECONCILING'       THEN NEW.state IN ('CONFIRMED', 'NOT_APPLIED', 'QUARANTINED', 'UNKNOWN')
        WHEN 'FAILED_DEFINITE'   THEN NEW.state = (CASE WHEN OLD.approval_sha IS NULL THEN 'INTENT' ELSE 'APPROVED' END)
        WHEN 'NOT_APPLIED'       THEN NEW.state = (CASE WHEN OLD.approval_sha IS NULL THEN 'INTENT' ELSE 'APPROVED' END)
        WHEN 'QUARANTINED'       THEN NEW.state IN ('RESOLVED_APPLIED', 'RESOLVED_NOT_APPLIED')
        ELSE false
    END) THEN
        RAISE EXCEPTION 'effect % may not move % -> %', NEW.id, OLD.state, NEW.state USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.state IN ('RESOLVED_APPLIED', 'RESOLVED_NOT_APPLIED') THEN
        IF NOT EXISTS (
            SELECT 1 FROM public.effect_resolutions WHERE effect_id = NEW.id AND to_state = NEW.state
                AND xact = txid_current()) THEN
            RAISE EXCEPTION 'effect % may enter % only with an audited resolution (fab effect resolve)', NEW.id, NEW.state
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    -- An abandon: never dispatched, with its audit row from this transaction.
    IF OLD.state IN ('INTENT', 'APPROVAL_REQUIRED', 'APPROVED') AND NEW.state = 'NOT_APPLIED' THEN
        IF OLD.dispatches <> 0 OR NEW.dispatches <> 0 OR NOT EXISTS (
            SELECT 1 FROM public.effect_resolutions WHERE effect_id = NEW.id AND from_state = OLD.state
                AND to_state = 'NOT_APPLIED' AND xact = txid_current()) THEN
            RAISE EXCEPTION 'effect % may be abandoned only if never dispatched and with an audited reason (fab effect abandon)', NEW.id
                USING ERRCODE = 'check_violation';
        END IF;
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
    am text;
    term text;
BEGIN
    IF (TG_OP = 'INSERT' AND NEW.state IN ('INTENT', 'APPROVAL_REQUIRED'))
       OR (TG_OP = 'UPDATE' AND NEW.state = 'DISPATCHED' AND OLD.state IS DISTINCT FROM 'DISPATCHED') THEN
        -- Review Q3: the effect's own mission, not terminal; missions before
        -- attempts, the order Store.Execute locks them in.
        SELECT m.terminal INTO term FROM public.missions m WHERE m.id = NEW.mission_id FOR SHARE;
        IF term IS DISTINCT FROM 'NONE' THEN
            RAISE EXCEPTION 'effect % mission % is %', NEW.id, NEW.mission_id, coalesce(term, 'missing')
                USING ERRCODE = 'check_violation';
        END IF;
        SELECT a.state, a.fence, a.mission_id INTO st, fe, am FROM public.attempts a WHERE a.id = NEW.attempt_id FOR SHARE;
        IF st IS NULL OR st NOT IN ('ADMITTED', 'LEASED', 'PREPARING', 'RUNNING') OR fe <> NEW.fence OR am <> NEW.mission_id THEN
            RAISE EXCEPTION 'effect % needs a live attempt % of mission % at fence % (attempt is %, fence %, mission %)',
                NEW.id, NEW.attempt_id, NEW.mission_id, NEW.fence, coalesce(st, 'missing'), fe, am USING ERRCODE = 'check_violation';
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
