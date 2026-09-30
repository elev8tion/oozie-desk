# Quality checklist (desk loop)

Tick when verified with a real run or test evidence.

## 1. Desk auth / setup help
- [x] Desk shows a setup notice when no model is signed in (before you submit) — `SetupHint` + desk template
- [x] Failed Make still lands on desk with `This model is not signed in.` plus how to fix it (`pi /login`) — `TestMakeUnsignedModelStopsAtTheFrontDoor`
- [x] Build button stays usable; Make refuses cleanly until signed in — same test

## 2. Fix → wait → reopen /run
- [x] POST `/improve/{slug}` redirects to `/fix/{requestID}` (wait screen)
- [x] Wait screen polls until improve is done (or failed) — `partials/improve/status` hx-get
- [x] On success, auto-opens `/run/{appID}` — `ImproveStatus` phase open
- [x] On failure, one plain sentence + back to Fix form — `TestImproveStatusPhases`

## 3. Recipe / remix / wish → make-wait
- [x] Accept / edit draft recipe → `/make/{projectID}` (not agent page)
- [x] Import recipe JSON → `/make/{projectID}`
- [x] Remix → `/make/{projectID}`
- [x] Build wish now → `/make/{projectID}`
- [x] Inbox / share accept → `/make/{projectID}`
- [x] Recipe/remix builds auto-publish like Make (`trackFrontDoor` + draft)

## 4. Remove dead noise
- [x] Feedback form gone from project page; route removed — `TestDeadPathsRedirectHome` 404 on feedback
- [x] `/onboarding` redirects to desk — 303
- [x] `/installed-apps` redirects to `/store?filter=installed` — 303
- [x] Dead `Home` handler redirects to desk

## 5. Vocabulary: “tool”
- [x] Desk / make wait / improve wait / wishes copy say **tool**
- [x] Store / run chrome tool-first; insights use tool
- [x] Projects escape hatch still says “project”

## Evidence (this pass)
```
go test ./... -count=1  → all ok
TestDeadPathsRedirectHome PASS
TestMakeUnsignedModelStopsAtTheFrontDoor PASS (includes pi /login)
TestSetupHintWhenUnsigned PASS
TestImproveStatusPhases PASS
```
