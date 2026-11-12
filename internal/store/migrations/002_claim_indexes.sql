-- Claiming work looks at one endpoint at a time and takes only as many deliveries as the endpoint has free slots.
-- These two indexes let it find the oldest due deliveries of one endpoint without reading the rest, so the cost of a
-- claim depends on how many endpoints have work and not on how many deliveries are waiting.
--
-- They are partial, so they hold only unfinished work and stay small however much history there is.
CREATE INDEX deliveries_pending_by_endpoint ON deliveries (endpoint_id, next_attempt_at, id) WHERE state = 'pending';
CREATE INDEX deliveries_inflight_by_endpoint ON deliveries (endpoint_id, lease_until, id) WHERE state = 'inflight';

-- Replaced by the two above. Every index costs a write on every state change, so one that nothing uses is dropped.
DROP INDEX deliveries_due;
