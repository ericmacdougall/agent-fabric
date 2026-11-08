-- Kimi review follow-ups (goodqa#15159). Safe to apply twice.
--
-- K6 (goodqa#15164): a function with no search_path of its own resolves
-- unqualified tables through the caller's path, where pg_temp comes first,
-- so a client's temporary table could shadow the table a trigger or
-- backstop reads. Every function in the schema now searches pg_temp last.
-- Functions that already pin a path ending in pg_temp keep it.
-- TestEveryFunctionPinsSearchPath holds later migrations to the same rule.
DO $do$
DECLARE
    fn regprocedure;
BEGIN
    FOR fn IN
        SELECT p.oid::regprocedure FROM pg_proc p
        WHERE p.pronamespace = 'public'::regnamespace
        AND NOT EXISTS (SELECT 1 FROM pg_depend d
                        WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
        AND NOT EXISTS (SELECT 1 FROM unnest(p.proconfig) c
                        WHERE c LIKE 'search_path=pg_catalog,%' AND c LIKE '%pg_temp')
    LOOP
        EXECUTE format('ALTER FUNCTION %s SET search_path = pg_catalog, public, pg_temp', fn);
    END LOOP;
END
$do$;

-- K5 (goodqa#15163): only the owner writes the migration ledger. A client
-- row could make Migrate skip a migration or refuse a valid one. ApplyRoles
-- revokes the same on every run; this covers a database before that rerun.
-- A table-level REVOKE also removes column grants of the same kind.
DO $do$
DECLARE
    g oid;
BEGIN
    FOR g IN
        SELECT DISTINCT a.grantee FROM pg_class c, aclexplode(c.relacl) a
        WHERE c.oid = 'public.fabric_schema'::regclass AND a.grantee <> c.relowner
        UNION
        SELECT DISTINCT a.grantee FROM pg_attribute t, aclexplode(t.attacl) a, pg_class c
        WHERE t.attrelid = 'public.fabric_schema'::regclass AND c.oid = t.attrelid AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('REVOKE INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER ON public.fabric_schema FROM %s',
                       CASE WHEN g = 0 THEN 'PUBLIC' ELSE quote_ident(pg_get_userbyid(g)) END);
    END LOOP;
END
$do$;
