-- 0022: native agent loop checkpoints (goodqa#15040, slice 1). Roll-forward only.
--
-- One conversation per attempt. Message content is bytea holding the exact
-- JSON bytes of the block array: jsonb would normalise key order and drop
-- duplicate keys, and a provider can reject a resent thinking block whose
-- bytes (signature included) changed. Messages are append-only.
CREATE TABLE agent_runs (
    attempt_id    text PRIMARY KEY REFERENCES attempts (id),
    fence         bigint NOT NULL,
    starts        int NOT NULL DEFAULT 0 CHECK (starts >= 0),
    config_sha    text NOT NULL,
    billing_mode  text NOT NULL CHECK (billing_mode IN ('api_key', 'subscription')),
    outcome       text,
    outcome_note  text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    ended_at      timestamptz
);

CREATE TABLE agent_messages (
    attempt_id   text NOT NULL REFERENCES attempts (id),
    idx          int NOT NULL CHECK (idx >= 0),
    role         text NOT NULL CHECK (role IN ('user', 'assistant')),
    content      bytea NOT NULL,
    fence        bigint NOT NULL,
    request_sha  text,
    request_ids  text[],
    stop_reason  text,
    model        text,
    usage        jsonb,
    attempts     jsonb,
    cost_micros  bigint,
    cost_label   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (attempt_id, idx),
    CHECK ((role = 'assistant') = (request_sha IS NOT NULL))
);

CREATE TABLE agent_tool_calls (
    attempt_id     text NOT NULL REFERENCES attempts (id),
    tool_use_id    text NOT NULL,
    msg_idx        int NOT NULL,
    name           text NOT NULL,
    input          bytea NOT NULL,
    side_effecting boolean NOT NULL,
    state          text NOT NULL DEFAULT 'PENDING' CHECK (state IN ('PENDING', 'EXECUTING', 'DONE', 'INTERRUPTED')),
    result         bytea,
    is_error       boolean,
    fence          bigint NOT NULL,
    started_at     timestamptz,
    ended_at       timestamptz,
    PRIMARY KEY (attempt_id, tool_use_id),
    FOREIGN KEY (attempt_id, msg_idx) REFERENCES agent_messages (attempt_id, idx),
    CHECK ((state IN ('DONE', 'INTERRUPTED')) = (result IS NOT NULL))
);

-- History is append-only: no update or delete of a message, ever, so no code
-- path (redaction included) can rewrite what was sent to or received from the
-- provider. Tests TRUNCATE ... CASCADE, which does not fire row triggers.
CREATE FUNCTION agent_messages_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'agent_messages is append-only (% refused)', TG_OP USING ERRCODE = 'restrict_violation';
END $$;
CREATE TRIGGER agent_messages_immutable BEFORE UPDATE OR DELETE ON agent_messages
    FOR EACH ROW EXECUTE FUNCTION agent_messages_immutable();

-- A tool call moves forward only: PENDING -> EXECUTING -> DONE | INTERRUPTED,
-- and a terminal call never changes (a re-run would double-apply an effect).
CREATE FUNCTION agent_tool_calls_forward() RETURNS trigger LANGUAGE plpgsql AS $$
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
         OR (OLD.state = 'PENDING' AND NEW.state = 'DONE' AND NOT OLD.side_effecting)) THEN
        RAISE EXCEPTION 'tool call % cannot move % -> %', OLD.tool_use_id, OLD.state, NEW.state USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER agent_tool_calls_forward BEFORE UPDATE OR DELETE ON agent_tool_calls
    FOR EACH ROW EXECUTE FUNCTION agent_tool_calls_forward();
