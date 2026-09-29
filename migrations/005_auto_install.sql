-- One-click publish ends with a usable app: publishes install to
-- ~/Applications by default (per-draft opt-out on the publish form).
ALTER TABLE publish_drafts ADD COLUMN auto_install INTEGER NOT NULL DEFAULT 1;
