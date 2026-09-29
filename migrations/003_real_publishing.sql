-- Publishing becomes real: store apps point at a built .app bundle on disk.
ALTER TABLE store_apps ADD COLUMN artifact_path TEXT NOT NULL DEFAULT '';

-- Remove fabricated demo data. Every store app, install record, and
-- publishing job created before this migration was simulated (publish
-- inserted a fake 'succeeded' row and never produced an app), so none of
-- it describes anything real on disk.
DELETE FROM installed_apps;
DELETE FROM store_apps;
DELETE FROM publishing_jobs;

-- Attachments never had an upload path; the UI affordance is gone.
DROP TABLE IF EXISTS attachments;
