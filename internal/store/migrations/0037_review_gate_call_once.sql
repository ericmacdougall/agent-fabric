-- 0037: one dispatched-or-settled review request per tool call.
-- A request hash includes the trusted-state fingerprint, so the same
-- tool_use_id can be proposed again under a new hash after any fingerprinted
-- column changes. The application looks the call up by its handle first
-- (goodqa#15235 H1); this index is the database backstop: a second request for
-- the same mission, session, tool, arguments and agent call handle cannot reach
-- DISPATCHED, UNKNOWN, CONFIRMED or FAILED_DEFINITE while another already has.
-- Requests with no agent call handle are not constrained (NULLs are distinct).
-- Roll forward only. It fails, naming the duplicate, if a database already
-- holds two such rows; classify those by hand first.
CREATE UNIQUE INDEX review_gate_call_once ON public.review_gate_requests (
    mission_id, session_id, tool,
    (pg_catalog.md5(((canonical_proposal::jsonb) -> 'args')::text)),
    ((pg_catalog.jsonb_path_query_first(canonical_proposal::jsonb,
        '$.provenance[*] ? (@.origin == "agent").id')) #>> '{}')
) WHERE state IN ('DISPATCHED', 'UNKNOWN', 'CONFIRMED', 'FAILED_DEFINITE');
