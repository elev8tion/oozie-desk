package projects

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oozie/internal/db"
)

type fakeBuilder struct {
	fail bool
}

func (f fakeBuilder) Build(workdir, appName string) (string, error) {
	if f.fail {
		return "", os.ErrNotExist
	}
	dir := filepath.Join(workdir, "dist")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	bin := filepath.Join(dir, strings.ToLower(strings.ReplaceAll(appName, " ", "-")))
	py := "#!/usr/bin/env python3\nimport os\nfrom http.server import BaseHTTPRequestHandler, HTTPServer\nclass H(BaseHTTPRequestHandler):\n    def do_GET(self):\n        self.send_response(204)\n        self.end_headers()\n    def log_message(self, *args):\n        pass\nHTTPServer((\"127.0.0.1\", int(os.environ.get(\"PORT\", \"8080\"))), H).serve_forever()\n"
	if err := os.WriteFile(bin, []byte(py), 0o755); err != nil {
		return "", err
	}
	return bin, nil
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := db.RunMigrations(database, os.DirFS(filepath.Join("..", "..", "..", "migrations"))); err != nil {
		t.Fatal(err)
	}
	tasteDirOverride = t.TempDir()
	t.Cleanup(func() { tasteDirOverride = "" })
	s := NewService(NewRepo(database))
	t.Cleanup(func() { s.StopRuntimes() })
	return s
}

