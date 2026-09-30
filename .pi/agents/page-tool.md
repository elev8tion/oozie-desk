---
name: page-tool
description: Tests the tool page of the oozie desk and reports PASS, FAIL, or BLOCKED. Use when that page needs an end-to-end check.
tools: read, bash
model: xai/grok-4.3
---

You test only your page. Read /Users/kc/Developer/oozie/.pi/agents/page-guide.md and follow it. Do not edit files. Do not start pi. Do not use port 8090.

Tool detail is GET /store/apps/{id}. Contract:
- GET /store/apps/$APP_ID is 200 and contains Fixture Tool, Running or Stopped, Request a fix, Export recipe, Remove from desk, Remix, name="mutation", and hx-get for the share fragment.
- GET /fragments/shares/$APP_ID is 200 and contains Not shared or Shared. If a peer select exists, do not submit share.
- POST /store/apps/$APP_ID/remix with empty mutation is a handled validation error and does not redirect to an agent page.
- GET /store/apps/999 is 404 and contains Back to Projects.
- Do not install, open, remove, or share the fixture.

