-- goodqa#15361 N3 mode C: client-side compaction rows. One append-only row per
-- compaction of a native attempt's history: the summary text and where the
-- kept history starts. The summary is a recorded effect: a resume and a replay
-- read the row and never regenerate it.
--
-- Safe to re-apply (IF NOT EXISTS, CREATE OR REPLACE, drop and re-create by
-- name). A new table: nothing locks existing rows. Roll forward only; the
-- forward guard refuses an older binary on a database that holds this file.
SET LOCAL lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS public.agent_compactions (
    attempt_id     text NOT NULL REFERENCES public.attempts (id),
    -- idx is the message index at which the compaction took effect (the next
    -- message to be appended); first_kept_idx is the oldest message sent
    -- verbatim after the summary.
    idx            int NOT NULL CHECK (idx >= 1),
    first_kept_idx int NOT NULL CHECK (first_kept_idx >= 1 AND first_kept_idx <= idx),
    summary        text NOT NULL CHECK (length(summary) > 0),
    request_sha    text NOT NULL,
    cost_micros    bigint NOT NULL CHECK (cost_micros >= 0),
    fence          bigint NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (attempt_id, idx)
);

CREATE OR REPLACE FUNCTION public.agent_compactions_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'agent_compactions is append-only (% refused)', TG_OP USING ERRCODE = 'restrict_violation';
END $$;
DROP TRIGGER IF EXISTS agent_compactions_immutable ON public.agent_compactions;
CREATE TRIGGER agent_compactions_immutable BEFORE UPDATE OR DELETE ON public.agent_compactions
    FOR EACH ROW EXECUTE FUNCTION public.agent_compactions_immutable();
