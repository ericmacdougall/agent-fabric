-- goodqa#15283: a native (anthropic-api) attempt records the canonical
-- worktree it started in, and a resume or replay from any other worktree is
-- refused by the runner. This migration is the database backstop.
--
-- Safe to re-apply: every statement is IF NOT EXISTS, CREATE OR REPLACE, or a
-- drop and re-create by name. The column has no default, so ADD COLUMN is a
-- metadata-only change. The database does not require a value: a binary that
-- predates 0038 inserts attempts without the column and stores NULL, which the
-- runner refuses to resume (ErrWorktreeUnbound). Roll forward only; the
-- fabric_schema forward guard is unchanged, so an older binary refuses a
-- database that holds this migration.

-- Migrate applies each file in its own transaction, so SET LOCAL holds for
-- this migration only. If a running binary holds a lock on attempts, the
-- migration fails after 5 seconds (SQLSTATE 55P03) instead of queueing every
-- other attempts reader and writer behind its ACCESS EXCLUSIVE request.
-- Retry once the holder is gone (red-team K7).
SET LOCAL lock_timeout = '5s';

ALTER TABLE public.attempts ADD COLUMN IF NOT EXISTS worktree text;

-- domain.ValidWorktree mirrors this check. A NULL passes. A value is allowed
-- only on an anthropic-api attempt, and must be absolute and not "/", with no
-- "." or ".." segment, no empty segment, no trailing "/", no control
-- character (C0, DEL and C1; Postgres text cannot hold NUL at all), and at
-- most 4096 bytes. Nothing here folds case.
ALTER TABLE public.attempts DROP CONSTRAINT IF EXISTS attempts_worktree_check;
ALTER TABLE public.attempts ADD CONSTRAINT attempts_worktree_check CHECK (
    worktree IS NULL
    OR (path = 'anthropic-api'
        AND left(worktree, 1) = '/'
        AND worktree <> '/'
        AND right(worktree, 1) <> '/'
        AND worktree !~ '(^|/)\.{1,2}(/|$)'
        AND strpos(worktree, '//') = 0
        AND worktree !~ '[\u0001-\u001f\u007f-\u009f]'
        AND octet_length(worktree) <= 4096));

-- Immutability: a plain BEFORE UPDATE trigger with no column list, so it runs
-- on every UPDATE of a row, whatever columns the statement names. IS DISTINCT
-- FROM covers NULL to a value, one value to another, and a value to NULL.
-- Not SECURITY DEFINER; search_path is pinned like every other function.
CREATE OR REPLACE FUNCTION public.fabric_attempts_worktree_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF NEW.worktree IS DISTINCT FROM OLD.worktree THEN
        RAISE EXCEPTION 'attempt % worktree is immutable', OLD.id USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS attempts_worktree_immutable ON public.attempts;
CREATE TRIGGER attempts_worktree_immutable BEFORE UPDATE ON public.attempts
    FOR EACH ROW EXECUTE FUNCTION public.fabric_attempts_worktree_immutable();
