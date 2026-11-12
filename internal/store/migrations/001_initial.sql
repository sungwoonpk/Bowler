-- Times are whole milliseconds since the Unix epoch, so they compare and sort as plain integers.

CREATE TABLE apps (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    uid        TEXT UNIQUE,
    created_at INTEGER NOT NULL
);

CREATE TABLE endpoints (
    id                     TEXT PRIMARY KEY,
    app_id                 TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    url                    TEXT NOT NULL,
    description            TEXT NOT NULL DEFAULT '',
    secret                 TEXT NOT NULL,
    prev_secret            TEXT,
    prev_secret_expires_at INTEGER,
    event_types            TEXT,                       -- a JSON list, or NULL for every event
    concurrency            INTEGER NOT NULL DEFAULT 1,
    disabled_at            INTEGER,
    disabled_reason        TEXT,
    created_at             INTEGER NOT NULL,
    deleted_at             INTEGER                     -- deleted endpoints are kept so their history stays readable
);
CREATE INDEX endpoints_by_app ON endpoints (app_id, created_at DESC, id DESC) WHERE deleted_at IS NULL;

CREATE TABLE messages (
    id              TEXT PRIMARY KEY,
    app_id          TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    event_type      TEXT NOT NULL,
    payload         TEXT NOT NULL,
    idempotency_key TEXT,
    created_at      INTEGER NOT NULL
);
CREATE UNIQUE INDEX messages_idempotency ON messages (app_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX messages_by_app ON messages (app_id, created_at DESC, id DESC);

CREATE TABLE deliveries (
    id              TEXT PRIMARY KEY,
    message_id      TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    endpoint_id     TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
    app_id          TEXT NOT NULL,
    state           TEXT NOT NULL CHECK (state IN ('pending', 'inflight', 'succeeded', 'failed', 'cancelled')),
    attempt_count   INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    lease_until     INTEGER,
    lease_owner     TEXT,
    manual          INTEGER NOT NULL DEFAULT 0,        -- 1 when the next attempt was asked for by a person
    last_status     INTEGER,
    last_failure    TEXT,
    created_at      INTEGER NOT NULL,
    finished_at     INTEGER,
    UNIQUE (message_id, endpoint_id)
);
-- The dispatcher looks only at work that is not finished, so finished rows do not slow it down.
CREATE INDEX deliveries_due ON deliveries (next_attempt_at) WHERE state IN ('pending', 'inflight');
CREATE INDEX deliveries_by_endpoint ON deliveries (endpoint_id, created_at DESC, id DESC);
CREATE INDEX deliveries_by_message ON deliveries (message_id);

CREATE TABLE attempts (
    id               TEXT PRIMARY KEY,
    delivery_id      TEXT NOT NULL REFERENCES deliveries(id) ON DELETE CASCADE,
    started_at       INTEGER NOT NULL,
    duration_ms      INTEGER NOT NULL,
    status_code      INTEGER,
    failure          TEXT,
    response_snippet TEXT,
    trigger          TEXT NOT NULL CHECK (trigger IN ('scheduled', 'manual'))
);
CREATE INDEX attempts_by_delivery ON attempts (delivery_id, started_at);
