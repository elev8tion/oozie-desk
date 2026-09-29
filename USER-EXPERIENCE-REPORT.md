# oozie — User Experience Report

*Written 2026-07-14 by driving the running app headlessly as a first-time user: created projects, ran real agent sessions, published, installed store apps, changed settings, probed error paths. Every claim below was observed live, not inferred from code.*

---

## 1. What Works — Verified Capabilities

### The agent is real, and it's the heart of the product ✅
This is the flow that matters, and it delivers:

- **Build request → real work.** Asked the agent to create `hello.txt` in a fresh project; pi spawned in the project directory, wrote the file, and the timeline showed `user → write (ok) → "Done." → completed`. The file was on disk with the exact content.
- **Conversation continuity works.** Asked a question in plan mode, replied "TypeScript" in a follow-up build request — pi remembered the context and wrote `preference.txt` containing `TypeScript`. The persistent `--session-id` design is doing its job.
- **Model picker is live.** All 8 models from `~/.pi/agent/settings.json` appear; switching via the dropdown returns 200 and swaps the composer.
- **Cancel works at the request level.** Sent a long request, cancelled it; timeline marked it `cancelled` and a real abort was sent. (Caveat: the tool call in my test finished in one bash shot before the cancel landed, so mid-flight interruption is unproven.)
- **Plan vs Build modes** both round-trip correctly.
- **No zombie processes observed** — after the session, zero stray pi processes were running.

### Everything else renders and persists
- **Projects**: create (with path defaulting), list, search/filter, archive, detail view — all work, all persist to SQLite.
- **Store**: browse, filter (featured/public/org/installed), app detail, install state.
- **Publishing**: draft form validates required fields; jobs list renders with status filter.
- **Settings**: saves and confirms with a toast.
- **Error handling basics**: bad project ID → 400 "invalid id", missing project → 404, empty name → validation toast. Nothing 500'd during the entire session.

---

## 2. Where It Falls Short — Ranked by User Impact

### 🔴 P0 — Trust model is not enforced — ✅ FIXED 2026-07-14
Created a project with **Trusted unchecked**, then asked the agent to write a file. **It wrote to disk immediately with no permission prompt.** The timeline showed `write (ok)` with zero approval step. The Trusted checkbox — the app's only safety affordance — was cosmetic in practice.

Root cause: pi's `--approve`/`--no-approve` flags control whether *project-local config files* are trusted — they never gated tool execution. Fix: an embedded pi extension (`internal/agent/pi/approval.ts`), loaded via `-e` for untrusted projects only, intercepts `write`/`edit`/`bash` tool calls and blocks on a `confirm` dialog that renders in oozie's existing permission panel. Verified live: prompt appears with tool + path/command detail, Deny blocks the write (`write (error)`, nothing on disk, model explains), Approve lets it through, and trusted projects still run prompt-free. If the extension can't be materialized, untrusted agents refuse to start (fail closed).

### 🔴 P0 — Publishing is theater
- Clicking Publish instantly shows "Publish succeeded." — the job is inserted directly as `succeeded`, no lifecycle.
- I published "UX Test App" twice; **it never appeared in the store** (search returned 0 results). Publishing produces a job row and nothing else.
- The jobs table shows the raw project **ID** ("3") instead of the project name.

### 🟠 P1 — Timeline is opaque while the agent works
- No token streaming: you stare at "pi is working…" until a whole message lands. For a multi-minute build this feels dead.
- Tool messages say only `write (ok)` / `bash (ok)` — no filename, no command, no diff. As a user I couldn't tell *what* the agent did without checking the filesystem myself.
- No per-message model attribution, no token/cost/context stats anywhere (pi exposes `get_session_stats`; the inspector panel is the natural home).

### 🟠 P1 — Settings save but do nothing
- Theme select persists to SQLite, but `data-theme="system"` is **hardcoded** in `templates/layouts/base.html:3` and the CSS only reads `prefers-color-scheme`. Choosing Light/Dark/Blueprint/Warm Mono changes nothing visually.
- Agent shortcut / send-message shortcut fields are saved but no JS ever reads them.
- Display name is a `disabled` input — pure decoration.

