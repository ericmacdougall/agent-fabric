-- 0011: canonical request bytes and immutable command history (Kimi P3.0,
-- kimi-p3-compare.md: byte-identical replay, request half; immutability).
--
-- A command row now keeps the exact bytes its request hash was taken over,
-- and the database checks the hash against them. request_format says which
-- rows predate this: LEGACY_HASH_ONLY rows (everything written before 0011)
-- have a hash and no request bytes; every new row is CANONICAL_V1 and must
-- carry both its request bytes and its response bytes. Roll-forward only.
ALTER TABLE commands
    ADD COLUMN canonical_request bytea,
    ADD COLUMN request_format    text NOT NULL DEFAULT 'LEGACY_HASH_ONLY';
ALTER TABLE commands ALTER COLUMN request_format SET DEFAULT 'CANONICAL_V1';
ALTER TABLE commands ADD CONSTRAINT commands_request_format_known
    CHECK (request_format IN ('LEGACY_HASH_ONLY', 'CANONICAL_V1'));
ALTER TABLE commands ADD CONSTRAINT commands_canonical_complete CHECK (
    CASE request_format
        WHEN 'CANONICAL_V1' THEN canonical_request IS NOT NULL AND response_bytes IS NOT NULL
                                 AND request_sha256 = encode(sha256(canonical_request), 'hex')
        ELSE canonical_request IS NULL
    END);

-- New rows are canonical: the legacy format exists only for rows that were
-- already here (a restore that must copy one disables triggers explicitly).
CREATE FUNCTION fabric_command_insert() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.request_format <> 'CANONICAL_V1' THEN
        RAISE EXCEPTION 'command %/%/% inserted in format %', NEW.actor, NEW.mission_id, NEW.command_id, NEW.request_format
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER commands_insert BEFORE INSERT ON commands
    FOR EACH ROW EXECUTE FUNCTION fabric_command_insert();

-- The receipt and the event log are history: the first response is the
-- answer forever, and events are only appended (fabric_reject_mutation and
-- fabric_reject_truncate come from 0009 and 0010).
CREATE TRIGGER commands_append_only BEFORE UPDATE OR DELETE ON commands
    FOR EACH ROW EXECUTE FUNCTION fabric_reject_mutation();
CREATE TRIGGER events_append_only BEFORE UPDATE OR DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION fabric_reject_mutation();
CREATE TRIGGER commands_no_truncate BEFORE TRUNCATE ON commands
    FOR EACH STATEMENT EXECUTE FUNCTION fabric_reject_truncate();
CREATE TRIGGER events_no_truncate BEFORE TRUNCATE ON events
    FOR EACH STATEMENT EXECUTE FUNCTION fabric_reject_truncate();
