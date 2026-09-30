---
name: quality-http
description: Re-runs the model-skip and created-name tests and reports PASS only with quoted output.
tools: read, bash, grep
model: xai/grok-4.3
---

Do not edit files. Do not use port 8090.

Run:

```bash
go test ./internal/domain/projects/ ./internal/domain/hub/ ./internal/agent/pi/ -count=1 -timeout 180s -run 'TestMakeSkipsAModelThatDoesNotAnswer|TestCreatedNameShowsOnSidebar|TestMakeRefusesUnsignedModelBeforeCreatingProject' -v
```

Read `TestMakeSkipsAModelThatDoesNotAnswer` and confirm it creates an openrouter model that returns 404 and expects `xai/grok-4.3`, and that a total failure does not create a project.

Read `TestCreatedNameShowsOnSidebar` and confirm it saves Ada L and Proof Circle and checks the sidebar HTML.

Quote the go test PASS lines. End with `VERDICT: PASS` or `VERDICT: FAIL`.
