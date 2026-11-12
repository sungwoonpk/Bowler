-- An endpoint may be limited to a number of attempts per second, so a receiver that can take only so much is not
-- flooded, whether by a burst of events or by a recovery that replays a long outage.
--
-- rate_per_second is 0 for no limit. rate_tat is the point in time (milliseconds) up to which the endpoint's allowance
-- has already been handed out. A claim may take work while that point is less than one second ahead of now, and moves
-- it forward by one interval per delivery taken. It lives in the database so a restart does not reset the allowance.
ALTER TABLE endpoints ADD COLUMN rate_per_second INTEGER NOT NULL DEFAULT 0 CHECK (rate_per_second BETWEEN 0 AND 1000);
ALTER TABLE endpoints ADD COLUMN rate_tat INTEGER NOT NULL DEFAULT 0;
