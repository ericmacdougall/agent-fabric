-- 0036: separate trusted app review authority from the least-privilege runner.
-- Legacy 0035 brokers remain for owner compatibility, but runner execution is
-- revoked and every function also rejects a runner defensively. The runner's
-- only review operation settles one exact PENDING tool call from durable state.

CREATE FUNCTION fabric_review_gate_reject_runner() RETURNS void LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF session_user = 'agent' OR session_user ~ '(^|_)runner$'
       OR current_user = 'agent' OR current_user ~ '(^|_)runner$' THEN
        RAISE EXCEPTION 'trusted review broker denied to runner role %', session_user
            USING ERRCODE = 'insufficient_privilege';
    END IF;
END $$;
REVOKE ALL ON FUNCTION fabric_review_gate_reject_runner() FROM PUBLIC;

CREATE OR REPLACE FUNCTION fabric_review_gate_broker_load(p_hash text, p_lock boolean)
RETURNS SETOF public.review_gate_requests LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    PERFORM public.fabric_review_gate_reject_runner();
    IF p_hash !~ '^[0-9a-f]{64}$' THEN
        RAISE EXCEPTION 'invalid review request hash' USING ERRCODE = 'check_violation';
    END IF;
    IF p_lock THEN
        RETURN QUERY SELECT * FROM public.review_gate_requests WHERE request_hash = p_hash FOR UPDATE;
    ELSE
        RETURN QUERY SELECT * FROM public.review_gate_requests WHERE request_hash = p_hash;
    END IF;
END $$;

CREATE OR REPLACE FUNCTION fabric_review_gate_broker_lock_circuit(p_session text, p_threshold integer)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE v_open boolean;
BEGIN
    PERFORM public.fabric_review_gate_reject_runner();
    INSERT INTO public.review_gate_circuits (session_id) VALUES (p_session) ON CONFLICT DO NOTHING;
    SELECT open INTO v_open FROM public.review_gate_circuits WHERE session_id = p_session FOR UPDATE;
    RETURN v_open;
END $$;

CREATE OR REPLACE FUNCTION fabric_review_gate_broker_circuit_open(p_session text)
RETURNS boolean LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
    SELECT public.fabric_review_gate_reject_runner();
    SELECT coalesce((SELECT open FROM public.review_gate_circuits WHERE session_id = p_session), false)
$$;

CREATE OR REPLACE FUNCTION fabric_review_gate_broker_submit(
    p_hash text, p_mission text, p_session text, p_tool text, p_proposer text,
    p_policy_version text, p_policy_digest text, p_mode text, p_canonical text)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    PERFORM public.fabric_review_gate_reject_runner();
    INSERT INTO public.review_gate_requests
        (request_hash, mission_id, session_id, tool, proposer_id, policy_version,
         policy_digest, mode, canonical_proposal, state)
    VALUES (p_hash, p_mission, p_session, p_tool, p_proposer, p_policy_version,
            p_policy_digest, p_mode, p_canonical, 'PENDING')
    ON CONFLICT (request_hash) DO NOTHING;
    RETURN FOUND;
END $$;

CREATE OR REPLACE FUNCTION fabric_review_gate_broker_audit_head()
RETURNS TABLE(seq bigint, last_hash text, at timestamptz)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    PERFORM public.fabric_review_gate_reject_runner();
    RETURN QUERY SELECT h.seq, h.last_hash, clock_timestamp() FROM public.review_gate_audit_head h WHERE h.id = 1 FOR UPDATE;
END $$;

