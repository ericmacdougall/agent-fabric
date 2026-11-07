-- 0016: goodqa#14929. Roll-forward only.
--
-- fabd connects as a non-owner app role (Store.ApplyRoles), so the 0015
-- check also admits members of the scheduler role recorded here. The
-- setting is per database; app and runner roles may only read it.
CREATE TABLE fabric_settings (
    key   text PRIMARY KEY,
    value text NOT NULL
);

CREATE OR REPLACE FUNCTION fabric_scheduler_reserved_event() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    sched_role text;
BEGIN
    IF NEW.kind IN ('DISPATCH_DEFERRED', 'MISSION_PARKED')
       OR (NEW.kind = 'ATTEMPT_FINISHED' AND COALESCE((NEW.payload->>'prelaunch')::boolean, false)) THEN
        IF pg_has_role(current_user, (SELECT relowner FROM pg_class WHERE oid = 'events'::regclass), 'USAGE') THEN
            RETURN NEW;
        END IF;
        SELECT value INTO sched_role FROM fabric_settings WHERE key = 'scheduler_role';
        IF sched_role IS NOT NULL AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname = sched_role)
           AND pg_has_role(current_user, sched_role, 'MEMBER') THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'event % is reserved to the scheduler role, not %', NEW.kind, current_user
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END $$;
