-- Desk-wide preferred coding model (provider/id). Empty means catalog default.
ALTER TABLE user_settings ADD COLUMN coding_model TEXT NOT NULL DEFAULT '';
