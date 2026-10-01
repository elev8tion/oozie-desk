package projects

import (
	"fmt"
	"strings"
)

// qualityBar is the product check every build prompt shares. Compiling is not done.
const qualityBar = `Quality bar — a compiling page is not done:
- The user's job is the product. Do not ship a page that only echoes the prompt, says hello, or only has footer links.
- Before writing files, state a 5-line spec: (1) job (2) screens (3) fields (4) what is saved (5) done-when. Then implement that spec.
- The finished HTML must show the user's nouns, one primary action, and a designed empty state. Never a blank page or a raw error.
- One tool, not a suite. Add a second route only when the job needs a detail or edit screen. Server-rendered HTML. No JavaScript framework.
- Visual: one background, one surface, one text color, one muted color, one accent. System font. 8px spacing. Support light and dark with prefers-color-scheme. No default unstyled blue-link page.
- If the user can enter anything, a form must save it under data/ (or $OOZIE_DATA_DIR) and GET / must show the saved rows back.`

const contractReminder = `Desk contract — do not skip:
- go.mod and main.go at the project root. Listen on $ADDR, or 127.0.0.1:$PORT if ADDR is empty. Never hardcode a port.
- GET / returns HTML 200.
- Footer: "Back to desk" with target="_top" to the desk URL from the system prompt (or $OOZIE_DESK_URL), and "Fix" to the improve URL when that URL is set.
- Verify with: go build -o /tmp/app .
- Do not ask questions. Pick sensible defaults and build them.`

const uiSkeleton = `Reference shape (adapt it; do not ship it unchanged):
<body style="font-family:ui-sans-serif,system-ui,sans-serif;margin:0;background:#f6f4ef;color:#1c1917">
  <main style="max-width:40rem;margin:2rem auto;padding:0 1.5rem">
    <h1>Tool name</h1>
    <form method="post" action="/">…primary fields and one submit…</form>
    <p>Empty state: nothing saved yet.</p>
  </main>
  <footer><a href="DESK_URL" target="_top">Back to desk</a> · <a href="FIX_URL">Fix</a></footer>
</body>`

// pageBuildMessage is the Make (and shared) build prompt. The request leads.
func pageBuildMessage(text string) string {
	return fmt.Sprintf(`Build this tool. The request below is the spec.

Request:
%s

%s

%s`, strings.TrimSpace(text), qualityBar, contractReminder)
}

// wishBuildMessage expands a vague wish into the same spec before coding.
func wishBuildMessage(text string) string {
	return pageBuildMessage(text) + `

This started as a wish, which is often vague. Turn it into the 5-line spec before any file write. If it names no fields, invent the smallest set that makes the job real (one input, a list, one action) and show those on the page. Do not build a page that only restates the wish.`
}

// incompleteScaffoldNudge fires when the agent stopped after go.mod or a stub.
// original is the user's build prompt so the retry cannot forget the job.
func incompleteScaffoldNudge(original string) string {
	msg := `You stopped before the tool was usable. go.mod alone, or a page that does not do the job, is not done.

Finish the original request now. The HTML must show that job — not a hello page, not only footer links.

` + qualityBar + "\n\n" + contractReminder
	original = strings.TrimSpace(original)
	if original != "" {
		msg += "\n\nOriginal request (implement this, do not ignore it):\n" + original
	}
	msg += "\n\nDo not end the turn until main.go exists, go build succeeds, and GET / would show the job."
	return msg
}

// improvementMessage is the Fix prompt. The user's ask is the acceptance test.
func improvementMessage(appName, text string) string {
	return fmt.Sprintf(`[improvement request filed from the running tool %q]

The user asked for this improvement:

%s

Acceptance: this change must be visible on GET /. A comment, a renamed title, or a restyle that ignores the ask is not done.

Steps:
- Read the current page source first. Change only what this ask needs. Do not restyle or rename unrelated parts.
- Put the result in the HTML the server returns, not only in a code comment.
- In your final note, name the visible change in one sentence.
- Keep the server listening on $ADDR and serving GET /. Verify with: go build -o /tmp/app .
oozie republishes and restarts the tool when you finish.`, appName, strings.TrimSpace(text))
}

// remixMessage requires the mutation to show up in the copied app.
func remixMessage(appName, mutation string) string {
	return fmt.Sprintf(`This project is a remix of %q. Its source was copied here. Runtime data/ and databases were left behind so this desk starts empty.

Mutation — this must be visible on the page. A renamed title alone is not enough:

%s

Before editing:
- Read the copied source.
- List what stays and what changes.
- Apply the mutation in the UI and in behavior. Delete what no longer serves it.
- Rename the module path, page title, and user-visible names to fit the new tool.
- Keep durable records under data/ only. Keep listening on $ADDR.
- Verify with: go build -o /tmp/remix .`, appName, strings.TrimSpace(mutation))
}

// recipeBuildMessage leads with the recipe prompts as the spec.
func recipeBuildMessage(name, headline, description string, prompts []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Rebuild this tool from its recipe. The prompts below are the spec — implement that job, not a brochure page.\n\nApp: %s", name)
	if headline != "" {
		fmt.Fprintf(&b, " — %s", headline)
	}
	if description != "" {
		fmt.Fprintf(&b, "\n\nDescription: %s", description)
	}
	b.WriteString("\n\nPrompts that shaped it, in order (later prompts refine earlier ones — do not replay a conflict literally):\n")
	for i, p := range prompts {
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, p)
	}
	b.WriteString("\n\n")
	b.WriteString(qualityBar)
	b.WriteString("\n\nData isolation: fresh desk, empty storage. Durable records only under data/. Do not invent the original author's personal rows.\n\n")
	b.WriteString(contractReminder)
	return b.String()
}

const recipePlanSystem = `You write build plans for oozie, a local desk that rebuilds store apps as small Go web tools.
Read the store listing carefully. Produce a concrete plan the coding agent will follow.
Rules:
- Cover the same user job as the listing, simplified for one local tool.
- Do not invent cloud accounts, store APIs, proprietary code, or binary ports.
- Be specific about screens, fields, and flows from the listing — not generic filler.
- Plain text only. No markdown code fences.

Output exactly these labels, then a short flow:
Job:
Screens:
Fields:
Saved:
Done when:
Flow:`

// tasteRules pulls user rules out of TASTE.md so a skipped file read cannot drop them.
// Placeholder bullets and the Signals log are omitted. Empty means no personal rules yet.
func tasteRules(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	if i := strings.Index(strings.ToLower(body), "## signals"); i >= 0 {
		body = body[:i]
	}
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		low := strings.ToLower(trim)
		if strings.Contains(low, "add your rules") || strings.Contains(low, "write in plain language") || strings.Contains(low, "rules here override") {
			continue
		}
		// The default file's example wraps in parentheses and quoted lines.
		// Those are not the user's rules.
		if strings.HasPrefix(trim, "\"") || strings.HasPrefix(trim, "- (") {
			continue
		}
		lines = append(lines, trim)
	}
	out := strings.TrimSpace(strings.Join(lines, "\n"))
	if len(out) > 1200 {
		out = out[:1200]
	}
	return out
}
