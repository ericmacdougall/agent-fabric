-- goodqa#15087: every mission records its provider, and every attempt the
-- launcher path that started it.
--   claude-code:   the official Claude Code binary, on the user's own login;
--   anthropic-api: the native loop, on an API key.
-- The launcher states its path (attempts.path); the trigger below checks it
-- against the mission, so a scheduler cannot start a native mission and the
-- native runner cannot start a claude-code one, whichever binary runs.

ALTER TABLE missions ADD COLUMN provider text;
ALTER TABLE attempts ADD COLUMN path text;
-- The native runner's liveness lock file, resolved once by the runner and
-- read back by the prober (never recomputed from the prober's environment).
ALTER TABLE attempts ADD COLUMN lock_path text;

-- Native evidence (main WAL wa725ee58353, w4c9ba86dc0d): an agent_runs row
-- for one of the mission's attempts, or any command the native runner sent.
CREATE TEMP TABLE fabric_0026_native ON COMMIT DROP AS
SELECT m.id
FROM missions m
WHERE EXISTS (SELECT 1 FROM attempts a JOIN agent_runs r ON r.attempt_id = a.id WHERE a.mission_id = m.id)
   OR EXISTS (SELECT 1 FROM commands c WHERE c.mission_id = m.id AND c.actor = 'native-runner');

-- A native mission that some other actor also started an attempt on cannot
-- be classified safely: stop, and let an operator decide.
DO $$
DECLARE
    bad text;
BEGIN
    SELECT n.id INTO bad
    FROM fabric_0026_native n
    WHERE EXISTS (SELECT 1 FROM commands c
                  WHERE c.mission_id = n.id AND c.kind = 'START_ATTEMPT' AND c.actor <> 'native-runner')
    ORDER BY n.id
    LIMIT 1;
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION '0026: mission % has native evidence and an attempt started by another actor; classify it by hand before migrating', bad
            USING ERRCODE = 'check_violation';
    END IF;
END $$;

UPDATE missions
SET provider = CASE WHEN id IN (SELECT id FROM fabric_0026_native) THEN 'anthropic-api' ELSE 'claude-code' END;

UPDATE attempts a
SET path = m.provider
FROM missions m
WHERE m.id = a.mission_id;

ALTER TABLE missions
    ALTER COLUMN provider SET NOT NULL,
    ADD CONSTRAINT missions_provider_check CHECK (provider IN ('claude-code', 'anthropic-api'));

ALTER TABLE attempts
    ALTER COLUMN path SET NOT NULL,
    ADD CONSTRAINT attempts_path_check CHECK (path IN ('claude-code', 'anthropic-api')),
    -- Only a native attempt has a lock file, and it must be this attempt's own
    -- file: absolute, no "." or ".." segment, no empty segment, ending in
    -- /native-locks/<id>.lock. right() with equality, not LIKE, so an id
    -- holding % or _ cannot widen the match.
    ADD CONSTRAINT attempts_lock_path_check CHECK (
        lock_path IS NULL
        OR (path = 'anthropic-api'
            AND left(lock_path, 1) = '/'
            AND lock_path !~ '(^|/)\.{1,2}(/|$)'
            AND strpos(lock_path, '//') = 0
            AND right(lock_path, length('/native-locks/' || id || '.lock')) = '/native-locks/' || id || '.lock'));

-- The triggers. None is SECURITY DEFINER: each runs as the caller, which
-- already reads missions. search_path is pinned and names are qualified, so
-- a caller's own schema cannot shadow them.

CREATE FUNCTION public.fabric_missions_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'mission % cannot be deleted (goodqa#15087)', OLD.id USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.provider IS DISTINCT FROM OLD.provider THEN
        RAISE EXCEPTION 'mission % provider is immutable (% -> %)', OLD.id, OLD.provider, NEW.provider
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER missions_guard BEFORE UPDATE OF provider OR DELETE ON public.missions
    FOR EACH ROW EXECUTE FUNCTION public.fabric_missions_guard();

-- TRUNCATE (a row trigger never sees one). The tests' reset opts in with
-- fabric.allow_truncate, but any role can SET a custom setting, so the
-- opt-in counts only for the session user that owns the table exactly
-- (main WAL wbcccc52bcd3). The app and runner roles never own it; role
-- membership does not count.
CREATE FUNCTION public.fabric_missions_no_truncate() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF current_setting('fabric.allow_truncate', true) = 'on'
       AND EXISTS (SELECT 1 FROM pg_catalog.pg_class c
                   WHERE c.oid = 'public.missions'::pg_catalog.regclass
                     AND pg_catalog.pg_get_userbyid(c.relowner) = session_user) THEN
        RETURN NULL;
    END IF;
    RAISE EXCEPTION 'TRUNCATE on missions is not allowed (goodqa#15087)' USING ERRCODE = 'restrict_violation';
END $$;
CREATE TRIGGER missions_no_truncate BEFORE TRUNCATE ON public.missions
    FOR EACH STATEMENT EXECUTE FUNCTION public.fabric_missions_no_truncate();

CREATE FUNCTION public.fabric_attempts_path() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    p text;
BEGIN
    SELECT m.provider INTO p FROM public.missions m WHERE m.id = NEW.mission_id;
    IF NEW.path IS DISTINCT FROM p THEN
        RAISE EXCEPTION 'attempt % path % differs from mission % provider %', NEW.id, NEW.path, NEW.mission_id, p
            USING ERRCODE = 'check_violation';
    END IF;
    IF TG_OP = 'INSERT' AND NEW.path = 'anthropic-api' AND NEW.lock_path IS NULL THEN
        RAISE EXCEPTION 'native attempt % has no lock_path', NEW.id USING ERRCODE = 'check_violation';
    END IF;
    IF TG_OP = 'UPDATE' AND NEW.lock_path IS DISTINCT FROM OLD.lock_path THEN
        RAISE EXCEPTION 'attempt % lock_path is immutable', NEW.id USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER attempts_path BEFORE INSERT OR UPDATE OF path, mission_id, lock_path ON public.attempts
    FOR EACH ROW EXECUTE FUNCTION public.fabric_attempts_path();
