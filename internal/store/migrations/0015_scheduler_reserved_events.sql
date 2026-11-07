-- 0015: red-team N3 on 42c7bd2. Roll-forward only.
--
-- A pre-launch abandonment, a deferral and a park decide the park budget
-- and the backoff, so only the scheduler writes them. Store.Execute refuses
-- the flags from any actor but fabd; the actor is the caller's claim, so
-- this trigger also requires the writing role to have the privileges of the
-- role that owns the events table (the role fabd and the migrations run as).
-- A runner connects with its own role (sched Config.RunnerDSN) and is
-- refused. While runners share the owner's role, this check passes for
-- them: the gate refuses to run in that configuration.
CREATE FUNCTION fabric_scheduler_reserved_event() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.kind IN ('DISPATCH_DEFERRED', 'MISSION_PARKED')
        OR (NEW.kind = 'ATTEMPT_FINISHED' AND COALESCE((NEW.payload->>'prelaunch')::boolean, false)))
       AND NOT pg_has_role(current_user, (SELECT relowner FROM pg_class WHERE oid = 'events'::regclass), 'USAGE') THEN
        RAISE EXCEPTION 'event % is reserved to the scheduler role, not %', NEW.kind, current_user
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER events_scheduler_reserved BEFORE INSERT ON events
    FOR EACH ROW EXECUTE FUNCTION fabric_scheduler_reserved_event();
