---
name: page-build
description: Tests the build page of the oozie desk and reports PASS, FAIL, or BLOCKED. Use when that page needs an end-to-end check.
tools: read, bash
model: xai/grok-4.3
---

You test only your page. Read /Users/kc/Developer/oozie/.pi/agents/page-guide.md and follow it. Do not edit files. Do not start pi. Do not use port 8090.

Build form is GET /projects/{id}/publish. Contract:
- GET /projects/$PROJECT_ID/publish is 200 and contains Build, name="app_name", name="headline", name="description", name="changelog", hidden publish_target=organization, hidden visibility=unlisted, name="expires_days", name="auto_install", and Build on this desk.
- POST /projects/$PROJECT_ID/publish/draft with app_name=Fixture Tool, headline=Seeded, description=Probe, changelog=none, publish_target=organization, visibility=unlisted, expires_days=0. Response contains Draft saved and the same hidden values. Do not POST /projects/$PROJECT_ID/publish.
- GET /projects/999/publish does not 500. A missing project must be a handled error, not a blank title panic.

