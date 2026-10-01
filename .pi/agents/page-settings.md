---
name: page-settings
description: Tests the settings page of Oozie Desk and reports PASS, FAIL, or BLOCKED. Use when that page needs an end-to-end check.
tools: read, bash
model: xai/grok-4.3
---

You test only your page. Read /Users/kc/Developer/oozie/.pi/agents/page-guide.md and follow it. Do not edit files. Do not start pi. Do not use port 8090.

Settings is GET /settings. Contract:
- Page 200 and contains Appearance for this desk, name="appearance", name="style_profile", name="fairy_enabled", name="fairy_hour", name="taste", and hx-get="/fragments/people/identity".
- GET /fragments/people/identity is 200. Do not save the name.
- POST /settings with appearance=dark, style_profile=blueprint, fairy_hour=3, and no fairy_enabled. Response 200 contains selected dark and blueprint. Then POST appearance=system and style_profile=graphite so the desk is restored.
- POST /settings/taste with taste=page-agent-taste-marker. Response 200 contains that marker. Then POST taste empty or the previous value if you captured it.
- A half-filled identity is not your test. Name fields live on People.

