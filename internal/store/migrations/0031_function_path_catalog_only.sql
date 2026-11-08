-- 0031: pin the reservation backstop functions to pg_catalog, pg_temp.
-- 0029_function_search_path gave every function "pg_catalog, public, pg_temp".
-- These trigger functions qualify every table with public., so they do not
-- need public on the path at all: nothing a role creates in public can then
-- shadow a catalog function they call. Bodies are unchanged from 0009/0010.
-- (The Kimi 0029 ledger REVOKE is already covered by 0029_function_search_path
-- and ApplyRoles, so it is not repeated here.)
CREATE OR REPLACE FUNCTION public.fabric_reject_mutation() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    RAISE EXCEPTION '% on % is not allowed (append-only)', TG_OP, TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $$;

CREATE OR REPLACE FUNCTION public.fabric_reservation_update() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF OLD.state <> 'HELD' THEN
        RAISE EXCEPTION 'reservation % is already %', OLD.id, OLD.state USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.id <> OLD.id OR NEW.hold_key <> OLD.hold_key OR NEW.attempt_id <> OLD.attempt_id OR NEW.fence <> OLD.fence
       OR NEW.resource <> OLD.resource OR NEW.unit <> OLD.unit OR NEW.amount <> OLD.amount
       OR NEW.generation <> OLD.generation OR NEW.expires_at <> OLD.expires_at OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'reservation % identity is immutable', OLD.id USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION public.fabric_capacity_agrees() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    r text := NEW.resource;
    h bigint;
    s numeric;
BEGIN
    SELECT held INTO h FROM public.resource_capacities WHERE resource = r;
    SELECT COALESCE(sum(amount), 0) INTO s FROM public.reservations WHERE resource = r AND state = 'HELD';
    IF h IS DISTINCT FROM s THEN
        RAISE EXCEPTION 'capacity % held=% but live holds sum to %', r, h, s USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION public.fabric_reservation_history() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM public.reservation_events WHERE reservation_id = NEW.id AND n = 1) THEN
        RAISE EXCEPTION 'reservation % has no HELD event', NEW.id USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.state <> 'HELD' AND NOT EXISTS (SELECT 1 FROM public.reservation_events WHERE reservation_id = NEW.id AND n = 2 AND state = NEW.state) THEN
        RAISE EXCEPTION 'reservation % is % without its terminal event', NEW.id, NEW.state USING ERRCODE = 'check_violation';
    END IF;
    IF (NEW.state = 'SETTLED') <> EXISTS (SELECT 1 FROM public.usage_receipts WHERE reservation_id = NEW.id) THEN
        RAISE EXCEPTION 'reservation % state % disagrees with its usage receipt', NEW.id, NEW.state USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION public.fabric_reservation_child_agrees() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    st text;
BEGIN
    SELECT state INTO st FROM public.reservations WHERE id = NEW.reservation_id;
    -- Nested IFs: plpgsql does not short-circuit AND, and NEW.n exists only
    -- on reservation_events.
    IF TG_TABLE_NAME = 'usage_receipts' THEN
        IF st IS DISTINCT FROM 'SETTLED' THEN
            RAISE EXCEPTION 'usage receipt for reservation % in state %', NEW.reservation_id, st USING ERRCODE = 'check_violation';
        END IF;
    ELSIF NEW.n = 2 AND st IS DISTINCT FROM NEW.state THEN
        RAISE EXCEPTION 'event % for reservation % in state %', NEW.state, NEW.reservation_id, st USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION public.fabric_reservation_insert() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    st text;
BEGIN
    IF NEW.state <> 'HELD' OR NEW.closed_at IS NOT NULL THEN
        RAISE EXCEPTION 'reservation % inserted in state %', NEW.id, NEW.state USING ERRCODE = 'check_violation';
    END IF;
    SELECT state INTO st FROM public.attempts WHERE id = NEW.attempt_id AND fence = NEW.fence;
    IF st IS NULL OR st NOT IN ('LEASED', 'PREPARING', 'RUNNING') THEN
        RAISE EXCEPTION 'reservation % for attempt % at fence % in state %', NEW.id, NEW.attempt_id, NEW.fence, st
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION public.fabric_reject_truncate() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF current_setting('fabric.allow_truncate', true) = 'on' THEN
        RETURN NULL;
    END IF;
    RAISE EXCEPTION 'TRUNCATE on % is not allowed (append-only)', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $$;
