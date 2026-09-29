-- Living apps: liveness beacons, the fix-me loop, disposable apps, and
-- the wish inbox for the nightly build fairy.

-- Beacon identity + TTL for published apps.
ALTER TABLE store_apps ADD COLUMN bundle_slug TEXT NOT NULL DEFAULT '';
ALTER TABLE store_apps ADD COLUMN expires_at TIMESTAMP;
ALTER TABLE publish_drafts ADD COLUMN expires_days INTEGER NOT NULL DEFAULT 0;

-- Launch events reported by installed apps' launcher shims.
CREATE TABLE app_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    store_app_id INTEGER NOT NULL REFERENCES store_apps(id) ON DELETE CASCADE,
    kind TEXT NOT NULL DEFAULT 'launch',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_app_events_app ON app_events(store_app_id, created_at);

-- Improvement requests filed from inside published apps (or the store).
-- When the linked agent request completes, oozie auto-republishes and,
-- if the app is installed, auto-reinstalls.
CREATE TABLE improve_requests (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id INTEGER NOT NULL REFERENCES agent_requests(id) ON DELETE CASCADE,
    store_app_id INTEGER NOT NULL REFERENCES store_apps(id) ON DELETE CASCADE,
    status TEXT NOT NULL DEFAULT 'building', -- building|publishing|done|failed
    note TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_improve_requests_request ON improve_requests(request_id);

-- The wish inbox: ideas the nightly build fairy turns into apps.
CREATE TABLE wishes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    text TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending', -- pending|building|built|failed
    project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
    error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    built_at TIMESTAMP
);

-- Night-shift settings.
ALTER TABLE user_settings ADD COLUMN fairy_enabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE user_settings ADD COLUMN fairy_hour INTEGER NOT NULL DEFAULT 2;

-- The streaming hot path filters agent_messages by request_id constantly;
-- it was the only hot column with no index.
CREATE INDEX idx_agent_messages_request ON agent_messages(request_id, created_at);

-- Backfill slugs for already-published apps (lowercased, spaces to dashes).
UPDATE store_apps SET bundle_slug = lower(replace(name, ' ', '-')) WHERE bundle_slug = '';
