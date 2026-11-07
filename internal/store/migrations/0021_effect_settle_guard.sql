-- Review T1/T2 on e35d78e, in addition to 0020's effects guard (published, so
-- not edited). A separate trigger, named to fire after effects_zz_outcome_guard.
--   T1: DISPATCHED settles to CONFIRMED or FAILED_DEFINITE only with a new
--       receipt whose outcome says so, which only the dispatcher writes. A
--       crash-left DISPATCHED row cannot be relabelled FAILED_DEFINITE and
--       sent again; it leaves by UNKNOWN and a readback (Broker.Reconcile
--       once its attempt is no longer live).
--   T2: an abandoned effect (an audited move to NOT_APPLIED) is final; a
--       retry would lose its approval requirement.
CREATE FUNCTION fabric_effects_settle_guard() RETURNS trigger LANGUAGE plpgsql
SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF OLD.state = 'DISPATCHED' AND NEW.state IN ('CONFIRMED', 'FAILED_DEFINITE')
       AND (NEW.receipt IS NULL OR NEW.receipt IS NOT DISTINCT FROM OLD.receipt
            OR NEW.receipt->>'outcome' IS DISTINCT FROM NEW.state) THEN
        RAISE EXCEPTION 'effect % may settle % only with a new receipt saying so', NEW.id, NEW.state
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.state = 'NOT_APPLIED' AND NEW.state IS DISTINCT FROM 'NOT_APPLIED' AND EXISTS (
        SELECT 1 FROM public.effect_resolutions r WHERE r.effect_id = NEW.id AND r.to_state = 'NOT_APPLIED') THEN
        RAISE EXCEPTION 'effect % was abandoned and is final', NEW.id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION fabric_effects_settle_guard() FROM PUBLIC;
CREATE TRIGGER effects_zz_settle_guard BEFORE UPDATE ON effects
    FOR EACH ROW EXECUTE FUNCTION fabric_effects_settle_guard();
