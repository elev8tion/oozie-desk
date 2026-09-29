# Redesign the button system — keep the light blue, make it look intentional

## Context

The primary buttons (light blue) have gone through two rounds of fixes (markup unification, CSS consolidation) but the user's verdict is that the **whole button style is still wrong**. Per their answers:

- **Problem:** the entire button look needs a redesign, not a tweak.
- **Direction:** *keep the light-blue accent identity, but polish it* — proper depth, borders, and contrast so it reads as a deliberate, finished design instead of a flat pale pill.

All button styling lives in one consolidated section at the bottom of `static/css/app.css` (added in commit `f6fb326`). Every template already uses exactly three variants (`button`, `button primary`, `button danger`) plus a `.small` size, so **this is a CSS-only redesign — no template changes**.

Current weaknesses of the existing style (from reading `static/css/app.css`):
- Primary is a flat single-color fill (`background:var(--accent)`) — no depth, reads washed-out, especially in dark mode where `--accent` is pale `#7aa2ff`.
- Secondary is nearly invisible: `--panel2` background on `--panel` surfaces with a faint `--line` border.
- Danger is a bare outline that looks unfinished next to the filled primary.
- Focus state is the generic global `:focus-visible` outline, which visually breaks the button shape.
- The segmented Build/Plan pills and settings tabs don't share the button design language.

## Design spec (the new look)

One visual language for all interactive controls, built on the light-blue accent:

**Primary (`.button.primary`)**
- Gradient fill for depth: `linear-gradient(180deg, color-mix(in srgb, var(--accent) 82%, #fff), var(--accent))` — lighter at top, accent at bottom.
- Crisp definition: 1px border of `color-mix(in srgb, var(--accent) 65%, #000)`.
- Inner top highlight: `inset 0 1px 0 rgba(255,255,255,.28)` + soft outer glow `0 2px 8px color-mix(in srgb, var(--accent) 40%, transparent)`.
- Text: `--accent-contrast` (white in light mode, deep navy `#0e1524` in dark mode) with `font-weight:650`.
- Hover: gradient brightens (`color-mix(accent 70%, #fff)` top stop) and glow expands. Active: gradient flattens + `inset 0 2px 4px` pressed look, translateY(1px).

**Secondary (`.button`)**
- Elevated neutral: `linear-gradient(180deg, var(--panel), var(--panel2))`, visible border `color-mix(in srgb, var(--text) 14%, var(--line))`, subtle `0 1px 2px` drop shadow, same inset top highlight at low opacity.
- Hover: accent-tinted — border shifts toward accent, faint accent wash background. Ties secondaries into the light-blue language without filling them.

**Danger (`.button.danger`)**
- Same construction as secondary but tinted red: red-mixed border and text, red wash on hover that fills to a soft red gradient. No longer a bare outline.

**Shared**
- Radius bumps 8px → 9px; `.small` stays 24px but gets radius 7px.
- Custom focus ring replacing the global outline on buttons: `box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 35%, transparent)` so focus follows the button's rounded shape.
- Disabled: desaturated, no gradient, no shadow.

**Matching controls (same language, so nothing looks like a stray variation)**
- Segmented Build/Plan pills (`.segmented label`): checked state gets the primary gradient treatment; unchecked matches secondary.
- Settings tabs `.tabs a.active`: switch from flat accent fill to the same primary gradient/border/highlight recipe.

## Files to modify

- `static/css/app.css` — only file. Rewrite the "Buttons — single source of truth" section (and the `.tabs a.active` rule) per the spec above. Add a `.segmented` checked-state rule. Nothing else in the stylesheet changes; the minified base line stays untouched.

Existing pieces to reuse (already in place, do not re-add):
- `--accent-contrast` custom property (light/dark values) — `static/css/app.css`
- Cache-busting: `cssVersion` template func keyed to app.css mtime (`internal/web/render/templates.go`) + `Cache-Control: no-cache` on `/static/` (`internal/app/routes.go`) — any CSS edit is picked up on normal page reload, no hard refresh needed anymore.
- All templates already use the 3-variant markup — no template edits.

## Verification (codebase/server only — no Chrome)

1. `go build ./...` passes.
2. Start the server (`ADDR=:8090 ./app`), then:
   - `curl -s localhost:8090/static/css/app.css | grep -c "linear-gradient"` confirms the new rules are served.
   - `curl -s localhost:8090/projects` renders 200 with no `template render error`, and the `app.css?v=` stamp changed (proves the browser will fetch fresh CSS).
3. Sanity: grep that exactly one `.button,button{` definition exists (no duplicate rules reintroduced).
4. User reloads http://localhost:8090 (normal reload — no-cache headers now force fresh CSS) and judges the look. Iterate on the gradient/border values from their feedback.
