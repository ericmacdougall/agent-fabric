-- 0018: red-team J1-J4 and the job nit on eef7ca7. Roll-forward only.
--
-- 0017 pinned what a runner may change; this pins which transitions it may
-- make. A runner writes only for its own live run (an attempt RUNNING
-- under the same mission and fence), and only the broker's runner-side
-- steps: submit, dispatch, settle a dispatch, and retry. Approval,
-- readback (RECONCILING, NOT_APPLIED, QUARANTINED) and answers belong to
-- the app role. The role is shared by every runner on a host, so "own run"
-- means any running attempt, not this process's.
CREATE FUNCTION fabric_runner_run_live(m text, a text, f bigint) RETURNS boolean LANGUAGE sql STABLE
SET search_path = pg_catalog, pg_temp AS $$
    SELECT EXISTS (SELECT 1 FROM public.attempts WHERE id = a AND mission_id = m AND fence = f AND state = 'RUNNING')
$$;

CREATE OR REPLACE FUNCTION fabric_jobs_runner_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF public.fabric_is_scheduler() THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.state = 'REQUESTED' AND NOT NEW.woken AND NEW.exit_code IS NULL AND NEW.ended_at IS NULL AND NEW.unit = ''
           AND public.fabric_runner_run_live(NEW.mission_id, NEW.attempt_id, NEW.fence) THEN
            RETURN NEW;
        END IF;
    ELSIF OLD.state = 'STARTED' AND NEW.state = 'EXITED' AND NEW.woken = OLD.woken
          AND (NEW.id, NEW.mission_id, NEW.attempt_id, NEW.fence, NEW.argv, NEW.rundir, NEW.unit, NEW.started_at)
              IS NOT DISTINCT FROM (OLD.id, OLD.mission_id, OLD.attempt_id, OLD.fence, OLD.argv, OLD.rundir, OLD.unit, OLD.started_at) THEN
        -- The job's watcher may outlive the run; the scheduler reads the
        -- unit back before it wakes the mission.
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'job write reserved to the scheduler (role %)', current_user USING ERRCODE = 'insufficient_privilege';
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
              IS NOT DISTINCT FROM (OLD.id, OLD.mission_id, OLD.attempt_id, OLD.fence, OLD.kind, OLD.subject, OLD.idem_key, OLD.request, OLD.request_sha, OLD.approval_sha, OLD.approved_by) THEN
        -- Dispatch: one more transport generation, for a live run.
        IF OLD.state IN ('INTENT', 'APPROVED') AND NEW.state = 'DISPATCHED' AND NEW.dispatches = OLD.dispatches + 1
           AND NEW.receipt IS NOT DISTINCT FROM OLD.receipt
           AND public.fabric_runner_run_live(OLD.mission_id, OLD.attempt_id, OLD.fence) THEN
            RETURN NEW;
        END IF;
        -- A failed dispatch precondition.
        IF OLD.state IN ('INTENT', 'APPROVED') AND NEW.state = 'FAILED_DEFINITE' AND NEW.dispatches = OLD.dispatches
           AND NEW.receipt IS NOT DISTINCT FROM OLD.receipt THEN
            RETURN NEW;
        END IF;
        -- Settling a dispatch with the transport's answer. The runner holds
        -- the executor, so its receipt is its own claim (see README).
        IF OLD.state = 'DISPATCHED' AND NEW.state IN ('CONFIRMED', 'FAILED_DEFINITE', 'UNKNOWN') AND NEW.dispatches = OLD.dispatches THEN
            RETURN NEW;
        END IF;
        -- Broker.Retry: an approval still binds the same request hash.
        IF OLD.state IN ('FAILED_DEFINITE', 'NOT_APPLIED')
           AND NEW.state = (CASE WHEN OLD.approval_sha IS NULL THEN 'INTENT' ELSE 'APPROVED' END)
           AND NEW.dispatches = OLD.dispatches AND NEW.receipt IS NOT DISTINCT FROM OLD.receipt
           AND public.fabric_runner_run_live(OLD.mission_id, OLD.attempt_id, OLD.fence) THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'effect write (%) to % is not a runner step (role %)', TG_OP, NEW.state, current_user
        USING ERRCODE = 'insufficient_privilege';
END $$;

-- A runner raises open asks for a running mission; answering and
-- withdrawing are the app role's (the runner has no UPDATE on asks).
CREATE FUNCTION fabric_asks_runner_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF public.fabric_is_scheduler() THEN
        RETURN NEW;
    END IF;
    IF NEW.state = 'open' AND NEW.answer IS NULL AND NEW.answered_by IS NULL AND NEW.answered_at IS NULL
       AND NEW.answer_source IS NULL AND NEW.withdrawn_reason IS NULL AND NEW.mission_id IS NOT NULL
       AND EXISTS (SELECT 1 FROM public.attempts WHERE mission_id = NEW.mission_id AND state = 'RUNNING') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'only an open ask for a running mission may be raised by role %', current_user
        USING ERRCODE = 'insufficient_privilege';
END $$;
CREATE TRIGGER asks_runner_guard BEFORE INSERT OR UPDATE ON asks
    FOR EACH ROW EXECUTE FUNCTION fabric_asks_runner_guard();
