-- 0008: exact replay bytes (Kimi P3.0, kimi-p3-comparison item 1).
-- jsonb normalises key order, whitespace and number spelling, so a replay
-- served from `response` is equal in meaning but not in bytes. The first
-- response is now also kept verbatim and a replay returns those bytes.
-- Rows written before 0008 have NULL here and replay from `response`.
ALTER TABLE commands ADD COLUMN response_bytes bytea;
-- The two forms must never disagree: a row whose bytes do not parse to the
-- jsonb it sits next to is refused, so a replay cannot serve one answer to
-- a reader of `response` and another to the caller.
ALTER TABLE commands ADD CONSTRAINT commands_response_bytes_agree
    CHECK (response_bytes IS NULL OR convert_from(response_bytes, 'UTF8')::jsonb = response);
