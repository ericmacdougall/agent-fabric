-- 0007: answering and withdrawing asks (DESIGN.md 3.11, AC-10).
-- An answer carries its own provenance (R12); a withdrawal carries a reason.
-- Roll-forward only; both columns are nullable so rows from 0002 stay valid.
ALTER TABLE asks
    ADD COLUMN answer_source    text,
    ADD COLUMN withdrawn_reason text;
-- An answered ask names who answered and with what source; nothing else may
-- be in the answered state.
ALTER TABLE asks ADD CONSTRAINT asks_answered_complete CHECK (
    state <> 'answered' OR (answer IS NOT NULL AND answered_by IS NOT NULL
                            AND answered_at IS NOT NULL AND answer_source IS NOT NULL AND answer_source <> ''));
