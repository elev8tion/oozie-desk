-- Published apps are local web servers, not .app bundles.
ALTER TABLE store_apps ADD COLUMN public_url TEXT NOT NULL DEFAULT '';
ALTER TABLE store_apps ADD COLUMN runtime_pid INTEGER NOT NULL DEFAULT 0;
