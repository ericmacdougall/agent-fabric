-- 0019: red-team K1/K2 on 4d8e1de. Roll-forward only.
--
-- Settling a dispatch now also needs the effect's run to be live. A
-- DISPATCHED effect left by a run that ended (a crash mid-transport) is
-- ambiguous and goes to the app role's readback, not to a runner. The role
-- is still shared, so a runner can settle another running mission's
-- dispatch (README known limits).
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
