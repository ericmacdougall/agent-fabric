-- 0034: durable native-review wake hints and bounded redacted tool results.
-- The request/tool checkpoints remain authoritative; NOTIFY is only a hint.
-- Results are committed atomically with CONFIRMED or FAILED_DEFINITE so replay
-- never needs to execute a terminal effect again.

CREATE TABLE review_gate_results (
    request_hash      text PRIMARY KEY REFERENCES review_gate_requests (request_hash),
    dispatch_fence    bigint NOT NULL CHECK (dispatch_fence >= 1),
    dispatch_idem_key text NOT NULL CHECK (octet_length(dispatch_idem_key) BETWEEN 1 AND 256),
    outcome           text NOT NULL CHECK (outcome IN ('CONFIRMED', 'FAILED_DEFINITE')),
    result_format     text NOT NULL CHECK (result_format = 'redacted-bounded-v1'),
    output            bytea NOT NULL CHECK (octet_length(output) <= 65536),
    is_error          boolean NOT NULL,
    truncated         boolean NOT NULL,
    output_sha256     text NOT NULL CHECK (output_sha256 ~ '^[0-9a-f]{64}$'),
    created_at        timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- A result is born only with its exact terminal dispatch and is immutable.
-- The surrounding Go transaction updates the request and inserts this row
-- atomically; any result-storage failure leaves DISPATCHED for conservative
-- recovery, never a false CONFIRMED row without its return value.
CREATE FUNCTION fabric_review_gate_result_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'review gate results are insert-only' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM public.review_gate_requests r
        WHERE r.request_hash = NEW.request_hash AND r.state = NEW.outcome
          AND r.dispatch_fence = NEW.dispatch_fence
          AND r.dispatch_idem_key = NEW.dispatch_idem_key
    ) THEN
        RAISE EXCEPTION 'review result does not match its terminal dispatch' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER review_gate_result_guard BEFORE INSERT OR UPDATE OR DELETE ON review_gate_results
    FOR EACH ROW EXECUTE FUNCTION fabric_review_gate_result_guard();
CREATE TRIGGER review_gate_results_no_agent BEFORE INSERT OR UPDATE OR DELETE ON review_gate_results
    FOR EACH ROW EXECUTE FUNCTION fabric_review_gate_no_agent_write();
CREATE TRIGGER review_gate_results_no_truncate BEFORE TRUNCATE ON review_gate_results
    FOR EACH STATEMENT EXECUTE FUNCTION fabric_reject_truncate();

-- Result bytes are redacted, but remain tool output rather than general audit
-- metadata. Keep them away from catalog-wide runner reads, including after a
-- later ApplyRoles rerun (roles.go contains the equivalent explicit revoke).
CREATE FUNCTION fabric_review_gate_revoke_result_reads() RETURNS void LANGUAGE plpgsql
SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    role_name text;
BEGIN
    REVOKE SELECT ON public.review_gate_results FROM PUBLIC;
    FOR role_name IN SELECT rolname FROM pg_catalog.pg_roles WHERE rolname = 'agent' OR rolname ~ '(^|_)runner$' LOOP
        EXECUTE format('REVOKE SELECT ON public.review_gate_results FROM %I', role_name);
    END LOOP;
END $$;
REVOKE ALL ON FUNCTION fabric_review_gate_revoke_result_reads() FROM PUBLIC;
SELECT fabric_review_gate_revoke_result_reads();