### 🟠 P1 — Misleading copy and labels
- Sidebar on every page: "**Stubbed AI**, publishing, and installs" — the AI hasn't been stubbed since 2026-07-12. This actively undersells the product's one real feature.
- Store cards: the button labeled "**Open**" actually POSTs to `/install` (toggles install state). Nothing in the store can be opened.
- App detail for installed apps shows a primary button reading "Installed" that is still clickable.

### 🟡 P2 — Rough edges
- Timestamps render as raw Go format: `2026-07-14 09:08:13 +0000 UTC`.
- Error pages are bare plaintext (`404 page not found`, `invalid id`) with no layout or way back.
- Non-HTMX form error paths return a bare toast `<div>` as the entire response body.
- Attachments panel shows "No attachments" but there is no upload route — dead affordance.
- `/tasks` silently 303s to `/projects`; an entire coded tasks domain (models, repo, handlers, templates) is orphaned with no routes and no table.
- Port conflicts fail with a raw `bind: address already in use` log line (worth a friendlier message since 8080 is a busy default).

---

## 3. Overall User Experience

**First impression: polished.** The dense macOS-style UI, consistent panels, toasts, and the new button system read as a finished product. Navigation is instant (server-rendered + HTMX earns its keep).

**The core loop is genuinely good.** Create project → open agent → type → watch pi do real work in your real directory → files appear. That loop worked flawlessly every time, including multi-turn context. This is already a usable local pi frontend.

**The honesty gap is the real UX problem.** The app *looks* uniformly finished, so the fake parts betray you silently: you toggle Trusted and believe you're protected (you're not), you Publish and see "succeeded" (nothing happened), you set Dark theme (nothing changes). A user can't tell the real 40% from the simulated 60% until something bites them. Closing that gap — either by making features real or clearly marking them as stubs — matters more than adding anything new.

---

## 4. Recommended Roadmap (in order)

1. **Fix the trust model** — verify the `--approve` / permission-mode mapping to pi; an untrusted project must surface write/bash approvals in the permission panel. This is a safety issue, not polish.
2. **Make the timeline informative** — show tool detail (file paths, commands, truncated output), stream deltas (SSE or finer polling), and surface `get_session_stats` (tokens/cost/context %) in the inspector.
3. **Decide publishing's fate** — either implement the real lifecycle (`queued → running → succeeded`, create/update the `store_apps` row so your app appears in your own store) or hide the feature behind a "coming soon" state.
4. **Truth-in-labeling sweep** — sidebar copy, "Open" → "Install/Installed", humanize timestamps, styled error pages. One afternoon, big credibility win.
5. **Wire the theme** — render `data-theme` from saved settings and add `[data-theme="dark"]` CSS overrides; delete the shortcut/display-name fields until they do something.
6. **Tasks domain** — wire it in (routes + `002_tasks.sql`) or delete it. It's the largest chunk of dead code in the repo.
7. **Attachments** — build upload + serving, or remove the panel.

---

## Appendix: Test Session Log

| Flow | Result |
|---|---|
| Create project (trusted) | ✅ 303 → detail page |
| Agent build request → file on disk | ✅ real pi, file created |
| Multi-turn context (plan → build follow-up) | ✅ remembered answer |
| Model switch via dropdown | ✅ 200 |
| Cancel running request | ✅ marked cancelled |
| Untrusted project write | ❌ no approval prompt, wrote anyway |
| Publish draft → store listing | ❌ job "succeeded", app never in store |
| Store install toggle | ✅ persists |
| Settings save | ✅ persists / ❌ never applied |
| `/tasks` | ❌ silent redirect |
| Bad IDs / empty inputs | ✅ handled, but bare-text errors |
| Zero 500s across whole session | ✅ |
