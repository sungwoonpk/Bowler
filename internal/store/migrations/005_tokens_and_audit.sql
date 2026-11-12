-- A token lets one app's own side act on that app and nothing else. Only a hash of the token is kept, so a copy of the
-- database does not hand out working tokens. The token is long and random, so a plain SHA-256 is enough, there is nothing
-- to guess and so nothing to slow down.
CREATE TABLE app_tokens (
    id           TEXT PRIMARY KEY,
    app_id       TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    token_prefix TEXT NOT NULL,                  -- the first characters, so a person can tell tokens apart
    created_at   INTEGER NOT NULL,
    revoked_at   INTEGER
);
CREATE INDEX app_tokens_by_app ON app_tokens (app_id, created_at DESC, id DESC);

-- Who did what. Entries are never updated. app_id has no foreign key on purpose: the record of an action outlives
-- anything that happens to the thing it was about.
CREATE TABLE audit_log (
    id            TEXT PRIMARY KEY,
    at            INTEGER NOT NULL,
    actor_kind    TEXT NOT NULL CHECK (actor_kind IN ('admin', 'app_token')),
    actor_id      TEXT NOT NULL,                 -- 'admin', or the id of the token
    app_id        TEXT,
    action        TEXT NOT NULL,
    resource_kind TEXT,
    resource_id   TEXT,
    detail        TEXT,                          -- a small JSON object, never a secret or a payload
    request_id    TEXT
);
CREATE INDEX audit_by_app ON audit_log (app_id, at DESC, id DESC);
CREATE INDEX audit_by_time ON audit_log (at DESC, id DESC);
