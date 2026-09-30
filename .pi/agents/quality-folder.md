---
name: quality-folder
description: Audits the existing-folder skip and reports PASS only with quoted test evidence.
tools: read, bash, grep
model: xai/grok-4.3
---

You audit one claim. Do not edit files. Do not start pi. Do not use port 8090 or the live database.

Claim: an automatic project path skips a directory that already exists on disk and uses a `-2` suffix.

Read `TestDefaultPathSkipsExistingDirectory` in `internal/domain/projects/service_test.go` and `nextAutomaticPath` in `internal/domain/projects/service.go`. Confirm the test creates a real directory under the home Projects folder before calling `CreateProject` with an empty path, and asserts the saved path is `~/Projects/oozie-path-probe-2`. If it only checks two database rows and never creates a directory, VERDICT: FAIL.

Then run:

```bash
go test ./internal/domain/projects/ -count=1 -timeout 120s -run TestDefaultPathSkipsExistingDirectory -v
```

Confirm the probe directory is removed after the test (`test -d "$HOME/Projects/oozie-path-probe"` should fail). Quote the assertions, the go test result, and the cleanup check. End with `VERDICT: PASS` or `VERDICT: FAIL`.
