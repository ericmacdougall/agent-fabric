-- 0005: outbox rows carry the inserting transaction id, so a consumer reads
-- only rows whose transaction is older than every in-progress one and can
-- never skip a row that commits late with a lower id (review H5 / RV-3).
ALTER TABLE outbox ADD COLUMN xid xid8 NOT NULL DEFAULT pg_current_xact_id();
CREATE INDEX outbox_xid_idx ON outbox (xid, id);
