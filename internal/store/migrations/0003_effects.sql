-- 0003: the effect broker (DESIGN.md 3.3, AC-5, AC-17).
-- An effect is INTENT -> (APPROVAL_REQUIRED -> APPROVED) -> DISPATCHED ->
-- CONFIRMED | FAILED_DEFINITE | UNKNOWN -> RECONCILING -> CONFIRMED |
-- NOT_APPLIED | QUARANTINED. The idempotency key is the remote key too, so a
-- transport retry reuses it; the request hash is bound at submit and by any
-- approval; a live subject admits one effect at a time.
CREATE TABLE effects (
    id            text PRIMARY KEY,
    mission_id    text NOT NULL REFERENCES missions (id),
    attempt_id    text NOT NULL,
    fence         bigint NOT NULL,
    kind          text NOT NULL,
    subject       text NOT NULL,
    idem_key      text NOT NULL UNIQUE,
    request       jsonb NOT NULL,
    request_sha   text NOT NULL,
    state         text NOT NULL,
    approval_sha  text,
    approved_by   text,
    dispatches    int NOT NULL DEFAULT 0,
    receipt       jsonb,
    last_error    text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX effects_live_subject_idx ON effects (subject)
    WHERE state IN ('DISPATCHED', 'UNKNOWN', 'RECONCILING');
CREATE INDEX effects_state_idx ON effects (state);
