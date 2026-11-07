-- 0017: red-team H1 on 193c0a1, and main's follow-up on jobs and effects.
-- Roll-forward only.
--
-- The runner role writes only jobs and effects (and inserts asks); see
-- Store.ApplyRoles. These triggers narrow those two tables to the
-- transitions a runner legitimately makes, so it cannot fake a state the
-- scheduler or an approver trusts. The owner and members of the recorded
-- scheduler role (fabd, fab) are not restricted here.
--
-- Every guard names its tables with their schema and pins search_path: any
-- role can create temporary tables, and pg_temp is searched first, so an
-- unqualified fabric_settings could be the caller's own.
CREATE FUNCTION fabric_is_scheduler() RETURNS boolean LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    r text;
BEGIN
    IF pg_has_role(current_user, (SELECT relowner FROM pg_catalog.pg_class WHERE oid = 'public.events'::regclass), 'USAGE') THEN
        RETURN true;
    END IF;
    SELECT value INTO r FROM public.fabric_settings WHERE key = 'scheduler_role';
    RETURN r IS NOT NULL AND EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = r) AND pg_has_role(current_user, r, 'MEMBER');
END $$;

-- A runner requests a job; its watcher records the job's exit. Starting,
-- failing to start and waking are the scheduler's (the scheduler also
-- checks the job's unit is gone before it wakes the mission).
CREATE FUNCTION fabric_jobs_runner_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF public.fabric_is_scheduler() THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.state = 'REQUESTED' AND NOT NEW.woken AND NEW.exit_code IS NULL AND NEW.ended_at IS NULL AND NEW.unit = '' THEN
            RETURN NEW;
        END IF;
    ELSIF OLD.state = 'STARTED' AND NEW.state = 'EXITED' AND NEW.woken = OLD.woken
          AND (NEW.id, NEW.mission_id, NEW.attempt_id, NEW.fence, NEW.argv, NEW.rundir, NEW.unit, NEW.started_at)
              IS NOT DISTINCT FROM (OLD.id, OLD.mission_id, OLD.attempt_id, OLD.fence, OLD.argv, OLD.rundir, OLD.unit, OLD.started_at) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'job write reserved to the scheduler (role %)', current_user USING ERRCODE = 'insufficient_privilege';
END $$;
CREATE TRIGGER jobs_runner_guard BEFORE INSERT OR UPDATE ON jobs
    FOR EACH ROW EXECUTE FUNCTION fabric_jobs_runner_guard();

-- A runner submits, dispatches and retries effects; it never approves one
-- and never changes what was requested or approved.
CREATE FUNCTION fabric_effects_runner_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF public.fabric_is_scheduler() THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.state IN ('INTENT', 'APPROVAL_REQUIRED') AND NEW.approval_sha IS NULL AND NEW.approved_by IS NULL THEN
            RETURN NEW;
        END IF;
    ELSIF (NEW.id, NEW.mission_id, NEW.attempt_id, NEW.fence, NEW.kind, NEW.subject, NEW.idem_key, NEW.request, NEW.request_sha, NEW.approval_sha, NEW.approved_by)
              IS NOT DISTINCT FROM (OLD.id, OLD.mission_id, OLD.attempt_id, OLD.fence, OLD.kind, OLD.subject, OLD.idem_key, OLD.request, OLD.request_sha, OLD.approval_sha, OLD.approved_by)
          AND (NEW.state <> 'APPROVED' OR OLD.state = 'APPROVED'
               -- Broker.Retry re-arms an approved effect; the approval still binds the same request hash.
               OR (OLD.state IN ('FAILED_DEFINITE', 'NOT_APPLIED') AND OLD.approval_sha IS NOT NULL)) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'effect approval is reserved to the scheduler and approvers (role %)', current_user USING ERRCODE = 'insufficient_privilege';
END $$;
CREATE TRIGGER effects_runner_guard BEFORE INSERT OR UPDATE ON effects
    FOR EACH ROW EXECUTE FUNCTION fabric_effects_runner_guard();

-- The 0016 reserved-event check had the same unqualified lookups; replace
-- it with the shared, pinned predicate.
CREATE OR REPLACE FUNCTION fabric_scheduler_reserved_event() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF NEW.kind IN ('DISPATCH_DEFERRED', 'MISSION_PARKED')
       OR (NEW.kind = 'ATTEMPT_FINISHED' AND COALESCE((NEW.payload->>'prelaunch')::boolean, false)) THEN
        IF public.fabric_is_scheduler() THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'event % is reserved to the scheduler role, not %', NEW.kind, current_user
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END $$;
