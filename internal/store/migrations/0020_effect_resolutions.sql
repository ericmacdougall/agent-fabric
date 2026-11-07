-- 0020: review N2 on aecf3fa. Roll-forward only.
--
-- The operator exit for a QUARANTINED effect: an append-only audit row
-- (who, why, evidence) and the effect moved to RESOLVED_APPLIED or
-- RESOLVED_NOT_APPLIED in the same transaction (Broker.ResolveQuarantined).
-- The runner role gets no write on this table (Store.ApplyRoles) and its
-- effects guard (0019) has no RESOLVED_* step.
CREATE TABLE effect_resolutions (
    id         bigserial PRIMARY KEY,
    effect_id  text NOT NULL REFERENCES effects (id),
    from_state text NOT NULL CHECK (from_state = 'QUARANTINED'),
    to_state   text NOT NULL CHECK (to_state IN ('RESOLVED_APPLIED', 'RESOLVED_NOT_APPLIED')),
    reason     text NOT NULL CHECK (reason <> ''),
    evidence   text NOT NULL CHECK (evidence <> ''),
    actor      text NOT NULL CHECK (actor <> ''),
    db_user    text NOT NULL DEFAULT current_user,
    at         timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX effect_resolutions_one_idx ON effect_resolutions (effect_id);

CREATE FUNCTION fabric_effect_resolutions_append_only() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    RAISE EXCEPTION 'effect_resolutions is append-only' USING ERRCODE = 'insufficient_privilege';
END $$;
CREATE TRIGGER effect_resolutions_append_only BEFORE UPDATE OR DELETE ON effect_resolutions
    FOR EACH ROW EXECUTE FUNCTION fabric_effect_resolutions_append_only();

-- Any move into RESOLVED_* must come from QUARANTINED with its audit row.
CREATE FUNCTION fabric_effects_resolution_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF NEW.state IN ('RESOLVED_APPLIED', 'RESOLVED_NOT_APPLIED') AND NEW.state IS DISTINCT FROM OLD.state THEN
        IF OLD.state <> 'QUARANTINED' OR NOT EXISTS (
            SELECT 1 FROM public.effect_resolutions WHERE effect_id = NEW.id AND to_state = NEW.state) THEN
            RAISE EXCEPTION 'effect % may enter % only from QUARANTINED with an audited resolution', NEW.id, NEW.state
                USING ERRCODE = 'check_violation';
        END IF;
    ELSIF OLD.state IN ('RESOLVED_APPLIED', 'RESOLVED_NOT_APPLIED') AND NEW.state IS DISTINCT FROM OLD.state THEN
        RAISE EXCEPTION 'effect % is resolved (%)', NEW.id, OLD.state USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER effects_resolution_guard BEFORE UPDATE ON effects
    FOR EACH ROW EXECUTE FUNCTION fabric_effects_resolution_guard();