func TestCreateProjectValidation(t *testing.T) {
	s := newTestService(t)
	if _, err := s.CreateProject(context.Background(), "  ", "", true); err == nil {
		t.Fatal("expected validation error for blank name")
	}
	p, err := s.CreateProject(context.Background(), "My Tool", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if p.ProjectPathDisplay != "~/Projects/my-tool" {
		t.Errorf("default path = %q", p.ProjectPathDisplay)
	}
}

func TestDefaultPathSkipsExistingDirectory(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "Projects", "oozie-path-probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s := newTestService(t)
	p, err := s.CreateProject(context.Background(), "oozie-path-probe", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if p.ProjectPathDisplay != "~/Projects/oozie-path-probe-2" {
		t.Fatalf("reused an existing folder: %s", p.ProjectPathDisplay)
	}
}

func TestPublishWithoutDraftUsesDefaults(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	s.SetBuilder(fakeBuilder{})
	p, _ := s.CreateProject(ctx, "NoDraft", filepath.Join(t.TempDir(), "p"), true)
	if err := s.Publish(ctx, p.ID); err != nil {
		t.Fatalf("publish without a draft should fall back to defaults, got %v", err)
	}
	s.WaitForJobs()
	d, err := s.GetDraft(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.AppName != "NoDraft" || d.Headline == "" || d.Description == "" {
		t.Errorf("auto-created draft missing defaults: %+v", d)
	}
	jobs, _ := s.ListJobs(ctx, "")
	if len(jobs) != 1 || jobs[0].Status != "succeeded" {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestSaveDraftFillsDefaults(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	p, _ := s.CreateProject(ctx, "Sparse", filepath.Join(t.TempDir(), "p"), true)
	if err := s.SaveDraft(ctx, PublishDraft{ProjectID: p.ID, AppName: "Sparse"}); err != nil {
		t.Fatalf("sparse draft should be accepted with defaults, got %v", err)
	}
	d, _ := s.GetDraft(ctx, p.ID)
	if d.Headline == "" || d.Description == "" {
		t.Errorf("defaults not filled: %+v", d)
	}
}

func TestPublishLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	s.SetBuilder(fakeBuilder{})
	workdir := filepath.Join(t.TempDir(), "proj")
	p, _ := s.CreateProject(ctx, "Shiny", workdir, true)
	draft := PublishDraft{ProjectID: p.ID, AppName: "Shiny", Headline: "h", Description: "d"}
	if err := s.SaveDraft(ctx, draft); err != nil {
		t.Fatal(err)
	}

	if err := s.Publish(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	s.WaitForJobs()

	for _, seed := range []string{"DESIGN.md"} {
		if _, err := os.Stat(filepath.Join(workdir, seed)); err != nil {
			t.Errorf("%s was not seeded into the project workdir: %v", seed, err)
		}
	}

	jobs, err := s.ListJobs(ctx, "")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %v (err %v)", jobs, err)
	}
	if jobs[0].Status != "succeeded" {
		t.Fatalf("job status = %s (%s)", jobs[0].Status, jobs[0].ErrorMessage)
	}
	if jobs[0].ProjectName != "Shiny" {
		t.Errorf("job project name = %q", jobs[0].ProjectName)
	}

	apps, err := s.ListStoreApps(ctx, "", "")
	if err != nil || len(apps) != 1 {
		t.Fatalf("store apps = %v (err %v)", apps, err)
	}
	if apps[0].ArtifactPath == "" {
		t.Error("store app has no artifact path")
	}

	// Republishing updates the same store app instead of duplicating it.
	if err := s.Publish(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	s.WaitForJobs()
	apps, _ = s.ListStoreApps(ctx, "", "")
	if len(apps) != 1 {
		t.Fatalf("after republish, store apps = %d, want 1", len(apps))
	}
}

func TestPublishFailureRecordsError(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	s.SetBuilder(fakeBuilder{fail: true})
	p, _ := s.CreateProject(ctx, "Broken", filepath.Join(t.TempDir(), "b"), true)
	_ = s.SaveDraft(ctx, PublishDraft{ProjectID: p.ID, AppName: "Broken", Headline: "h", Description: "d"})

	if err := s.Publish(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	s.WaitForJobs()

	jobs, _ := s.ListJobs(ctx, "failed")
	if len(jobs) != 1 || jobs[0].ErrorMessage == "" {
		t.Fatalf("expected one failed job with an error message, got %v", jobs)
	}
	if apps, _ := s.ListStoreApps(ctx, "", ""); len(apps) != 0 {
		t.Errorf("failed publish must not create a store app, got %d", len(apps))
	}
}

func TestUninstallAndRemoveStoreApp(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	p, _ := s.CreateProject(ctx, "Gone Soon", filepath.Join(t.TempDir(), "g"), true)
	id, err := s.repo.UpsertStoreApp(ctx, p.ID, PublishDraft{AppName: "Gone Soon", Headline: "h", Description: "d"}, "", "gone-soon")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.repo.InstallApp(ctx, id); err != nil {
		t.Fatal(err)
	}

	if err := s.UninstallApp(ctx, id); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	app, _ := s.repo.GetStoreApp(ctx, id)
	if app.Installed || app.InstallCount != 0 {
		t.Errorf("after uninstall: installed=%v count=%d", app.Installed, app.InstallCount)
	}

	if err := s.RemoveStoreApp(ctx, id); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if apps, _ := s.ListStoreApps(ctx, "", ""); len(apps) != 0 {
		t.Errorf("store should be empty after remove, has %d", len(apps))
	}
}

func TestInstallRequiresArtifact(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	p, _ := s.CreateProject(ctx, "NoArt", filepath.Join(t.TempDir(), "n"), true)
	id, err := s.repo.UpsertStoreApp(ctx, p.ID, PublishDraft{AppName: "NoArt", Headline: "h", Description: "d"}, "", "noart")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InstallApp(ctx, id); err == nil {
		t.Fatal("expected validation error installing an app with no artifact")
	}
}

// TestImproveLoopRepublishes proves the fix-me wormhole closes itself: an
// improve-linked agent request that settles 'completed' republishes the
// project and marks the improvement done.
func TestImproveLoopRepublishes(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	s.SetBuilder(fakeBuilder{})
	p, _ := s.CreateProject(ctx, "Fixable", filepath.Join(t.TempDir(), "f"), true)
	_ = s.SaveDraft(ctx, PublishDraft{ProjectID: p.ID, AppName: "Fixable", Headline: "h", Description: "d"})
	if err := s.Publish(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	s.WaitForJobs()
	app, err := s.AppBySlug(ctx, "fixable")
	if err != nil {
		t.Fatalf("published app not found by slug: %v", err)
	}

	session, _ := s.repo.GetSession(ctx, p.ID)
	reqID, err := s.repo.CreateAgentRequest(ctx, session.ID, "build", "make it better")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.repo.InsertImproveRequest(ctx, reqID, app.ID, "make it better"); err != nil {
		t.Fatal(err)
	}

	s.RequestSettled(p.ID, reqID, "completed")
	s.WaitForJobs()

	imp, err := s.repo.ImproveByRequest(ctx, reqID)
	if err != nil || imp == nil {
		t.Fatalf("improve request lookup: %v", err)
	}
	if imp.Status != "done" {
		t.Fatalf("improve status = %q, want done", imp.Status)
	}
	jobs, _ := s.ListJobs(ctx, "succeeded")
	if len(jobs) != 2 {
		t.Fatalf("expected the improvement to trigger a second publish job, got %d succeeded", len(jobs))
	}
}

// TestImproveLoopFailedAgent marks the improvement failed when the agent
// request settles unsuccessfully, without republishing.
func TestImproveLoopFailedAgent(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	s.SetBuilder(fakeBuilder{})
	p, _ := s.CreateProject(ctx, "Unfixable", filepath.Join(t.TempDir(), "u"), true)
	id, _ := s.repo.UpsertStoreApp(ctx, p.ID, PublishDraft{AppName: "Unfixable", Headline: "h", Description: "d"}, "", "unfixable")
	session, _ := s.repo.GetSession(ctx, p.ID)
	reqID, _ := s.repo.CreateAgentRequest(ctx, session.ID, "build", "impossible")
	_ = s.repo.InsertImproveRequest(ctx, reqID, id, "impossible")

	s.RequestSettled(p.ID, reqID, "failed")
	s.WaitForJobs()

	imp, _ := s.repo.ImproveByRequest(ctx, reqID)
	if imp == nil || imp.Status != "failed" {
		t.Fatalf("improve status = %v, want failed", imp)
	}
	if jobs, _ := s.ListJobs(ctx, ""); len(jobs) != 0 {
		t.Fatalf("failed agent run must not publish, got %d jobs", len(jobs))
	}
}

// TestReaperRemovesExpiredApps proves disposable apps self-destruct: an
// app published with a past expiry is uninstalled and delisted by the
// reaper sweep, while permanent apps survive.
func TestReaperRemovesExpiredApps(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	p, _ := s.CreateProject(ctx, "Ephemeral", filepath.Join(t.TempDir(), "e"), true)
	id, err := s.repo.UpsertStoreApp(ctx, p.ID, PublishDraft{AppName: "Ephemeral", Headline: "h", Description: "d", ExpiresDays: 1}, "", "ephemeral")
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := s.CreateProject(ctx, "Forever", filepath.Join(t.TempDir(), "f2"), true)
	if _, err := s.repo.UpsertStoreApp(ctx, p2.ID, PublishDraft{AppName: "Forever", Headline: "h", Description: "d"}, "", "forever"); err != nil {
		t.Fatal(err)
	}

	// Not yet expired: sweep must keep it.
	s.reapExpiredApps(ctx)
	if _, err := s.GetStoreApp(ctx, id); err != nil {
		t.Fatalf("unexpired app was reaped: %v", err)
	}

	// Force the expiry into the past and sweep again.
	if _, err := s.repo.db.ExecContext(ctx, `UPDATE store_apps SET expires_at=datetime('now','-1 hour') WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	s.reapExpiredApps(ctx)
	if _, err := s.GetStoreApp(ctx, id); err == nil {
		t.Fatal("expired app still in store after reap")
	}
	apps, _ := s.ListStoreApps(ctx, "", "")
	if len(apps) != 1 || apps[0].Name != "Forever" {
		t.Fatalf("permanent app should survive the reap, got %v", apps)
	}
}

// TestRemixCopiesSource proves remixing forks the workdir (minus build
// products) into a new project. Agent send fails (no pi in tests) but the
// fork itself must be complete by then.
func TestRemixCopiesSource(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	srcDir := filepath.Join(t.TempDir(), "orig")
	p, _ := s.CreateProject(ctx, "Origin", srcDir, true)
	id, _ := s.repo.UpsertStoreApp(ctx, p.ID, PublishDraft{AppName: "Origin", Headline: "h", Description: "d"}, "", "origin")
	writeTestFile(t, filepath.Join(srcDir, "Package.swift"), "// package")
	writeTestFile(t, filepath.Join(srcDir, "Sources", "main.swift"), "print(1)")
	writeTestFile(t, filepath.Join(srcDir, ".build", "junk"), "x")
	writeTestFile(t, filepath.Join(srcDir, "dist", "junk"), "x")
	writeTestFile(t, filepath.Join(srcDir, "data", "app.db"), "CREATOR-SECRET-ROWS")
	writeTestFile(t, filepath.Join(srcDir, "notes.sqlite"), "MORE-SECRET")
	writeTestFile(t, filepath.Join(srcDir, ".env"), "TOKEN=secret")

	if _, err := s.RemixApp(ctx, id, ""); err == nil {
		t.Fatal("empty mutation must be rejected")
	}
	remix, err := s.RemixApp(ctx, id, "make it purple")
	if _, ok := err.(ErrValidation); !ok {
		t.Fatalf("expected agent-unavailable validation error, got %v", err)
	}
	if remix.ID == 0 {
		t.Fatal("remix project was not created")
	}
	home, _ := os.UserHomeDir()
	dstDir := filepath.Join(home, "Projects", "origin-remix")
	t.Cleanup(func() {
		// Only clean what this test created — never a real user directory.
		if _, err := os.Stat(filepath.Join(dstDir, "Sources", "main.swift")); err == nil {
			os.RemoveAll(dstDir)
		}
	})
	if _, err := os.Stat(filepath.Join(dstDir, "Package.swift")); err != nil {
		t.Errorf("source not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "Sources", "main.swift")); err != nil {
		t.Errorf("nested source not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, ".build")); err == nil {
		t.Error(".build must not be copied")
	}
	if _, err := os.Stat(filepath.Join(dstDir, "dist")); err == nil {
		t.Error("dist must not be copied")
	}
	if _, err := os.Stat(filepath.Join(dstDir, "data")); err == nil {
		t.Error("data/ must not be copied — creator usage stays on the source desk")
	}
	if _, err := os.Stat(filepath.Join(dstDir, "notes.sqlite")); err == nil {
		t.Error("*.sqlite must not be copied")
	}
	if _, err := os.Stat(filepath.Join(dstDir, ".env")); err == nil {
		t.Error(".env must not be copied")
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRecipeRoundTrip exports a published app as a recipe and imports it
// back into a fresh project.
func TestRecipeRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	dir := filepath.Join(t.TempDir(), "r")
	p, _ := s.CreateProject(ctx, "Recipe App", dir, true)
	id, _ := s.repo.UpsertStoreApp(ctx, p.ID, PublishDraft{AppName: "Recipe App", Headline: "hl", Description: "de"}, "", "recipe-app")
	session, _ := s.repo.GetSession(ctx, p.ID)
	_, _ = s.repo.CreateAgentRequest(ctx, session.ID, "build", "build me a timer")
	_, _ = s.repo.CreateAgentRequest(ctx, session.ID, "build", "make it purple")

	// Plant creator usage data beside the project. Export must ignore it.
	secret := "CREATOR-PRIVATE-FEEDING-LOG-9f3a"
	writeTestFile(t, filepath.Join(dir, "data", "app.db"), secret)
	writeTestFile(t, filepath.Join(dir, "usage.db"), secret)

	rec, err := s.ExportRecipe(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Kind != "oozie-recipe/v1" || len(rec.Prompts) != 2 || rec.Prompts[1] != "make it purple" {
		t.Fatalf("recipe = %+v", rec)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "CREATOR-PRIVATE") {
		t.Fatalf("recipe leaked creator usage data: %s", raw)
	}
	if strings.Contains(rec.Design, secret) {
		t.Fatal("design field carried usage data")
	}

	// Import: agent send fails without pi, but the project must exist and
	// carry the recipe's identity.
	imported, err := s.ImportRecipe(ctx, string(raw))
	if _, ok := err.(ErrValidation); !ok {
		t.Fatalf("expected agent-unavailable validation error, got %v", err)
	}
	if imported.ID == 0 || imported.Name != "Recipe App" {
		t.Fatalf("imported project = %+v", imported)
	}
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(home, "Projects", "recipe-app")); err == nil {
		os.RemoveAll(filepath.Join(home, "Projects", "recipe-app"))
	}

	if _, err := s.ImportRecipe(ctx, `{"kind":"nope"}`); err == nil {
		t.Fatal("wrong kind must be rejected")
	}
}

// TestWishLifecycle covers the fairy loop's plumbing without pi: a wish is
// added, a build attempt fails cleanly (no agent), and the settle path
// publishes and marks a wish built when the agent completes.
func TestWishLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	s.SetBuilder(fakeBuilder{})

	if err := s.AddWish(ctx, "   "); err == nil {
		t.Fatal("blank wish must be rejected")
	}
	if err := s.AddWish(ctx, "I wish I had an app that tracks my sourdough starter feedings"); err != nil {
		t.Fatal(err)
	}
	wishes, _ := s.ListWishes(ctx)
	if len(wishes) != 1 || wishes[0].Status != "pending" {
		t.Fatalf("wishes = %+v", wishes)
	}

	// No pi in tests: BuildWish must fail the wish honestly.
	if _, err := s.BuildWish(ctx, wishes[0].ID); err == nil {
		t.Fatal("expected agent-unavailable error")
	}
	w, _ := s.repo.GetWish(ctx, wishes[0].ID)
	if w.Status != "failed" {
		t.Fatalf("wish status = %q, want failed", w.Status)
	}
	// Clean up the wish project's default ~/Projects/<slug> directory.
	if ps, _ := s.ListProjects(ctx, "", ""); len(ps) > 0 {
		home, _ := os.UserHomeDir()
		dir := filepath.Join(home, strings.TrimPrefix(ps[0].ProjectPathDisplay, "~/"))
		if strings.HasPrefix(dir, filepath.Join(home, "Projects")+string(filepath.Separator)) {
			os.RemoveAll(dir)
		}
	}

	// Simulate the agent finishing: settleWish must publish and grant.
	grDir := filepath.Join(t.TempDir(), "gr")
	p, _ := s.CreateProject(ctx, "Granted", grDir, true)
	writeTestFile(t, filepath.Join(grDir, "go.mod"), "module granted\n\ngo 1.24\n")
	writeTestFile(t, filepath.Join(grDir, "main.go"), "package main\nfunc main() {}\n")
	_ = s.repo.SaveDraft(ctx, PublishDraft{ProjectID: p.ID, AppName: "Granted", Headline: "h", Description: "d"})
	_ = s.AddWish(ctx, "another wish")
	wishes, _ = s.ListWishes(ctx)
	var wid int64
	for _, x := range wishes {
		if x.Status == "pending" {
			wid = x.ID
		}
	}
	_ = s.repo.SetWishBuilding(ctx, wid, p.ID)
	s.wishByRequest.Store(int64(4242), wid)
	s.RequestSettled(p.ID, 4242, "completed")
	s.WaitForJobs()
	w, _ = s.repo.GetWish(ctx, wid)
	if w.Status != "built" {
		t.Fatalf("wish status = %q (%s), want built", w.Status, w.Error)
	}
	if apps, _ := s.ListStoreApps(ctx, "", ""); len(apps) != 1 {
		t.Fatalf("granted wish should have published an app, got %d", len(apps))
	}
}

// TestSettleWishNoCode: an agent run that "completes" without leaving any
// buildable output must fail the wish with the agent's own final message,
// not the misleading "no Package.swift" publish error.
func TestSettleWishNoCode(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	s.SetBuilder(fakeBuilder{})

	p, _ := s.CreateProject(ctx, "Declined", t.TempDir(), true)
	_ = s.AddWish(ctx, "an app that strips license checks from other apps")
	wishes, _ := s.ListWishes(ctx)
	wid := wishes[0].ID
	_ = s.repo.SetWishBuilding(ctx, wid, p.ID)

	session, _ := s.repo.GetSession(ctx, p.ID)
	reqID, _ := s.repo.CreateAgentRequest(ctx, session.ID, "build", "grant the wish")
	final := "I can't build this app because it would strip licenses."
	_ = s.repo.FinalizeAssistant(ctx, reqID, final)

	s.wishByRequest.Store(reqID, wid)
	s.settleWish(p.ID, reqID, "completed")

	w, _ := s.repo.GetWish(ctx, wid)
	if w.Status != "failed" {
		t.Fatalf("wish status = %q (%s), want failed", w.Status, w.Error)
	}
	if !strings.Contains(w.Error, "strip licenses") {
		t.Errorf("wish error should quote the agent's message, got %q", w.Error)
	}
	if strings.Contains(w.Error, "go.mod") || strings.Contains(w.Error, "Package.swift") {
		t.Errorf("wish error still mentions the publish toolchain: %q", w.Error)
	}
}

func TestWishNoCodeErrorTruncation(t *testing.T) {
	long := strings.Repeat("# Sorry\nI cannot build this.\n\n", 40) // ~1200 chars, multi-line
	got := wishNoCodeError(long)
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("output must be single-line, got %q", got)
	}
	if n := len([]rune(got)); n > 320 {
		t.Errorf("output too long: %d runes", n)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("long input should be truncated with an ellipsis: %q", got)
	}
	if got := wishNoCodeError("  \n \t "); got != "the agent finished without producing a tool and left no message — see the project timeline" {
		t.Errorf("empty input fallback = %q", got)
	}
}

// TestTasteAccumulatesSignals proves remix mutations land in TASTE.md and
// that the taste file is materialized into project workdirs.
func TestTasteAccumulatesSignals(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	srcDir := filepath.Join(t.TempDir(), "t")
	p, _ := s.CreateProject(ctx, "Tasteful", srcDir, true)
	id, _ := s.repo.UpsertStoreApp(ctx, p.ID, PublishDraft{AppName: "Tasteful", Headline: "h", Description: "d"}, "", "tasteful")
	writeTestFile(t, filepath.Join(srcDir, "Package.swift"), "// p")

	_, _ = s.RemixApp(ctx, id, "menu-bar apps over windows, always")
	home, _ := os.UserHomeDir()
	t.Cleanup(func() { os.RemoveAll(filepath.Join(home, "Projects", "tasteful-remix")) })

	taste := s.LoadTaste()
	if !strings.Contains(taste, "menu-bar apps over windows") || !strings.Contains(taste, "[remix Tasteful]") {
		t.Fatalf("remix signal missing from taste:\n%s", taste)
	}
	dir := filepath.Join(t.TempDir(), "mat")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s.materializeTaste(dir)
	body, err := os.ReadFile(filepath.Join(dir, "TASTE.md"))
	if err != nil || !strings.Contains(string(body), "menu-bar apps") {
		t.Fatalf("taste not materialized: %v", err)
	}
}

// TestStalePromptsSweptAtStartup: prompts left pending by a dead process
// are expired by the startup sweep so they can't wedge the agent page.
func TestStalePromptsSweptAtStartup(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	p, _ := s.CreateProject(ctx, "Stuck", filepath.Join(t.TempDir(), "s"), false)
	session, _ := s.repo.GetSession(ctx, p.ID)
	reqID, _ := s.repo.CreateAgentRequest(ctx, session.ID, "build", "do things")
	_ = s.repo.InsertPermission(ctx, p.ID, reqID, "rpc-1", "bash", "run a build")
	_ = s.repo.InsertQuestion(ctx, p.ID, reqID, "rpc-2", "Which color?", "[]")

	s.RecoverOrphanedJobs(ctx)

	if perm, _ := s.repo.PendingPermission(ctx, p.ID); perm != nil {
		t.Fatalf("stale permission survived the sweep: %+v", perm)
	}
	if q, _ := s.repo.PendingQuestion(ctx, p.ID); q != nil {
		t.Fatalf("stale question survived the sweep: %+v", q)
	}
}

// TestStaleAgentRequestsSweptAtStartup: a crash mid-run must not leave the
// agent UI stuck on "streaming" forever.
func TestStaleAgentRequestsSweptAtStartup(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	p, _ := s.CreateProject(ctx, "Midflight", filepath.Join(t.TempDir(), "m"), true)
	session, _ := s.repo.GetSession(ctx, p.ID)
	reqID, _ := s.repo.CreateAgentRequest(ctx, session.ID, "build", "build the app")
	_ = s.repo.InsertMessage(ctx, reqID, "assistant", "loading", "still thinking…")
	_ = s.repo.InsertToolStart(ctx, reqID, "call-1", "bash: swift build (running)")

	s.RecoverOrphanedJobs(ctx)

	page, err := s.AgentPage(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if page.Streaming {
		t.Fatal("agent page still reports streaming after the startup sweep")
	}
	found := false
	for _, req := range page.Requests {
		if req.ID == reqID {
			found = true
			if req.Status != "failed" {
				t.Fatalf("stranded request status = %q, want failed", req.Status)
			}
		}
	}
	if !found {
		t.Fatal("stranded request missing from agent page")
	}
	for _, msg := range page.Messages {
		if msg.RequestID == reqID && msg.Status == "loading" {
			t.Fatalf("loading message survived the sweep: %+v", msg)
		}
	}
}

// TestDeleteProjectPermanently: deletion removes the project, its agent
// history, its store listing — and only touches disk when asked, guarded
// to real project dirs.
func TestDeleteProjectPermanently(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	s.SetBuilder(fakeBuilder{})
	home, _ := os.UserHomeDir()
	workdir := filepath.Join(home, "Projects", "oozie-delete-test")
	t.Cleanup(func() { os.RemoveAll(workdir) })
	p, _ := s.CreateProject(ctx, "Doomed", workdir, true)
	_ = s.SaveDraft(ctx, PublishDraft{ProjectID: p.ID, AppName: "Doomed", Headline: "h", Description: "d"})
	if err := s.Publish(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	s.WaitForJobs()
	session, _ := s.repo.GetSession(ctx, p.ID)
	_, _ = s.repo.CreateAgentRequest(ctx, session.ID, "build", "hello")

	if err := s.DeleteProject(ctx, p.ID, true); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetProject(ctx, p.ID); err == nil {
		t.Fatal("project still exists")
	}
	if apps, _ := s.ListStoreApps(ctx, "", ""); len(apps) != 0 {
		t.Fatalf("store app survived deletion: %v", apps)
	}
	if prompts, _ := s.repo.UserPrompts(ctx, p.ID); len(prompts) != 0 {
		t.Fatalf("agent history survived deletion: %v", prompts)
	}
	if _, err := os.Stat(workdir); err == nil {
		t.Fatal("workdir survived delete_files=true")
	}
}

// TestSafeDeleteGuards: the disk guard refuses anything outside home or
// at the top level of home.
func TestSafeDeleteGuards(t *testing.T) {
	home, _ := os.UserHomeDir()
	for _, dir := range []string{home, filepath.Join(home, "Documents"), "/tmp/whatever", "/"} {
		if err := safeDeleteProjectDir(dir); err == nil {
			t.Errorf("guard allowed deleting %s", dir)
		}
	}
	// A legit project dir two levels deep is allowed.
	ok := filepath.Join(home, "Projects", "oozie-guard-test")
	if err := os.MkdirAll(ok, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := safeDeleteProjectDir(ok); err != nil {
		t.Errorf("guard refused a legit project dir: %v", err)
	}
	// Deleting a dir that's already gone is fine.
	if err := safeDeleteProjectDir(ok); err != nil {
		t.Errorf("guard errored on missing dir: %v", err)
	}
}

// TestAutoInstallOnPublish: with no saved draft, a publish auto-creates a
// default draft with auto-install on — the app lands installed. An
// explicit draft with auto-install off stays uninstalled.
func TestAutoInstallOnPublish(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	s.SetBuilder(fakeBuilder{})
	p, _ := s.CreateProject(ctx, "Clicky", filepath.Join(t.TempDir(), "c"), true)
	if err := s.Publish(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	s.WaitForJobs()
	app, err := s.AppBySlug(ctx, "clicky")
	if err != nil {
		t.Fatal(err)
	}
	if !app.Installed {
		t.Fatal("publish with default draft should auto-install")
	}
	if app.PublicURL == "" || !tcpUp(app.PublicURL) {
		t.Fatalf("app not reachable at %q", app.PublicURL)
	}
	url := app.PublicURL
	if err := s.UninstallApp(ctx, app.ID); err != nil {
		t.Fatal(err)
	}
	if tcpUp(url) {
		t.Fatalf("app still reachable at %s after stop", url)
	}

	p2, _ := s.CreateProject(ctx, "Shy", filepath.Join(t.TempDir(), "s"), true)
	_ = s.SaveDraft(ctx, PublishDraft{ProjectID: p2.ID, AppName: "Shy", Headline: "h", Description: "d", AutoInstall: false})
	if err := s.Publish(ctx, p2.ID); err != nil {
		t.Fatal(err)
	}
	s.WaitForJobs()
	app2, _ := s.AppBySlug(ctx, "shy")
	if app2.Installed {
		t.Fatal("auto-install off must not install")
	}
}

// TestGetStoreAppWithLaunchEvents is the regression test for the scan bug
// that silently broke installs once an app had beacon pings: MAX(created_at)
// comes back as text and must still produce a LastLaunchAt.
func TestGetStoreAppWithLaunchEvents(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	p, _ := s.CreateProject(ctx, "Pinged", filepath.Join(t.TempDir(), "pg"), true)
	id, _ := s.repo.UpsertStoreApp(ctx, p.ID, PublishDraft{AppName: "Pinged", Headline: "h", Description: "d"}, "", "pinged")
	if err := s.repo.RecordAppEvent(ctx, "pinged", "launch"); err != nil {
		t.Fatal(err)
	}
	app, err := s.GetStoreApp(ctx, id)
	if err != nil {
		t.Fatalf("GetStoreApp with launch events: %v", err)
	}
	if app.LaunchCount != 1 || app.LastLaunchAt == nil {
		t.Fatalf("liveness not populated: count=%d last=%v", app.LaunchCount, app.LastLaunchAt)
	}
}

func TestMakeRequiresASentence(t *testing.T) {
	s := newTestService(t)
	if _, err := s.Make(context.Background(), "  "); err == nil {
		t.Fatal("blank sentence must be rejected")
	}
}

func TestMakeOpensWhenAgentFinishes(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	s.SetBuilder(fakeBuilder{})
	dir := t.TempDir()
	p, err := s.CreateProject(ctx, "Sourdough", dir, true)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module sourdough\n\ngo 1.24\n")
	writeTestFile(t, filepath.Join(dir, "main.go"), "package main\nfunc main() {}\n")
	if err := s.repo.SaveDraft(ctx, PublishDraft{ProjectID: p.ID, AppName: "Sourdough", Headline: "h", Description: "track feedings", AutoInstall: true}); err != nil {
		t.Fatal(err)
	}
	s.makeByRequest.Store(int64(77), p.ID)
	s.RequestSettled(p.ID, 77, "completed")
	s.WaitForJobs()
	st, err := s.MakeStatus(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != "open" || !strings.HasPrefix(st.URL, "/run/") {
		t.Fatalf("status = %+v, want open at /run/<id>", st)
	}
	appID, err := s.repo.StoreAppIDForProject(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	app, err := s.GetStoreApp(ctx, appID)
	if err != nil {
		t.Fatal(err)
	}
	if app.PublicURL == "" || !tcpUp(app.PublicURL) {
		t.Fatalf("app not listening at %q", app.PublicURL)
	}
}
