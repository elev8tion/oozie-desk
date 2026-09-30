-- Pending store-link recipes waiting for Accept / Reject / Edit.
-- A draft holds the proposed plan and recipe JSON only — never runtime data.

CREATE TABLE IF NOT EXISTS recipe_drafts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  source_url TEXT NOT NULL,
  source_kind TEXT NOT NULL,
  name TEXT NOT NULL,
  headline TEXT NOT NULL DEFAULT '',
  store_description TEXT NOT NULL DEFAULT '',
  plan TEXT NOT NULL,
  recipe_json TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  project_id INTEGER NULL REFERENCES projects(id) ON DELETE SET NULL,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_recipe_drafts_status ON recipe_drafts(status);
