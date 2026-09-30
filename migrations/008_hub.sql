-- Local desk identity, invite-only peers, and share links.
-- A share grant is the token. Deleting the row turns the link off.

CREATE TABLE IF NOT EXISTS node_keys (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  node_id TEXT NOT NULL,
  private_key TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS peers (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  node_id TEXT NOT NULL UNIQUE,
  display_name TEXT NOT NULL,
  circle_name TEXT NOT NULL DEFAULT '',
  addr TEXT NOT NULL DEFAULT '',
  revoked INTEGER NOT NULL DEFAULT 0,
  last_seen TEXT NULL,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS invitations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  code_hash TEXT NOT NULL UNIQUE,
  expires_at INTEGER NOT NULL,
  used_at INTEGER NULL,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS share_grants (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  store_app_id INTEGER NOT NULL REFERENCES store_apps(id) ON DELETE CASCADE,
  peer_id INTEGER NOT NULL REFERENCES peers(id) ON DELETE CASCADE,
  token TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS share_inbox (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  author_node_id TEXT NOT NULL,
  author_name TEXT NOT NULL DEFAULT '',
  token TEXT NOT NULL,
  app_name TEXT NOT NULL DEFAULT '',
  headline TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(author_node_id, token)
);

ALTER TABLE store_apps ADD COLUMN kind TEXT NOT NULL DEFAULT 'page';
ALTER TABLE organizations ADD COLUMN industry_pack TEXT NOT NULL DEFAULT '';
ALTER TABLE user_settings ADD COLUMN connect_enabled INTEGER NOT NULL DEFAULT 0;
