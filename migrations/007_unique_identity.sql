-- Disambiguate duplicate bundle_slugs (keep lowest id) then enforce uniqueness on non-empty slugs.
UPDATE store_apps AS s1
SET bundle_slug = s1.bundle_slug || '-' || s1.id
WHERE s1.bundle_slug != ''
  AND s1.id > (SELECT MIN(s2.id) FROM store_apps s2 WHERE s2.bundle_slug = s1.bundle_slug);

CREATE UNIQUE INDEX IF NOT EXISTS store_apps_bundle_slug_unique ON store_apps(bundle_slug) WHERE bundle_slug != '';
