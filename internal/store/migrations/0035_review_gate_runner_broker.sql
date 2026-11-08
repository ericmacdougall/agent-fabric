-- 0035: least-privilege native-runner review broker.
-- Runners execute fixed, hash-bound operations; they receive no review-table
-- SELECT or DML grants. Every function has a fixed safe search_path.

CREATE FUNCTION fabric_review_gate_broker_load(p_hash text, p_lock boolean)
RETURNS SETOF public.review_gate_requests LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF p_hash !~ '^[0-9a-f]{64}$' THEN
        RAISE EXCEPTION 'invalid review request hash' USING ERRCODE = 'check_violation';
    END IF;
    IF p_lock THEN
        RETURN QUERY SELECT * FROM public.review_gate_requests WHERE request_hash = p_hash FOR UPDATE;
    ELSE
        RETURN QUERY SELECT * FROM public.review_gate_requests WHERE request_hash = p_hash;
    END IF;
END $$;

CREATE FUNCTION fabric_review_gate_broker_lock_circuit(p_session text, p_threshold integer)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE v_open boolean;
BEGIN
    INSERT INTO public.review_gate_circuits (session_id) VALUES (p_session) ON CONFLICT DO NOTHING;
    SELECT open INTO v_open FROM public.review_gate_circuits WHERE session_id = p_session FOR UPDATE;
    RETURN v_open;
END $$;

CREATE FUNCTION fabric_review_gate_broker_circuit_open(p_session text)
RETURNS boolean LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
    SELECT coalesce((SELECT open FROM public.review_gate_circuits WHERE session_id = p_session), false)
$$;

CREATE FUNCTION fabric_review_gate_broker_submit(
    p_hash text, p_mission text, p_session text, p_tool text, p_proposer text,
    p_policy_version text, p_policy_digest text, p_mode text, p_canonical text)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    INSERT INTO public.review_gate_requests
        (request_hash, mission_id, session_id, tool, proposer_id, policy_version,
         policy_digest, mode, canonical_proposal, state)
    VALUES (p_hash, p_mission, p_session, p_tool, p_proposer, p_policy_version,
            p_policy_digest, p_mode, p_canonical, 'PENDING')
    ON CONFLICT (request_hash) DO NOTHING;
    RETURN FOUND;
END $$;

CREATE FUNCTION fabric_review_gate_broker_audit_head()
RETURNS TABLE(seq bigint, last_hash text, at timestamptz)
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
    SELECT h.seq, h.last_hash, clock_timestamp() FROM public.review_gate_audit_head h WHERE h.id = 1 FOR UPDATE
$$;

