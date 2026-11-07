-- 0025: goodqa#15082 F0. Host jobs are disabled. Roll-forward only.
--
-- 0018 let a runner INSERT a REQUESTED job for its own running attempt; the
-- scheduler then launched that argv outside the run's sandbox. The scheduler
-- no longer launches jobs, `fab db roles` no longer grants the runner INSERT
-- on jobs, and this guard refuses a runner INSERT even if a deployment
-- grants the privilege again by hand. 0017 and 0018 are published and not
-- edited; this replaces the function with 0018's body minus the INSERT
-- branch. A legacy watcher (an old binary still running) may still record
-- its job's exit: STARTED to EXITED with identity fields unchanged.
CREATE OR REPLACE FUNCTION fabric_jobs_runner_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF public.fabric_is_scheduler() THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.state = 'STARTED' AND NEW.state = 'EXITED' AND NEW.woken = OLD.woken
          AND (NEW.id, NEW.mission_id, NEW.attempt_id, NEW.fence, NEW.argv, NEW.rundir, NEW.unit, NEW.started_at)
              IS NOT DISTINCT FROM (OLD.id, OLD.mission_id, OLD.attempt_id, OLD.fence, OLD.argv, OLD.rundir, OLD.unit, OLD.started_at) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'job write reserved to the scheduler (role %)', current_user USING ERRCODE = 'insufficient_privilege';
END $$;
