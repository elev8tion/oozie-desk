# oozie — Status

> This file describes the original Mac factory. This directory is the web fork: Publish runs `go build`, Start serves the binary on localhost, and nothing is installed to `/Applications`. See README.md.


*Updated 2026-07-14 after the "make it legit" overhaul. The two earlier assessments in this file and USER-EXPERIENCE-REPORT.md are historical; everything flagged there as fake, dead, or misleading has been fixed or removed.*

## Done — the product works end-to-end (verified live)

- **Agent**: real local pi per project, persistent sessions, model picker, cancel, question/permission panels, fail-closed approval gating for untrusted projects.
- **Publishing is real**: Publish runs `swift build -c release`, wraps the executable into `dist/<Name>.app`, and the job lifecycle (`queued → running → succeeded/failed`) is genuine, with build errors surfaced in the jobs list (which auto-refreshes while a build runs).
- **Store is real**: published apps appear in the store; Install copies the bundle to `~/Applications` (verified: the installed app launches and runs); Open launches it; Reinstall re-copies.
- **oozie is a Mac app**: `make app` produces `dist/oozie.app` (self-contained binary, embedded templates/CSS/JS/migrations, vendored HTMX, no network dependency); `make install-app` puts it in `~/Applications`. Data lives in `~/Library/Application Support/oozie/`.
- **Safety/hygiene**: binds to 127.0.0.1 only, graceful shutdown reaps pi subprocesses, panic-recovery + request-logging middleware, styled error pages, humanized timestamps, settings (theme + accent style) actually apply, no dead code or fake seed data, tests across build/service/HTTP layers.

## Polish round (2026-07-14, verified live)

- [x] **Live streaming**: pi `message_update` deltas persist as a throttled in-place "loading" assistant message; the timeline polls at 1s, so text appears as it's generated instead of all at once. Verified with a real agent run.
- [x] **Session stats**: after each run oozie queries pi's `get_session_stats` and shows tokens in/out, cost, and context % in the inspector (updates live during polling). Verified live: real token/cost figures rendered.
- [x] **App icons**: drop `icon.png` (or `icon.icns`) at the project root and publish — oozie converts it via sips/iconutil into the bundle's AppIcon. The agent prompt mentions this.
- [x] **Ad-hoc code signing**: published bundles are `codesign -s -` signed (signature verified in tests). Real Developer-ID signing/notarization only matters if apps are shared beyond this machine — out of scope for a personal workspace.
- [x] **Idle reaping**: pi processes idle for 30+ minutes are stopped automatically; sessions persist on disk so the next prompt resumes with full context.

Nothing is known to be missing or fake. Future ideas, not commitments: SSE instead of polling, per-message model attribution, publish-time version bumping.

## Living-apps release (2026-07-14, smoke-tested live)

Plan: `Plans/living-apps.md`. All nine features shipped, one commit each, tests green throughout:

- [x] **Foundation**: WAL + busy_timeout + per-connection foreign_keys via DSN; `agent_messages(request_id)` index; startup sweeps fail orphaned publish jobs and stale wishes.
- [x] **Liveness beacon**: launcher shim in every published bundle pings `/api/beacon/<slug>` and execs the real binary; store rows show launch counts/recency and a dormant badge. (Verified in the swift-build test: shim written, binary beside it, bundle still signs and runs.)
- [x] **Fix-me wormhole**: `/improve/<slug>` files a BUILD request; on completion oozie republishes and reinstalls automatically. System prompt requires a Help-menu "Improve this app…" item in every GUI app. (Loop covered by tests.)
- [x] **Disposable apps**: publish-form lifetime picker → `expires_at`; hourly reaper removes expired apps. (Covered by test.)
- [x] **Remix**: store page forks source (minus `.build/`, `dist/`, `.git/`) into a new project with a mutation prompt. (Covered by test.)
- [x] **Recipes**: export prompts+design+icon as `.oozie-recipe.json`; import rebuilds locally. (Round-trip covered by test.)
- [x] **Wish inbox + fairy**: wishes page, build-now, and a nightly scheduler (settings: enabled + hour) that grants up to 3 wishes and auto-publishes. (Lifecycle covered by test.)
- [x] **Insights**: dashboard strip — dormant apps, most-used app, waiting wishes.
- [x] **Taste**: `TASTE.md` beside the DB, editable in Settings, refreshed into every workdir before agent runs, appended with remix/fix/surgery signals; prompt tells the agent it overrides DESIGN.md.
- [x] **Surgery**: capture via the project's visual-review script, click-to-target UI, agent request carries the click coordinates; rides the improve loop for auto-republish.

Caveats worth knowing: the fairy builds wishes in **trusted** projects (unattended runs can't answer permission prompts); wish→request linkage is in-memory, so a restart mid-build fails the wish honestly at the next startup; surgery's executable-name guess is the app name without spaces — capture reports the script's error output if that misses.