CREATE FUNCTION fabric_review_gate_broker_audit_append(
    p_previous_seq bigint, p_request_hash text, p_event text, p_actor text,
    p_detail text, p_policy_version text, p_at timestamptz, p_prev_hash text, p_hash text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
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

CREATE FUNCTION fabric_review_gate_broker_evaluated(
    p_hash text, p_next text, p_tier integer, p_reversibility text,
    p_tainted boolean, p_human_required boolean)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
    UPDATE public.review_gate_requests SET state = p_next, risk_tier = p_tier,
        reversibility = p_reversibility, tainted = p_tainted, human_required = p_human_required,
        updated_at = clock_timestamp()
    WHERE request_hash = p_hash AND state = 'PENDING' AND p_next IN ('READY', 'AWAITING_HUMAN')
$$;

CREATE FUNCTION fabric_review_gate_broker_denied(p_hash text)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
    UPDATE public.review_gate_requests SET state = 'DENIED', updated_at = clock_timestamp()
    WHERE request_hash = p_hash AND state IN ('PENDING', 'READY', 'AWAITING_HUMAN', 'APPROVED')
$$;

CREATE FUNCTION fabric_review_gate_broker_denial_count(p_session text, p_threshold integer)
RETURNS bigint LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
    INSERT INTO public.review_gate_circuits (session_id, denial_count, open, updated_at)
        VALUES (p_session, 1, p_threshold <= 1, clock_timestamp())
    ON CONFLICT (session_id) DO UPDATE SET denial_count = review_gate_circuits.denial_count + 1,
        open = review_gate_circuits.open OR review_gate_circuits.denial_count + 1 >= p_threshold,
        updated_at = clock_timestamp()
    RETURNING denial_count
$$;

CREATE FUNCTION fabric_review_gate_broker_live_approval(p_hash text, p_digest text)
RETURNS boolean LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
    SELECT coalesce((SELECT a.expires_at > clock_timestamp() AND a.operator_id <> r.proposer_id
        FROM public.review_gate_approvals a
        JOIN public.review_gate_requests r ON r.request_hash = a.request_hash
        WHERE a.request_hash = p_hash AND a.policy_digest = p_digest
        ORDER BY a.expires_at DESC, a.id DESC LIMIT 1), false)
$$;

CREATE FUNCTION fabric_review_gate_broker_dispatch(
    p_hash text, p_key text, p_fence bigint, p_lease_until timestamptz)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
    UPDATE public.review_gate_requests SET state = 'DISPATCHED', dispatch_idem_key = p_key,
        dispatch_fence = p_fence, lease_until = p_lease_until, updated_at = clock_timestamp()
    WHERE request_hash = p_hash AND state IN ('READY', 'APPROVED')
$$;

CREATE FUNCTION fabric_review_gate_broker_heartbeat(
    p_hash text, p_key text, p_fence bigint, p_lease_micros bigint)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    UPDATE public.review_gate_requests
    SET lease_until = clock_timestamp() + p_lease_micros * interval '1 microsecond',
        updated_at = clock_timestamp()
    WHERE request_hash = p_hash AND dispatch_idem_key = p_key AND dispatch_fence = p_fence
      AND state = 'DISPATCHED' AND lease_until > clock_timestamp();
    RETURN FOUND;
END $$;

CREATE FUNCTION fabric_review_gate_broker_transition(p_hash text, p_from text, p_to text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
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

CREATE FUNCTION fabric_review_gate_broker_expired(p_hash text)
RETURNS boolean LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
    SELECT lease_until IS NULL OR lease_until <= clock_timestamp()
    FROM public.review_gate_requests WHERE request_hash = p_hash AND state = 'DISPATCHED'
$$;

CREATE FUNCTION fabric_review_gate_broker_result_insert(
    p_hash text, p_fence bigint, p_key text, p_outcome text, p_output bytea,
    p_is_error boolean, p_truncated boolean, p_digest text)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
    INSERT INTO public.review_gate_results
        (request_hash, dispatch_fence, dispatch_idem_key, outcome, result_format,
         output, is_error, truncated, output_sha256)
    VALUES (p_hash, p_fence, p_key, p_outcome, 'redacted-bounded-v1',
            p_output, p_is_error, p_truncated, p_digest)
$$;

CREATE FUNCTION fabric_review_gate_broker_terminal_for_call(
    p_mission text, p_session text, p_tool text, p_args jsonb, p_call_id text)
RETURNS SETOF public.review_gate_requests LANGUAGE sql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp AS $$
    SELECT r.* FROM public.review_gate_requests r
    WHERE r.mission_id = p_mission AND r.session_id = p_session AND r.tool = p_tool
      AND r.canonical_proposal::jsonb->'args' = p_args
      AND r.canonical_proposal::jsonb->'provenance' @>
          jsonb_build_array(jsonb_build_object('id', p_call_id))
      AND r.state IN ('DENIED', 'UNKNOWN', 'CONFIRMED', 'FAILED_DEFINITE')
    ORDER BY r.created_at DESC, r.request_hash DESC LIMIT 1
$$;

CREATE FUNCTION fabric_review_gate_broker_result_load(p_hash text)
RETURNS TABLE(output bytea, is_error boolean, truncated boolean, output_sha256 text)
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
    SELECT v.output, v.is_error, v.truncated, v.output_sha256
    FROM public.review_gate_results v
    JOIN public.review_gate_requests r ON r.request_hash = v.request_hash
    WHERE v.request_hash = p_hash AND r.state = v.outcome
      AND r.dispatch_fence = v.dispatch_fence AND r.dispatch_idem_key = v.dispatch_idem_key
$$;

CREATE FUNCTION fabric_review_gate_broker_wait_state(p_hash text)
RETURNS TABLE(state text, waiting boolean) LANGUAGE sql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp AS $$
    SELECT r.state, CASE r.state
        WHEN 'AWAITING_HUMAN' THEN true
        WHEN 'APPROVED' THEN NOT EXISTS (
            SELECT 1 FROM public.review_gate_approvals a WHERE a.request_hash = r.request_hash
              AND a.policy_digest = r.policy_digest AND a.operator_id <> r.proposer_id
              AND a.expires_at > clock_timestamp())
        WHEN 'DISPATCHED' THEN r.lease_until IS NOT NULL AND r.lease_until > clock_timestamp()
        ELSE false END
    FROM public.review_gate_requests r WHERE r.request_hash = p_hash
$$;

CREATE FUNCTION fabric_review_gate_broker_finish_tool(
    p_attempt text, p_fence bigint, p_tool_call text, p_result bytea, p_is_error boolean)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE v_state text; v_path text; v_provider text; v_fresh boolean;
BEGIN
    SELECT a.state, a.path, m.provider, a.lease_until IS NOT NULL AND a.lease_until > now()
      INTO v_state, v_path, v_provider, v_fresh
    FROM public.attempts a JOIN public.missions m ON m.id = a.mission_id
    WHERE a.id = p_attempt AND a.fence = p_fence FOR SHARE OF a;
    IF NOT FOUND OR v_state NOT IN ('LEASED', 'PREPARING', 'RUNNING')
       OR v_path <> 'anthropic-api' OR v_provider <> 'anthropic-api' OR NOT v_fresh THEN
        RAISE EXCEPTION 'reviewed tool settlement has a stale or invalid attempt fence' USING ERRCODE = 'check_violation';
    END IF;
    UPDATE public.agent_tool_calls SET state = 'DONE', result = p_result,
        is_error = p_is_error, ended_at = now()
    WHERE attempt_id = p_attempt AND tool_use_id = p_tool_call AND state = 'PENDING' AND fence = p_fence;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'reviewed tool call is not PENDING' USING ERRCODE = 'check_violation';
    END IF;
END $$;

-- The trigger reads review authority internally; this does not grant its caller
-- SELECT on that authority. All identity and transition checks remain intact.
CREATE OR REPLACE FUNCTION agent_tool_calls_forward() RETURNS trigger LANGUAGE plpgsql
SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'agent_tool_calls rows are never deleted' USING ERRCODE = 'restrict_violation';
    END IF;
    IF OLD.state IN ('DONE', 'INTERRUPTED') THEN
        RAISE EXCEPTION 'tool call % is already %', OLD.tool_use_id, OLD.state USING ERRCODE = 'restrict_violation';
    END IF;
    IF (NEW.attempt_id, NEW.tool_use_id, NEW.msg_idx, NEW.name, NEW.input, NEW.side_effecting)
       IS DISTINCT FROM (OLD.attempt_id, OLD.tool_use_id, OLD.msg_idx, OLD.name, OLD.input, OLD.side_effecting) THEN
        RAISE EXCEPTION 'tool call identity is immutable' USING ERRCODE = 'restrict_violation';
    END IF;
    IF NOT ((OLD.state = 'PENDING' AND NEW.state = 'EXECUTING')
         OR (OLD.state = 'EXECUTING' AND NEW.state IN ('DONE', 'INTERRUPTED'))
         OR (OLD.state = 'PENDING' AND NEW.state = 'DONE' AND NOT OLD.side_effecting)
         OR (OLD.state = 'PENDING' AND NEW.state = 'DONE' AND OLD.side_effecting AND EXISTS (
             SELECT 1 FROM public.review_gate_requests r
             WHERE r.session_id = NEW.attempt_id AND r.tool = NEW.name
               AND r.canonical_proposal::jsonb->'args' = convert_from(NEW.input, 'UTF8')::jsonb
               AND r.canonical_proposal::jsonb->'provenance' @>
                   jsonb_build_array(jsonb_build_object('id', 'call-' || NEW.msg_idx || '-' || NEW.tool_use_id))
               AND (r.state IN ('DENIED', 'UNKNOWN', 'CONFIRMED', 'FAILED_DEFINITE')
                    OR (r.state = 'READY' AND r.mode IN ('shadow', 'evaluate')))))) THEN
        RAISE EXCEPTION 'tool call % cannot move % -> %', OLD.tool_use_id, OLD.state, NEW.state USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END $$;

-- Remove PUBLIC and inherited effective reads, not just direct runner grants.
DO $$
DECLARE runner_name text; parent_name text; fn regprocedure;
BEGIN
    REVOKE SELECT ON public.review_gate_requests FROM PUBLIC;
    REVOKE SELECT ON public.review_gate_results FROM PUBLIC;
    FOR runner_name IN SELECT rolname FROM pg_catalog.pg_roles WHERE rolname = 'agent' OR rolname ~ '(^|_)runner$' LOOP
        EXECUTE format('REVOKE SELECT ON public.review_gate_requests FROM %I', runner_name);
        EXECUTE format('REVOKE SELECT ON public.review_gate_results FROM %I', runner_name);
        FOR parent_name IN SELECT r.rolname FROM pg_catalog.pg_roles r
            WHERE r.rolname <> runner_name AND pg_has_role(runner_name, r.rolname, 'MEMBER') LOOP
            EXECUTE format('REVOKE SELECT ON public.review_gate_requests FROM %I', parent_name);
            EXECUTE format('REVOKE SELECT ON public.review_gate_results FROM %I', parent_name);
        END LOOP;
    END LOOP;
    FOR fn IN SELECT p.oid::regprocedure FROM pg_catalog.pg_proc p
        JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
        WHERE n.nspname = 'public' AND p.proname LIKE 'fabric\_review\_gate\_broker\_%' LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', fn);
        -- Trusted broker authority is app-only even at this intermediate
        -- migration. A fresh installation must never expose the legacy runner
        -- lifecycle while later migrations are still being applied.
        FOR runner_name IN SELECT rolname FROM pg_catalog.pg_roles
            WHERE rolname ~ '(^|_)app$' LOOP
            EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO %I', fn, runner_name);
        END LOOP;
    END LOOP;
END $$;
