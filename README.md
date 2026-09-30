# oozie (web fork)

A local desk for small personal tools. You describe a tool, your local **pi** agent builds it, and oozie opens it on localhost. A company is a circle of desks that pair by invitation. A share sends the recipe, never the data.

This is a copy of the Mac factory. The original at `/Users/kc/oozie` is unchanged. This fork does not install anything into `/Applications`.

## The loop

1. **Describe the tool** — one sentence on the desk. The project, directory, and trust flag are defaults.
2. **Wait** — the agent builds one Go page that listens on `$ADDR`. No icon and no screenshot pass.
3. **Open** — when the build finishes, oozie starts it and opens it at `/run/<id>` inside the desk chrome. A failed start is a failed job.
4. **Back** — the run bar always has "Back to desk". New tools also footer-link `OOZIE_DESK_URL` so a bare localhost tab can return home.
5. **Fix** — a second footer link returns to oozie with another sentence. Remix copies a working tool into a new one.

People is invitation-only. Connect stays off until you turn it on, and then it listens on a private address, not on the factory port. A share link does nothing until its owner turns it on. The other desk imports the recipe and builds the tool there.

Projects, wishes, recipes, and the job list stay under More. They are not the way in.

## Still in the loop

- **Back to desk** — published apps receive `OOZIE_DESK_URL` and must link home to the factory desk from every page
- **Fix** — published apps can link to `/improve/<slug>`; a request there becomes an agent build, then oozie republishes and restarts the app
- **Launch pings** — an app may GET `/api/beacon/<slug>` so the store shows usage
- **Remix** — fork a store app's source into a new project with a mutation prompt
- **Recipes** — export and import an app as prompts plus design notes
- **Disposable apps** — publish with a TTL; the hourly reaper stops and delists them
- **Wishes** — ideas the nightly fairy can build and publish
- **Taste** — `TASTE.md` in Settings, copied into every project

Pixel surgery and the native Mac shell are not part of this fork.

## Run

```bash
make run    # http://127.0.0.1:8090
make test
make build  # dist/oozie-web
```

Data lives in `~/Library/Application Support/oozie-web/app.db`.

## Requirements

- Go 1.24+
- [pi](https://github.com/) installed (`~/.pi/agent/settings.json`)

## Environment variables

- `ADDR` (default `127.0.0.1:8090`)
- `DATABASE_PATH` (default `~/Library/Application Support/oozie-web/app.db`)
- `PI_BIN` (default `pi`)
- `OOZIE_OPEN_BROWSER=1` — open the UI on start
