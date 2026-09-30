# Design standard for apps built in this project

Every app built here is a small local web app: one Go server, HTML pages,
and no build step in the browser. It should feel finished — calm, dense
enough to be useful, and obvious to operate. These rules are mandatory
unless TASTE.md or the user overrides them.

## Shape
- One `main.go` at the project root (or `cmd/app`) and `go.mod`.
- Listen on `$ADDR` (for example `127.0.0.1:8091`). If `ADDR` is empty, use
  `127.0.0.1:` plus `$PORT`. Never pick your own port.
- Serve real HTML. Prefer `html/template` and a single stylesheet. HTMX is
  welcome for in-page updates; do not add a JavaScript framework.
- Prefer the standard library. Add a dependency only when the app cannot
  work without it.
- A footer always includes a link labeled "Back to desk" pointing at
  `$OOZIE_DESK_URL` when that variable is set (the oozie home page). Use
  `target="_top"` so the link leaves an iframe shell if one is wrapping the
  tool. Every tool must let the user return to the desk without closing the tab.
- A second footer link labeled "Improve this app" (or "Fix") points at
  `$OOZIE_IMPROVE_URL` when that variable is set.

## Data
- Durable user records live only under `data/` in the project root (or
  `$OOZIE_DATA_DIR` when set). Create that directory on first write.
- SQLite files, uploads, and caches belong there — never in source, never
  embedded as sample rows that look like real usage.
- Sharing sends the recipe (prompts + design), not `data/`. Another desk
  rebuilds an empty tool. Do not hardcode the author's personal data.

## Layout
- A clear page title, one primary action, and content that does not sit
  flush against the window edge. Use an 8px spacing scale (8/16/24/32).
- Readable measure: body text stays under about 70 characters. Tables and
  tools may be wider.
- Empty, loading, and error states are designed. Never a blank page, a
  spinner with no label, or a raw stack trace.

## Color & type
- A small palette: one background, one surface, one text color, one muted
  text color, one accent. Support light and dark with `prefers-color-scheme`
  or a toggle. Do not hardcode a white page with default blue links and
  stop there.
- System font stack (`ui-sans-serif, system-ui, sans-serif`). Hierarchy
  comes from size and weight, not a pile of typefaces.
- Icons are inline SVG or text, not emoji standing in for buttons.

## Interaction
- Forms post to the server and re-render HTML. Confirm destructive actions.
- Keyboard: the primary field is focused, Enter submits, links and buttons
  are real controls with visible focus.
- The app must build with `go build -o /tmp/app .` from the project root
  and serve `GET /` with a 200 once started.
