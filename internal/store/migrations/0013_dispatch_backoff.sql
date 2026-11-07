-- 0013: pre-launch backoff (red-team F1 on f54d526).
-- An attempt abandoned before its runner started leaves the mission eligible
-- again at once; without a wait the scheduler re-dispatched it every pass.
-- not_before is the projection of the latest DISPATCH_DEFERRED event: its
-- transaction time (events.at) plus the backoff the reducer recorded, so it is
-- recomputable from the log. The dispatch query skips a mission until then.
ALTER TABLE missions ADD COLUMN not_before timestamptz;
