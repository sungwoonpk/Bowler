-- Retention deletes the oldest messages first, across every app. This index lets it find them without reading the rest.
CREATE INDEX messages_by_created ON messages (created_at, id);
