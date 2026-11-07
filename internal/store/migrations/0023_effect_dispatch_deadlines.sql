-- Review nit 4 on cbebbd6: each dispatch stamps its transport deadline, so
-- every reconciler judges staleness by the deadline the dispatcher used,
-- not by its own timeout setting. Append-only; one row per (effect,
-- dispatch), written in the same transaction as the move to DISPATCHED.
CREATE TABLE effect_dispatches (
    effect_id text NOT NULL REFERENCES effects (id),
    dispatch  integer NOT NULL,
    deadline  timestamptz NOT NULL,
    at        timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (effect_id, dispatch)
);

CREATE FUNCTION fabric_effect_dispatches_check() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'effect_dispatches is append-only' USING ERRCODE = 'insufficient_privilege';
    END IF;
    -- Review X1: clock_timestamp, the time of this insert, as the window start.
    NEW.at := clock_timestamp();
    -- Only for the dispatch just committed, with a bounded deadline (a far
    -- deadline would keep an effect from ever being reconciled).
    IF NOT EXISTS (SELECT 1 FROM public.effects e WHERE e.id = NEW.effect_id AND e.state = 'DISPATCHED'
                   AND e.dispatches = NEW.dispatch) THEN
        RAISE EXCEPTION 'effect % is not at dispatch %', NEW.effect_id, NEW.dispatch USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.deadline < clock_timestamp() - interval '5 seconds' OR NEW.deadline > clock_timestamp() + interval '15 minutes' THEN
        RAISE EXCEPTION 'dispatch deadline % out of range', NEW.deadline USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER effect_dispatches_check BEFORE INSERT OR UPDATE OR DELETE ON effect_dispatches
    FOR EACH ROW EXECUTE FUNCTION fabric_effect_dispatches_check();
CREATE TRIGGER effect_dispatches_no_truncate BEFORE TRUNCATE ON effect_dispatches
    FOR EACH STATEMENT EXECUTE FUNCTION fabric_reject_truncate();
