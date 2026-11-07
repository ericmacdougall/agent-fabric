-- 0024: AC-12, the publish effect. Roll-forward only. Commutes with 0022
-- (agent_turns), which touches no effects function.
--
-- A publish whose precondition finds the remote branch head moved is
-- QUARANTINED with nothing sent (Broker.Dispatch on ErrPreconditionConflict),
-- and leaves only by an audited operator resolution. 0020 and 0021 are
-- published and not edited; this replaces two functions with the bodies of
-- 0020 and 0019 plus the marked AC-12 lines:
--   - fabric_effects_outcome_guard: a publish is born APPROVAL_REQUIRED, and
--     a publish may move INTENT or APPROVED to QUARANTINED. The existing
--     rules keep that move free of a dispatch count, a receipt and an
--     approval change. Other kinds keep review S5 (no never-sent
--     quarantine).
--   - fabric_effects_runner_guard: the same move for a runner on its own
--     live run, as for a failed precondition.
CREATE OR REPLACE FUNCTION fabric_effects_outcome_guard() RETURNS trigger LANGUAGE plpgsql
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
        -- AC-12: a publish always waits for an approval bound to its hash.
        IF NEW.kind = 'publish' AND NEW.state <> 'APPROVAL_REQUIRED' THEN
            RAISE EXCEPTION 'publish effect % must be born APPROVAL_REQUIRED (got %)', NEW.id, NEW.state
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
    -- AC-12: INTENT and APPROVED may also move to QUARANTINED for a publish.
    IF NOT (CASE OLD.state
        WHEN 'INTENT'            THEN NEW.state IN ('DISPATCHED', 'FAILED_DEFINITE', 'NOT_APPLIED')
                                      OR (NEW.state = 'QUARANTINED' AND OLD.kind = 'publish')
        WHEN 'APPROVAL_REQUIRED' THEN NEW.state IN ('APPROVED', 'NOT_APPLIED')
        WHEN 'APPROVED'          THEN NEW.state IN ('DISPATCHED', 'FAILED_DEFINITE', 'NOT_APPLIED')
                                      OR (NEW.state = 'QUARANTINED' AND OLD.kind = 'publish')
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

CREATE OR REPLACE FUNCTION fabric_effects_runner_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF public.fabric_is_scheduler() THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.state IN ('INTENT', 'APPROVAL_REQUIRED') AND NEW.approval_sha IS NULL AND NEW.approved_by IS NULL
           AND NEW.dispatches = 0 AND NEW.receipt IS NULL
           AND public.fabric_runner_run_live(NEW.mission_id, NEW.attempt_id, NEW.fence) THEN
            RETURN NEW;
        END IF;
    ELSIF (NEW.id, NEW.mission_id, NEW.attempt_id, NEW.fence, NEW.kind, NEW.subject, NEW.idem_key, NEW.request, NEW.request_sha, NEW.approval_sha, NEW.approved_by)
              IS NOT DISTINCT FROM (OLD.id, OLD.mission_id, OLD.attempt_id, OLD.fence, OLD.kind, OLD.subject, OLD.idem_key, OLD.request, OLD.request_sha, OLD.approval_sha, OLD.approved_by)
          AND public.fabric_runner_run_live(OLD.mission_id, OLD.attempt_id, OLD.fence) THEN
        -- Dispatch: one more transport generation.
        IF OLD.state IN ('INTENT', 'APPROVED') AND NEW.state = 'DISPATCHED' AND NEW.dispatches = OLD.dispatches + 1
           AND NEW.receipt IS NOT DISTINCT FROM OLD.receipt THEN
            RETURN NEW;
        END IF;
        -- A failed dispatch precondition.
        IF OLD.state IN ('INTENT', 'APPROVED') AND NEW.state = 'FAILED_DEFINITE' AND NEW.dispatches = OLD.dispatches
           AND NEW.receipt IS NOT DISTINCT FROM OLD.receipt THEN
            RETURN NEW;
        END IF;
        -- AC-12: a publish precondition that found the remote head moved.
        IF OLD.state IN ('INTENT', 'APPROVED') AND NEW.state = 'QUARANTINED' AND OLD.kind = 'publish'
           AND NEW.dispatches = OLD.dispatches AND NEW.receipt IS NOT DISTINCT FROM OLD.receipt THEN
            RETURN NEW;
        END IF;
        -- Settling a dispatch with the transport's answer.
        IF OLD.state = 'DISPATCHED' AND NEW.state IN ('CONFIRMED', 'FAILED_DEFINITE', 'UNKNOWN') AND NEW.dispatches = OLD.dispatches THEN
            RETURN NEW;
        END IF;
        -- Broker.Retry: an approval still binds the same request hash.
        IF OLD.state IN ('FAILED_DEFINITE', 'NOT_APPLIED')
           AND NEW.state = (CASE WHEN OLD.approval_sha IS NULL THEN 'INTENT' ELSE 'APPROVED' END)
           AND NEW.dispatches = OLD.dispatches AND NEW.receipt IS NOT DISTINCT FROM OLD.receipt THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'effect write (%) to % is not a runner step (role %)', TG_OP, NEW.state, current_user
        USING ERRCODE = 'insufficient_privilege';
END $$;
