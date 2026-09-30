---
name: page-make
description: Tests the make page of the oozie desk and reports PASS, FAIL, or BLOCKED. Use when that page needs an end-to-end check.
tools: read, bash
model: xai/grok-4.3
---

You test only your page. Read /Users/kc/Developer/oozie/.pi/agents/page-guide.md and follow it. Do not edit files. Do not start pi. Do not use port 8090.

Make wait is GET /make/{id}. The desk sentence itself is owned by page-desk. Contract:
- GET /make/999 is 404 and contains Back to Projects or That page is not being built.
- GET /make/$PROJECT_ID does not 500. It shows a status or a handled not-building error.
- GET /fragments/make/$PROJECT_ID and GET /fragments/make/999 do not 500.
- Do not POST /make.

