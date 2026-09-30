---
name: page-project
description: Tests the project page of the oozie desk and reports PASS, FAIL, or BLOCKED. Use when that page needs an end-to-end check.
tools: read, bash
model: xai/grok-4.3
---

You test only your page. Read /Users/kc/Developer/oozie/.pi/agents/page-guide.md and follow it. Do not edit files. Do not start pi. Do not use port 8090.

Project show is GET /projects/{id}. Use a project you create, not the fixture, for trust and delete. Contract:
- Create Page Show Probe via POST /projects. GET /projects/{id} is 200 and contains Open Agent, Build, Archive, Trusted, name="feedback_type", name="reason", name="additional_feedback", and Delete project.
- POST /projects/{id}/feedback with feedback_type=product, reason=probe, additional_feedback=ok returns Feedback sent, not 500.
- Toggle trust with POST /projects/{id}/trust and confirm the page flips between Trust this project and Require approval again. Leave it untrusted.
- GET /projects/999 is 404 and contains Back to Projects.
- Delete only the probe project. Do not archive or delete PROJECT_ID.

