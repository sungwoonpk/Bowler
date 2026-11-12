-- A token may be read only, may expire, and records roughly when it was last used.
--
-- scope 'full' is what every token had before. 'read' may look and may not change anything, and may not read a signing
-- secret either, since whoever has one can forge webhooks to that endpoint.
--
-- last_used_at is approximate on purpose: the service writes it at most once in ten minutes per token, so that using a
-- token does not turn every request into a database write.
ALTER TABLE app_tokens ADD COLUMN scope TEXT NOT NULL DEFAULT 'full' CHECK (scope IN ('full', 'read'));
ALTER TABLE app_tokens ADD COLUMN expires_at INTEGER;
ALTER TABLE app_tokens ADD COLUMN last_used_at INTEGER;
