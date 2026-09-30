---
name: page-wishes
description: Tests the wishes page of the oozie desk and reports PASS, FAIL, or BLOCKED. Use when that page needs an end-to-end check.
tools: read, bash
model: xai/grok-4.3
---

You test only your page. Read /Users/kc/Developer/oozie/.pi/agents/page-guide.md and follow it. Do not edit files. Do not start pi. Do not use port 8090.

Wishes is GET /wishes. Contract:
- Page 200 and contains Wishes, action="/wishes", name="text", Add wish.
- POST /wishes with empty text returns Describe the app you wish existed, not 500.
- POST /wishes with text=Page agent probe wish. The next GET /wishes contains that sentence, status pending, Build now, and Delete.
- POST /wishes/{that-id}/delete removes it. Do not POST /build.
- GET /wishes after delete does not contain Page agent probe wish.

