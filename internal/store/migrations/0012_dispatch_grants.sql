-- 0012: the final dispatch grant (C5; Kimi dispatch gate, adopted by intent).
--
-- After ACK_START and before any runner byte, the scheduler asks for one
-- grant per attempt. The grant is decided under the attempt and reservation
-- row locks with clock_timestamp() read after them (F3): the attempt is at
-- its fence, RUNNING, its lease unexpired, its intent revision current, and
-- every required reservation HELD and unexpired. The decision, GRANTED or
-- DENIED, is stored once and replayed; a launcher is started only on a
-- GRANTED row this scheduler inserted. Used only when the scheduler is
-- configured with holds; without them nothing writes here.
CREATE TABLE dispatch_grants (
    attempt_id   text PRIMARY KEY,
    fence        bigint NOT NULL,
    outcome      text NOT NULL CHECK (outcome IN ('GRANTED', 'DENIED')),
    reason       text NOT NULL DEFAULT '',
    reservations text[] NOT NULL DEFAULT '{}',
    decided_at   timestamptz NOT NULL,
    FOREIGN KEY (attempt_id, fence) REFERENCES attempts (id, fence),
    CHECK ((outcome = 'DENIED') = (reason <> '')),
    CHECK (outcome = 'DENIED' OR cardinality(reservations) > 0)
);
CREATE TRIGGER dispatch_grants_append_only BEFORE UPDATE OR DELETE ON dispatch_grants
    FOR EACH ROW EXECUTE FUNCTION fabric_reject_mutation();
CREATE TRIGGER dispatch_grants_no_truncate BEFORE TRUNCATE ON dispatch_grants
    FOR EACH STATEMENT EXECUTE FUNCTION fabric_reject_truncate();
