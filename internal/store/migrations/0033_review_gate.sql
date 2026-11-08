-- 0033: default-deny agent tool review, durable human approval, fenced
-- dispatch, conservative outcomes and hash-chained audit. Roll-forward only.
-- Migrations 0032 and earlier remain unchanged.

CREATE TABLE review_gate_requests (
    request_hash       text PRIMARY KEY CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    mission_id         text NOT NULL CHECK (octet_length(mission_id) BETWEEN 1 AND 256),
    session_id         text NOT NULL CHECK (octet_length(session_id) BETWEEN 1 AND 256),
    tool               text NOT NULL CHECK (octet_length(tool) BETWEEN 1 AND 256),
    proposer_id        text NOT NULL CHECK (octet_length(proposer_id) BETWEEN 1 AND 256),
    policy_version     text NOT NULL CHECK (octet_length(policy_version) BETWEEN 1 AND 256),
    policy_digest      text NOT NULL CHECK (policy_digest ~ '^[0-9a-f]{64}$'),
    mode               text NOT NULL CHECK (mode IN ('shadow', 'evaluate', 'enforce')),
    canonical_proposal text NOT NULL CHECK (octet_length(canonical_proposal) BETWEEN 1 AND 1048575),
    state              text NOT NULL CHECK (state IN
        ('PENDING', 'DENIED', 'READY', 'AWAITING_HUMAN', 'APPROVED',
         'DISPATCHED', 'UNKNOWN', 'CONFIRMED', 'FAILED_DEFINITE')),
    risk_tier          int CHECK (risk_tier BETWEEN 0 AND 3),
    reversibility      text CHECK (reversibility IN ('no_side_effect', 'reversible', 'unknown', 'irreversible')),
    tainted            boolean,
    human_required     boolean,
    dispatch_idem_key  text UNIQUE CHECK (octet_length(dispatch_idem_key) BETWEEN 1 AND 256),
    dispatch_fence     bigint NOT NULL DEFAULT 0 CHECK (dispatch_fence >= 0),
    lease_until        timestamptz,
    created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at         timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX review_gate_requests_session_idx ON review_gate_requests (session_id, created_at);
CREATE INDEX review_gate_recovery_idx ON review_gate_requests (lease_until) WHERE state = 'DISPATCHED';

-- Multiple insert-only rows make renewal append-only. Only a live row matching
-- the request's complete policy digest can authorize APPROVED or dispatch.
CREATE TABLE review_gate_approvals (
    id             bigserial PRIMARY KEY,
    request_hash   text NOT NULL REFERENCES review_gate_requests (request_hash),
    operator_id    text NOT NULL CHECK (octet_length(operator_id) BETWEEN 1 AND 256),
    proposer_id    text NOT NULL CHECK (octet_length(proposer_id) BETWEEN 1 AND 256),
    policy_digest  text NOT NULL CHECK (policy_digest ~ '^[0-9a-f]{64}$'),
    expires_at     timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    db_user        text NOT NULL DEFAULT current_user,
    CHECK (operator_id <> proposer_id)
);
CREATE INDEX review_gate_approvals_live_idx ON review_gate_approvals (request_hash, expires_at DESC, id DESC);

CREATE TABLE review_gate_circuits (
    session_id    text PRIMARY KEY CHECK (octet_length(session_id) BETWEEN 1 AND 256),
    denial_count  bigint NOT NULL DEFAULT 0 CHECK (denial_count >= 0),
    open          boolean NOT NULL DEFAULT false,
    updated_at    timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE review_gate_audit_head (
    id        int PRIMARY KEY CHECK (id = 1),
    seq       bigint NOT NULL CHECK (seq >= 0),
    last_hash text NOT NULL CHECK (last_hash = '' OR last_hash ~ '^[0-9a-f]{64}$')
);
INSERT INTO review_gate_audit_head (id, seq, last_hash) VALUES (1, 0, '');

-- The audit stores operational metadata and hashes, never canonical args,
-- original tasks, model reasons, evidence bodies or executor result payloads.
CREATE TABLE review_gate_audit (
    seq            bigint PRIMARY KEY CHECK (seq > 0),
    request_hash   text NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    event          text NOT NULL CHECK (octet_length(event) BETWEEN 1 AND 128),
    actor          text NOT NULL CHECK (octet_length(actor) BETWEEN 1 AND 256),
    detail         text NOT NULL DEFAULT '' CHECK (octet_length(detail) <= 1024),
    policy_version text NOT NULL CHECK (octet_length(policy_version) BETWEEN 1 AND 256),
    at             timestamptz NOT NULL,
    prev_hash      text NOT NULL CHECK (prev_hash = '' OR prev_hash ~ '^[0-9a-f]{64}$'),
    hash           text NOT NULL UNIQUE CHECK (hash ~ '^[0-9a-f]{64}$')
);

-- Existing runner roles may have catalog-wide SELECT grants. Revoke plaintext
-- proposal reads for every current *_runner role. Store.ApplyRoles reruns the
-- equivalent narrow grant/revoke after later migrations; this migration does
-- not depend on editing roles.go. No canonical proposal is exposed by audit.
CREATE FUNCTION fabric_review_gate_revoke_runner_reads() RETURNS void LANGUAGE plpgsql
SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    role_name text;
BEGIN
    REVOKE SELECT ON public.review_gate_requests FROM PUBLIC;
    FOR role_name IN SELECT rolname FROM pg_catalog.pg_roles WHERE rolname = 'agent' OR rolname ~ '(^|_)runner$' LOOP
        EXECUTE format('REVOKE SELECT ON public.review_gate_requests FROM %I', role_name);
    END LOOP;
END $$;
REVOKE ALL ON FUNCTION fabric_review_gate_revoke_runner_reads() FROM PUBLIC;
SELECT fabric_review_gate_revoke_runner_reads();

-- roles.go is deliberately untouched. These guards fail closed if a runner
-- receives accidental write grants. The trusted app/broker authenticates the
-- separate human operator identity above the database.
CREATE FUNCTION fabric_review_gate_no_agent_write() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF current_user = 'agent' OR current_user ~ '(^|_)runner$' THEN
        RAISE EXCEPTION 'review gate write denied to agent/runner role %', current_user
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
END $$;

CREATE TRIGGER review_gate_requests_no_agent BEFORE INSERT OR UPDATE OR DELETE ON review_gate_requests
    FOR EACH ROW EXECUTE FUNCTION fabric_review_gate_no_agent_write();
CREATE TRIGGER review_gate_approvals_no_agent BEFORE INSERT OR UPDATE OR DELETE ON review_gate_approvals
    FOR EACH ROW EXECUTE FUNCTION fabric_review_gate_no_agent_write();
CREATE TRIGGER review_gate_circuits_no_agent BEFORE INSERT OR UPDATE OR DELETE ON review_gate_circuits
    FOR EACH ROW EXECUTE FUNCTION fabric_review_gate_no_agent_write();
CREATE TRIGGER review_gate_audit_no_agent BEFORE INSERT OR UPDATE OR DELETE ON review_gate_audit
    FOR EACH ROW EXECUTE FUNCTION fabric_review_gate_no_agent_write();
CREATE TRIGGER review_gate_audit_head_no_agent BEFORE INSERT OR UPDATE OR DELETE ON review_gate_audit_head
    FOR EACH ROW EXECUTE FUNCTION fabric_review_gate_no_agent_write();

CREATE FUNCTION fabric_review_gate_request_guard() RETURNS trigger LANGUAGE plpgsql
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
    -- The only same-state rewrite is a fresh-only heartbeat extension.
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
    IF NEW.state = 'READY' AND NEW.human_required THEN
        RAISE EXCEPTION 'READY cannot require a human' USING ERRCODE = 'check_violation';
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
    RETURN NEW;
END $$;
CREATE TRIGGER review_gate_request_state_guard BEFORE INSERT OR UPDATE OR DELETE ON review_gate_requests
    FOR EACH ROW EXECUTE FUNCTION fabric_review_gate_request_guard();

CREATE FUNCTION fabric_review_gate_approval_guard() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    proposer text;
    st text;
    digest text;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'human approvals are insert-only' USING ERRCODE = 'insufficient_privilege';
    END IF;
    SELECT r.proposer_id, r.state, r.policy_digest INTO proposer, st, digest FROM public.review_gate_requests r
        WHERE r.request_hash = NEW.request_hash;
    IF st IS NULL OR st <> 'AWAITING_HUMAN' OR proposer IS DISTINCT FROM NEW.proposer_id
       OR digest IS DISTINCT FROM NEW.policy_digest OR NEW.operator_id = NEW.proposer_id
       OR NEW.expires_at <= clock_timestamp() THEN
        RAISE EXCEPTION 'approval is not live, separate and bound to an awaiting request and policy' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER review_gate_approval_guard BEFORE INSERT OR UPDATE OR DELETE ON review_gate_approvals
    FOR EACH ROW EXECUTE FUNCTION fabric_review_gate_approval_guard();

CREATE FUNCTION fabric_review_gate_append_only() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = 'insufficient_privilege';
END $$;
CREATE TRIGGER review_gate_audit_append_only BEFORE UPDATE OR DELETE ON review_gate_audit
    FOR EACH ROW EXECUTE FUNCTION fabric_review_gate_append_only();
CREATE TRIGGER review_gate_approvals_append_only BEFORE UPDATE OR DELETE ON review_gate_approvals
    FOR EACH ROW EXECUTE FUNCTION fabric_review_gate_append_only();

CREATE TRIGGER review_gate_audit_no_truncate BEFORE TRUNCATE ON review_gate_audit
    FOR EACH STATEMENT EXECUTE FUNCTION fabric_reject_truncate();
CREATE TRIGGER review_gate_approvals_no_truncate BEFORE TRUNCATE ON review_gate_approvals
    FOR EACH STATEMENT EXECUTE FUNCTION fabric_reject_truncate();
CREATE TRIGGER review_gate_requests_no_truncate BEFORE TRUNCATE ON review_gate_requests
    FOR EACH STATEMENT EXECUTE FUNCTION fabric_reject_truncate();
CREATE TRIGGER review_gate_circuits_no_truncate BEFORE TRUNCATE ON review_gate_circuits
    FOR EACH STATEMENT EXECUTE FUNCTION fabric_reject_truncate();
CREATE TRIGGER review_gate_audit_head_no_truncate BEFORE TRUNCATE ON review_gate_audit_head
    FOR EACH STATEMENT EXECUTE FUNCTION fabric_reject_truncate();

-- A review-wrapped side-effecting call deliberately stays PENDING while the
-- gate owns its dispatch. It can settle directly to DONE only with a durable
-- outcome for the same attempt/tool, exact JSON args and loop-attached handle.
-- Ordinary side-effecting tools still require PENDING -> EXECUTING -> DONE.
CREATE OR REPLACE FUNCTION agent_tool_calls_forward() RETURNS trigger LANGUAGE plpgsql AS $$
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
END;
$$;
