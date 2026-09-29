# Living Apps — the "oozie becomes a gardener" release

Nine features that turn oozie from a build tool into a living ecosystem for
personal software, plus the foundation work they force. Built in dependency
order; one commit per feature; `go build ./... && go test ./...` green at
every step.

## Phase 0 — Foundation (enabler, not a feature)

New background writers (beacon endpoint, fairy scheduler, TTL reaper,
auto-republish) multiply concurrent SQLite writers, so the known DB gaps
must close first:

- Open the DB with WAL + busy_timeout + per-connection foreign_keys via DSN
  pragmas (`internal/db/sqlite.go`).
- `CREATE INDEX idx_agent_messages_request ON agent_messages(request_id)`.
- Startup sweep: any `publishing_jobs` left `queued`/`running` from a dead
  process are marked `failed` with an honest message.
- Migration `004_living_apps.sql` carries all new schema (below).

## Phase 1 — Liveness beacon ("the Store learns which apps are alive")

Published bundles get a launcher shim: `Contents/MacOS/<Name>` becomes a
2-line shell script that fires `curl http://127.0.0.1:8080/api/beacon/<slug>`
(1s timeout, background, failure-silent) and `exec`s the real binary
(`<Name>-bin`). No agent cooperation, no network beyond localhost, works
even when oozie is closed (ping is simply lost).

- `store_apps.bundle_slug` column (unique, derived from name at publish).
- `app_events (id, store_app_id, kind, created_at)` — kind: `launch`.
- `POST|GET /api/beacon/{slug}` records a launch event, 204.
- Store rows show liveness: "opened 12× · last 2h ago", a `dormant` badge
  after 14 days installed-but-unlaunched, and `never opened` when installed
  with zero events.

## Phase 2 — The fix-me wormhole ("apps that ask to be repaired")

- `GET /improve/{slug}` — a tiny focused page: "What should be better about
  <App>?" textarea. Reachable from inside every published app: the agent
  system prompt now requires a Help-menu item "Improve <App>…" that opens
  that URL (slug injected into the prompt after first publish; also stated
  generically for new apps).
- `POST /improve/{slug}` files the text as a BUILD agent request on the
  app's project, records a row in `improve_requests (id, request_id,
  store_app_id, auto_publish, created_at)`.
- When that agent request settles `completed`, oozie auto-republishes the
  project; when the publish job succeeds and the app is installed, it
  auto-reinstalls the new bundle. The loop closes without a human touching
  the factory.
- Store page gets the same "Request a fix" affordance.

## Phase 3 — Disposable apps (TTL)

- `publish_drafts.expires_days` (0 = permanent) → `store_apps.expires_at`.
- Hourly reaper: expired apps are uninstalled from ~/Applications and
  removed from the store (project untouched — republish resurrects).
- UI: "self-destructs in 3d" badge on store rows; expiry picker on the
  publish form (Permanent / 1 day / 1 week / 1 month).

## Phase 4 — Remix ("app genetics")

- `POST /store/apps/{id}/remix` with a mutation prompt. Creates a new
  project (`<Name> Remix`), copies the source workdir (skipping `.build/`,
  `dist/`, `.git/`), and fires a BUILD request: "This project is a remix of
  <Name>. Mutation: …".
- Remix button + prompt field on the store app page.

## Phase 5 — Recipes ("share apps as prompts, not binaries")

- Export: `GET /store/apps/{id}/recipe` downloads `<name>.oozie-recipe.json`
  — app metadata + every user prompt from the project's session history +
  the project's DESIGN.md (if customized) + icon.png (base64, if present).
- Import: `/recipes/import` page; paste or upload a recipe → new project,
  icon materialized, and one synthesized BUILD request that replays the
  intent ("Rebuild this app from its recipe: …").

## Phase 6 — Nightly build fairy (wish inbox + night shift)

- `wishes (id, text, status pending|building|built|failed, project_id,
  error, created_at, built_at)`; a Wishes page: add, delete, "build now".
- Settings gain `fairy_enabled` + `fairy_hour` (default 02:00). A scheduler
  goroutine wakes each minute; at the configured hour it takes up to 3
  pending wishes and, sequentially: creates a project, files a BUILD
  request from the wish text, and on completion auto-publishes (reusing the
  Phase 2 machinery). Morning Store shows the overnight prototypes.

## Phase 7 — Insights ("ambient synthesis", honest v1)

No screen watching. Derives suggestions from data oozie already owns:
- Dormant installed apps → "retire or ask the agent why you don't use it?"
- Heavily-used apps with pending improve requests → surface first.
- Wish inbox items older than N days → nudge.
Rendered as an Insights strip on the dashboard. (Real ambient observation
of Mac usage is deliberately out of scope — consent and scope creep.)

## Phase 8 — Design genome (taste)

- `~/Library/Application Support/oozie/TASTE.md` — the user's evolving
  design voice. Editable in Settings ("Your taste").
- Seeded into every project workdir alongside DESIGN.md; system prompt
  tells the agent TASTE.md overrides DESIGN.md's generic choices.
- Signals append automatically: remix mutations and improve-request texts
  are distilled into a "Signals" section (raw, dated) the user can promote
  into rules.

## Phase 9 — Surgery (point-and-fix)

- Store app page (with linked project): "Surgery" runs the project's
  `Tools/visual-review.sh` to capture a fresh screenshot (falls back to the
  existing `review.png`), shows it in the browser.
- Click a point on the screenshot + describe the change → files a BUILD
  request carrying the relative click coordinates and instructing the agent
  to read `review.png`, identify the element at that point, and make the
  change (then re-run visual review).

## Schema (004_living_apps.sql)

```sql
ALTER TABLE store_apps ADD COLUMN bundle_slug TEXT NOT NULL DEFAULT '';
ALTER TABLE store_apps ADD COLUMN expires_at TIMESTAMP;
ALTER TABLE publish_drafts ADD COLUMN expires_days INTEGER NOT NULL DEFAULT 0;
ALTER TABLE user_settings ADD COLUMN fairy_enabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE user_settings ADD COLUMN fairy_hour INTEGER NOT NULL DEFAULT 2;
CREATE TABLE app_events (…);
CREATE TABLE improve_requests (…);
CREATE TABLE wishes (…);
CREATE INDEX idx_agent_messages_request ON agent_messages(request_id);
```

## Sequencing rationale

Beacon first (slug + events feed Phases 2/3/7). Fix-me loop second (its
auto-publish machinery is reused by the fairy). TTL/remix/recipes are
independent middles. Fairy needs auto-publish. Insights needs events.
Taste and Surgery last — they touch prompts and seeds, which everything
else leaves alone.