CREATE OR REPLACE FUNCTION fabric_review_gate_broker_audit_append(
    p_previous_seq bigint, p_request_hash text, p_event text, p_actor text,
    p_detail text, p_policy_version text, p_at timestamptz, p_prev_hash text, p_hash text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    PERFORM public.fabric_review_gate_reject_runner();
    IF p_event NOT IN ('SUBMITTED', 'MODEL_ALLOWED', 'HUMAN_REQUIRED', 'DENIED',
        'SHADOW_NO_EXECUTION', 'EVALUATE_NO_EXECUTION', 'DISPATCHED', 'STALE_NO_EXECUTION',
        'EXECUTION_UNKNOWN', 'EXECUTION_CONFIRMED', 'EXECUTION_FAILED_DEFINITE', 'RECOVERED_UNKNOWN') THEN
        RAISE EXCEPTION 'review audit event % is not a runner operation', p_event USING ERRCODE = 'insufficient_privilege';
    END IF;
    INSERT INTO public.review_gate_audit
        (seq, request_hash, event, actor, detail, policy_version, at, prev_hash, hash)
    VALUES (p_previous_seq + 1, p_request_hash, p_event, p_actor, p_detail,
            p_policy_version, p_at, p_prev_hash, p_hash);
    UPDATE public.review_gate_audit_head SET seq = p_previous_seq + 1, last_hash = p_hash
    WHERE id = 1 AND seq = p_previous_seq;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'review audit head changed' USING ERRCODE = 'serialization_failure';
    END IF;
END $$;

CREATE OR REPLACE FUNCTION fabric_review_gate_broker_evaluated(
    p_hash text, p_next text, p_tier integer, p_reversibility text,
    p_tainted boolean, p_human_required boolean)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    PERFORM public.fabric_review_gate_reject_runner();
    UPDATE public.review_gate_requests SET state = p_next, risk_tier = p_tier,
        reversibility = p_reversibility, tainted = p_tainted, human_required = p_human_required,
        updated_at = clock_timestamp()
    WHERE request_hash = p_hash AND state = 'PENDING' AND p_next IN ('READY', 'AWAITING_HUMAN');
END $$;

CREATE OR REPLACE FUNCTION fabric_review_gate_broker_denied(p_hash text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    PERFORM public.fabric_review_gate_reject_runner();
    UPDATE public.review_gate_requests SET state = 'DENIED', updated_at = clock_timestamp()
    WHERE request_hash = p_hash AND state IN ('PENDING', 'READY', 'AWAITING_HUMAN', 'APPROVED');
END $$;

CREATE OR REPLACE FUNCTION fabric_review_gate_broker_denial_count(p_session text, p_threshold integer)
RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE v_count bigint;
BEGIN
    PERFORM public.fabric_review_gate_reject_runner();
    INSERT INTO public.review_gate_circuits (session_id, denial_count, open, updated_at)
        VALUES (p_session, 1, p_threshold <= 1, clock_timestamp())
    ON CONFLICT (session_id) DO UPDATE SET denial_count = review_gate_circuits.denial_count + 1,
        open = review_gate_circuits.open OR review_gate_circuits.denial_count + 1 >= p_threshold,
        updated_at = clock_timestamp()
    RETURNING denial_count INTO v_count;
    RETURN v_count;
END $$;

CREATE OR REPLACE FUNCTION fabric_review_gate_broker_live_approval(p_hash text, p_digest text)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE v_live boolean;
BEGIN
    PERFORM public.fabric_review_gate_reject_runner();
    SELECT coalesce((SELECT a.expires_at > clock_timestamp() AND a.operator_id <> r.proposer_id
        FROM public.review_gate_approvals a
        JOIN public.review_gate_requests r ON r.request_hash = a.request_hash
        WHERE a.request_hash = p_hash AND a.policy_digest = p_digest
        ORDER BY a.expires_at DESC, a.id DESC LIMIT 1), false) INTO v_live;
    RETURN v_live;
END $$;

CREATE OR REPLACE FUNCTION fabric_review_gate_broker_dispatch(
    p_hash text, p_key text, p_fence bigint, p_lease_until timestamptz)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    PERFORM public.fabric_review_gate_reject_runner();
    UPDATE public.review_gate_requests SET state = 'DISPATCHED', dispatch_idem_key = p_key,
        dispatch_fence = p_fence, lease_until = p_lease_until, updated_at = clock_timestamp()
    WHERE request_hash = p_hash AND state IN ('READY', 'APPROVED');
END $$;

CREATE OR REPLACE FUNCTION fabric_review_gate_broker_heartbeat(
    p_hash text, p_key text, p_fence bigint, p_lease_micros bigint)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    PERFORM public.fabric_review_gate_reject_runner();
    UPDATE public.review_gate_requests
    SET lease_until = clock_timestamp() + p_lease_micros * interval '1 microsecond',
        updated_at = clock_timestamp()
    WHERE request_hash = p_hash AND dispatch_idem_key = p_key AND dispatch_fence = p_fence
      AND state = 'DISPATCHED' AND lease_until > clock_timestamp();
    RETURN FOUND;
END $$;

CREATE OR REPLACE FUNCTION fabric_review_gate_broker_transition(p_hash text, p_from text, p_to text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    PERFORM public.fabric_review_gate_reject_runner();
    IF NOT (p_from = 'DISPATCHED' AND p_to IN ('UNKNOWN', 'CONFIRMED', 'FAILED_DEFINITE')) THEN
        RAISE EXCEPTION 'refused review transition % -> %', p_from, p_to USING ERRCODE = 'check_violation';
    END IF;
    IF p_to = 'UNKNOWN' AND NOT EXISTS (
        SELECT 1 FROM public.review_gate_requests r WHERE r.request_hash = p_hash
          AND r.state = 'DISPATCHED' AND (r.lease_until IS NULL OR r.lease_until <= clock_timestamp())) THEN
        RAISE EXCEPTION 'a live dispatch cannot be recovered as UNKNOWN' USING ERRCODE = 'check_violation';
    END IF;
    UPDATE public.review_gate_requests SET state = p_to, lease_until = NULL,
        updated_at = clock_timestamp()
    WHERE request_hash = p_hash AND state = p_from;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'review request is not %', p_from USING ERRCODE = 'check_violation';
    END IF;
END $$;

CREATE OR REPLACE FUNCTION fabric_review_gate_broker_expired(p_hash text)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE v_expired boolean;
BEGIN
    PERFORM public.fabric_review_gate_reject_runner();
    SELECT lease_until IS NULL OR lease_until <= clock_timestamp() INTO v_expired
    FROM public.review_gate_requests WHERE request_hash = p_hash AND state = 'DISPATCHED';
    RETURN v_expired;
END $$;

CREATE OR REPLACE FUNCTION fabric_review_gate_broker_result_insert(
    p_hash text, p_fence bigint, p_key text, p_outcome text, p_output bytea,
    p_is_error boolean, p_truncated boolean, p_digest text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    PERFORM public.fabric_review_gate_reject_runner();
    INSERT INTO public.review_gate_results
        (request_hash, dispatch_fence, dispatch_idem_key, outcome, result_format,
         output, is_error, truncated, output_sha256)
    VALUES (p_hash, p_fence, p_key, p_outcome, 'redacted-bounded-v1',
            p_output, p_is_error, p_truncated, p_digest);
END $$;

CREATE OR REPLACE FUNCTION fabric_review_gate_broker_terminal_for_call(
    p_mission text, p_session text, p_tool text, p_args jsonb, p_call_id text)
RETURNS SETOF public.review_gate_requests LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    PERFORM public.fabric_review_gate_reject_runner();
    RETURN QUERY SELECT r.* FROM public.review_gate_requests r
    WHERE r.mission_id = p_mission AND r.session_id = p_session AND r.tool = p_tool
      AND r.canonical_proposal::jsonb->'args' = p_args
      AND r.canonical_proposal::jsonb->'provenance' @>
          jsonb_build_array(jsonb_build_object('id', p_call_id))
      AND r.state IN ('DENIED', 'UNKNOWN', 'CONFIRMED', 'FAILED_DEFINITE')
    ORDER BY r.created_at DESC, r.request_hash DESC LIMIT 1;
END $$;

CREATE OR REPLACE FUNCTION fabric_review_gate_broker_result_load(p_hash text)
RETURNS TABLE(output bytea, is_error boolean, truncated boolean, output_sha256 text)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    PERFORM public.fabric_review_gate_reject_runner();
    RETURN QUERY SELECT v.output, v.is_error, v.truncated, v.output_sha256
    FROM public.review_gate_results v
    JOIN public.review_gate_requests r ON r.request_hash = v.request_hash
    WHERE v.request_hash = p_hash AND r.state = v.outcome
      AND r.dispatch_fence = v.dispatch_fence AND r.dispatch_idem_key = v.dispatch_idem_key;
END $$;

CREATE OR REPLACE FUNCTION fabric_review_gate_broker_wait_state(p_hash text)
RETURNS TABLE(state text, waiting boolean) LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    PERFORM public.fabric_review_gate_reject_runner();
    RETURN QUERY SELECT r.state, CASE r.state
        WHEN 'AWAITING_HUMAN' THEN true
        WHEN 'APPROVED' THEN NOT EXISTS (
            SELECT 1 FROM public.review_gate_approvals a WHERE a.request_hash = r.request_hash
              AND a.policy_digest = r.policy_digest AND a.operator_id <> r.proposer_id
              AND a.expires_at > clock_timestamp())
        WHEN 'DISPATCHED' THEN r.lease_until IS NOT NULL AND r.lease_until > clock_timestamp()
        ELSE false END
    FROM public.review_gate_requests r WHERE r.request_hash = p_hash;
END $$;

CREATE OR REPLACE FUNCTION fabric_review_gate_broker_finish_tool(
    p_attempt text, p_fence bigint, p_tool_call text, p_result bytea, p_is_error boolean)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    PERFORM public.fabric_review_gate_reject_runner();
    RAISE EXCEPTION 'legacy reviewed-tool broker is disabled; use exact durable settlement'
        USING ERRCODE = 'insufficient_privilege';
END $$;

-- APPROVED shadow/evaluate requests become READY without changing identity,
-- risk or dispatch metadata. READY is the existing no-execution settlement
-- state accepted by agent_tool_calls_forward.
CREATE OR REPLACE FUNCTION fabric_review_gate_request_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'review gate requests may not be deleted' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.state <> 'PENDING' OR NEW.dispatch_fence <> 0 OR NEW.dispatch_idem_key IS NOT NULL
           OR NEW.lease_until IS NOT NULL OR NEW.risk_tier IS NOT NULL OR NEW.reversibility IS NOT NULL
           OR NEW.tainted IS NOT NULL OR NEW.human_required IS NOT NULL THEN
            RAISE EXCEPTION 'a review request is born unevaluated and undispatched' USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF (to_jsonb(NEW) - ARRAY['state', 'risk_tier', 'reversibility', 'tainted', 'human_required',
            'dispatch_idem_key', 'dispatch_fence', 'lease_until', 'updated_at'])
       IS DISTINCT FROM
       (to_jsonb(OLD) - ARRAY['state', 'risk_tier', 'reversibility', 'tainted', 'human_required',
            'dispatch_idem_key', 'dispatch_fence', 'lease_until', 'updated_at']) THEN
        RAISE EXCEPTION 'review request identity, proposal and policy are immutable' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.state = OLD.state THEN
        IF OLD.state = 'DISPATCHED' AND NEW.lease_until > OLD.lease_until AND NEW.lease_until > clock_timestamp()
           AND (to_jsonb(NEW) - ARRAY['lease_until', 'updated_at']) IS NOT DISTINCT FROM
               (to_jsonb(OLD) - ARRAY['lease_until', 'updated_at']) THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'review request % may not be rewritten in state %', NEW.request_hash, OLD.state
            USING ERRCODE = 'check_violation';
    END IF;
    IF NOT (CASE OLD.state
        WHEN 'PENDING'         THEN NEW.state IN ('DENIED', 'READY', 'AWAITING_HUMAN')
        WHEN 'AWAITING_HUMAN'  THEN NEW.state IN ('APPROVED', 'DENIED')
        WHEN 'READY'           THEN NEW.state IN ('DISPATCHED', 'DENIED')
        WHEN 'APPROVED'        THEN NEW.state IN ('AWAITING_HUMAN', 'DISPATCHED', 'DENIED')
                                  OR (NEW.state = 'READY' AND OLD.mode IN ('shadow', 'evaluate'))
        WHEN 'DISPATCHED'      THEN NEW.state IN ('UNKNOWN', 'CONFIRMED', 'FAILED_DEFINITE')
        WHEN 'UNKNOWN'         THEN NEW.state IN ('CONFIRMED', 'FAILED_DEFINITE')
        ELSE false
    END) THEN
        RAISE EXCEPTION 'review request may not move % -> %', OLD.state, NEW.state USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.state <> 'PENDING' AND (NEW.risk_tier, NEW.reversibility, NEW.tainted, NEW.human_required)
       IS DISTINCT FROM (OLD.risk_tier, OLD.reversibility, OLD.tainted, OLD.human_required) THEN
        RAISE EXCEPTION 'risk classification is immutable after evaluation' USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.state = 'PENDING' AND NEW.state IN ('READY', 'AWAITING_HUMAN')
       AND (NEW.risk_tier IS NULL OR NEW.reversibility IS NULL OR NEW.tainted IS NULL OR NEW.human_required IS NULL) THEN
        RAISE EXCEPTION 'evaluated review needs complete risk classification' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.state = 'READY' AND NEW.human_required
       AND NOT (OLD.state = 'APPROVED' AND OLD.mode IN ('shadow', 'evaluate')) THEN
        RAISE EXCEPTION 'READY cannot require a human before approval' USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.state = 'AWAITING_HUMAN' AND NEW.state = 'APPROVED' AND NOT EXISTS (
        SELECT 1 FROM public.review_gate_approvals a WHERE a.request_hash = NEW.request_hash
          AND a.policy_digest = NEW.policy_digest AND a.operator_id <> NEW.proposer_id
          AND a.expires_at > clock_timestamp()) THEN
        RAISE EXCEPTION 'APPROVED needs a live approval for the exact policy' USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.state = 'APPROVED' AND NEW.state = 'AWAITING_HUMAN' AND EXISTS (
        SELECT 1 FROM public.review_gate_approvals a WHERE a.request_hash = NEW.request_hash
          AND a.policy_digest = NEW.policy_digest AND a.expires_at > clock_timestamp()) THEN
        RAISE EXCEPTION 'a live approval cannot be renewed or rewritten' USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.state IN ('READY', 'APPROVED') AND NEW.state = 'DISPATCHED' THEN
        IF NEW.dispatch_idem_key IS NULL OR NEW.dispatch_fence <> OLD.dispatch_fence + 1 OR NEW.lease_until IS NULL THEN
            RAISE EXCEPTION 'dispatch needs an idempotency key, next fence and lease' USING ERRCODE = 'check_violation';
        END IF;
        IF OLD.state = 'APPROVED' AND NOT EXISTS (
            SELECT 1 FROM public.review_gate_approvals a WHERE a.request_hash = NEW.request_hash
              AND a.policy_digest = NEW.policy_digest AND a.operator_id <> NEW.proposer_id
              AND a.expires_at > clock_timestamp()) THEN
            RAISE EXCEPTION 'dispatch needs a live approval for the exact policy' USING ERRCODE = 'check_violation';
        END IF;
    ELSIF (NEW.dispatch_idem_key, NEW.dispatch_fence) IS DISTINCT FROM
          (OLD.dispatch_idem_key, OLD.dispatch_fence) THEN
        RAISE EXCEPTION 'dispatch fence and key change only at dispatch' USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.state = 'APPROVED' AND NEW.state = 'READY' AND NEW.lease_until IS DISTINCT FROM OLD.lease_until THEN
        RAISE EXCEPTION 'no-execution READY transition cannot change dispatch lease' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;

-- Exact runner settlement accepts no review hash, output bytes or error flag.
-- It derives the review from authoritative attempt/tool-call identity and copies
-- only the durable result (or a fixed metadata control outcome).
CREATE FUNCTION fabric_review_gate_finish_reviewed_tool(
    p_attempt text, p_fence bigint, p_tool_call text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    v_mission text; v_state text; v_path text; v_provider text; v_fresh boolean;
    v_name text; v_input bytea; v_msg_idx integer;
    v_hash text; v_review_state text; v_mode text;
    v_output bytea; v_is_error boolean; v_status text; v_result_digest text;
    v_result_fence bigint; v_result_key text; v_result_outcome text;
BEGIN
    IF NOT (session_user = 'agent' OR session_user ~ '(^|_)runner$'
            OR current_user = 'agent' OR current_user ~ '(^|_)runner$') THEN
        RAISE EXCEPTION 'exact reviewed-tool settlement requires direct runner authentication'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    SELECT a.mission_id, a.state, a.path, m.provider,
           a.lease_until IS NOT NULL AND a.lease_until > clock_timestamp()
      INTO v_mission, v_state, v_path, v_provider, v_fresh
    FROM public.attempts a JOIN public.missions m ON m.id = a.mission_id
    WHERE a.id = p_attempt AND a.fence = p_fence FOR SHARE OF a;
    IF NOT FOUND OR v_state NOT IN ('LEASED', 'PREPARING', 'RUNNING')
       OR v_path <> 'anthropic-api' OR v_provider <> 'anthropic-api' OR NOT v_fresh THEN
        RAISE EXCEPTION 'reviewed tool settlement has a stale or invalid attempt fence' USING ERRCODE = 'check_violation';
    END IF;

    SELECT c.name, c.input, c.msg_idx INTO v_name, v_input, v_msg_idx
    FROM public.agent_tool_calls c
    WHERE c.attempt_id = p_attempt AND c.tool_use_id = p_tool_call
      AND c.state = 'PENDING' AND c.fence = p_fence
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'reviewed tool call is not PENDING at the attempt fence' USING ERRCODE = 'check_violation';
    END IF;

    SELECT r.request_hash, r.state, r.mode INTO v_hash, v_review_state, v_mode
    FROM public.review_gate_requests r
    WHERE r.mission_id = v_mission AND r.session_id = p_attempt AND r.tool = v_name
      AND r.canonical_proposal::jsonb->'args' = convert_from(v_input, 'UTF8')::jsonb
      AND r.canonical_proposal::jsonb->'provenance' @>
          jsonb_build_array(jsonb_build_object('id', 'call-' || v_msg_idx || '-' || p_tool_call))
      AND (r.state IN ('DENIED', 'UNKNOWN', 'CONFIRMED', 'FAILED_DEFINITE')
           OR (r.state = 'READY' AND r.mode IN ('shadow', 'evaluate')))
    ORDER BY r.created_at DESC, r.request_hash DESC LIMIT 1;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no eligible durable review outcome for this exact tool call' USING ERRCODE = 'check_violation';
    END IF;

    v_is_error := true;
    IF v_review_state IN ('CONFIRMED', 'FAILED_DEFINITE') THEN
        SELECT v.output, v.is_error, v.dispatch_fence, v.dispatch_idem_key, v.outcome, v.output_sha256
          INTO v_output, v_is_error, v_result_fence, v_result_key, v_result_outcome, v_result_digest
        FROM public.review_gate_results v WHERE v.request_hash = v_hash;
        IF FOUND THEN
            IF v_result_digest IS DISTINCT FROM encode(sha256(v_output), 'hex') THEN
                RAISE EXCEPTION 'durable result output digest mismatch'
                    USING ERRCODE = 'check_violation';
            END IF;
            IF NOT EXISTS (SELECT 1 FROM public.review_gate_requests r
                WHERE r.request_hash = v_hash AND r.state = v_result_outcome
                  AND r.dispatch_fence = v_result_fence AND r.dispatch_idem_key = v_result_key) THEN
                RAISE EXCEPTION 'durable result does not match its request outcome and dispatch' USING ERRCODE = 'check_violation';
            END IF;
        ELSE
            v_status := CASE v_review_state WHEN 'CONFIRMED' THEN 'confirmed_result_unavailable' ELSE 'failed_definite' END;
        END IF;
    ELSIF v_review_state = 'DENIED' THEN
        v_status := 'denied';
    ELSIF v_review_state = 'UNKNOWN' THEN
        v_status := 'unknown_outcome';
    ELSIF v_review_state = 'READY' AND v_mode IN ('shadow', 'evaluate') THEN
        v_status := v_mode || '_no_execution';
    ELSE
        RAISE EXCEPTION 'review outcome is not eligible for runner settlement' USING ERRCODE = 'check_violation';
    END IF;
    IF v_status IS NOT NULL THEN
        v_output := convert_to(json_build_object(
            'status', v_status, 'request_hash', v_hash, 'mode', v_mode)::text, 'UTF8');
        v_is_error := true;
    END IF;

    UPDATE public.agent_tool_calls SET state = 'DONE', result = v_output,
        is_error = v_is_error, ended_at = clock_timestamp()
    WHERE attempt_id = p_attempt AND tool_use_id = p_tool_call AND state = 'PENDING' AND fence = p_fence;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'reviewed tool call changed before settlement' USING ERRCODE = 'check_violation';
    END IF;
END $$;

DO $$
DECLARE runner_name text; fn regprocedure;
BEGIN
    FOR fn IN SELECT p.oid::regprocedure FROM pg_catalog.pg_proc p
        JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
        WHERE n.nspname = 'public' AND p.proname LIKE 'fabric\_review\_gate\_broker\_%' LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', fn);
        FOR runner_name IN SELECT rolname FROM pg_catalog.pg_roles
            WHERE rolname = 'agent' OR rolname ~ '(^|_)runner$' LOOP
            EXECUTE format('REVOKE ALL ON FUNCTION %s FROM %I', fn, runner_name);
        END LOOP;
        FOR runner_name IN SELECT rolname FROM pg_catalog.pg_roles
            WHERE rolname ~ '(^|_)app$' LOOP
            EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO %I', fn, runner_name);
        END LOOP;
    END LOOP;
END $$;
REVOKE ALL ON FUNCTION fabric_review_gate_finish_reviewed_tool(text,bigint,text) FROM PUBLIC;
DO $$
DECLARE runner_name text;
BEGIN
    FOR runner_name IN SELECT rolname FROM pg_catalog.pg_roles
        WHERE rolname = 'agent' OR rolname ~ '(^|_)runner$' LOOP
        EXECUTE format('GRANT EXECUTE ON FUNCTION fabric_review_gate_finish_reviewed_tool(text,bigint,text) TO %I', runner_name);
    END LOOP;
END $$;
