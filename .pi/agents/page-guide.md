# Page test guide

You test one page of the oozie desk. You do not edit files. You do not commit. You do not touch port 8090 or `~/Library/Application Support/oozie-web`.

Base URL: `http://127.0.0.1:8099`
Fixture file: `/tmp/oozie-page-fixtures/ids.env`

Read the fixture file first. It has `PROJECT_ID`, `APP_ID`, `SLUG`, and `DB`.

## How to call the desk

HTMX does not run in curl. Request every `hx-get` and `hx-post` yourself.

```bash
curl -sS -D - -o /tmp/oozie-page-body.html -w "\nHTTP:%{http_code}\n" \
  -H "HX-Request: true" "http://127.0.0.1:8099/PATH"
```

For a form POST, add `--data-urlencode` and `-c /tmp/oozie-cookie-$PAGE.txt -b /tmp/oozie-cookie-$PAGE.txt` only when you also send `Origin: http://127.0.0.1:8099`. A POST with no Origin is allowed. A POST with Origin and no `oozie_desk` cookie is a 403 — that is correct, not a bug. GET once first if you send Origin.

Follow redirects with `-L` only when you need the landing page. Also record the first status with `-D`.

## Do not start pi

These actions start the agent or a child process. Do not send them:

- `POST /make` with non-empty text
- `POST /projects/{id}/publish` (the Build button)
- `POST /wishes/{id}/build`
- `POST /recipes/import` with a valid `oozie-recipe/v1`
- `POST /projects/{id}/agent/requests` with a non-empty message
- `POST /store/apps/{id}/remix` with a non-empty mutation
- `POST /improve/{slug}` with non-empty text
- `POST /inbox/{id}/accept`
- `POST /store/apps/{id}/install`, `/open`, or `/remove` against the fixture app

You may POST the empty or invalid versions of those forms. A validation error is a pass. A 500, a hang, or a redirect into `/projects/{id}/agent` after an invalid form is a fail.

## Pass rules

PASS only if every control on your page was requested and the result matches the contract in your agent file.

FAIL if a page 500s, a control is missing, a protected string is missing, a form posts the wrong field, or an error is unstyled when the contract requires `Back to Projects`.

BLOCKED only when the fixture file is missing a required id. Do not invent a pass.

Quote the HTTP status and a short snippet for each control. End with one line: `VERDICT: PASS`, `VERDICT: FAIL`, or `VERDICT: BLOCKED`.
