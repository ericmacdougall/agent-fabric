-- The application fence is not the only writer: an already-running old binary
-- must fail closed too. Native agent checkpoints may attach only to attempts
-- whose launcher path is anthropic-api (0026 already binds path to provider).
-- Do not install a guard over an already-violating row: classify it by hand.
-- Block concurrent writes before checking rows and installing the trigger.
LOCK TABLE public.agent_runs IN ACCESS EXCLUSIVE MODE;
DO $$
DECLARE
    bad text;
BEGIN
    SELECT r.attempt_id INTO bad
    FROM public.agent_runs r JOIN public.attempts a ON a.id = r.attempt_id
    WHERE a.path IS DISTINCT FROM 'anthropic-api'
    ORDER BY r.attempt_id
    LIMIT 1;
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION '0032: agent_runs attempt % has a non-anthropic path', bad
            USING ERRCODE = 'check_violation';
    END IF;
END $$;

CREATE FUNCTION public.fabric_agent_runs_provider() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    p text;
BEGIN
    SELECT a.path INTO p FROM public.attempts a WHERE a.id = NEW.attempt_id;
    IF p IS DISTINCT FROM 'anthropic-api' THEN
        RAISE EXCEPTION 'agent_runs attempt % has path %, want anthropic-api', NEW.attempt_id, p
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER agent_runs_provider BEFORE INSERT OR UPDATE OF attempt_id ON public.agent_runs
    FOR EACH ROW EXECUTE FUNCTION public.fabric_agent_runs_provider();
