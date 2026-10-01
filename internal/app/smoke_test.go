package app

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"oozie-desk"
	"oozie-desk/internal/db"
	"oozie-desk/internal/web/render"
)

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "smoke.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	migrationsFS, _ := fs.Sub(oozie.Assets, "migrations")
	if err := db.RunMigrations(database, migrationsFS); err != nil {
		t.Fatal(err)
	}
	templatesFS, _ := fs.Sub(oozie.Assets, "templates")
	staticFS, _ := fs.Sub(oozie.Assets, "static")
	renderer, err := render.New(templatesFS, "test")
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{}, database, renderer, staticFS).Routes()
}

func TestPagesRender(t *testing.T) {
	handler := newTestServer(t)
	cases := []struct {
		path string
		want int
	}{
		{"/", 200},
		{"/projects", 200},
		{"/projects/new", 200},
		{"/store", 200},
		{"/publishing/jobs", 200},
		{"/people", 200},
		{"/settings", 200},
		{"/static/js/htmx.min.js", 200},
		{"/static/css/app.css", 200},
		{"/projects/999", 404},
		{"/store/apps/999", 404},
		{"/no-such-page", 404},
		{"/projects/banana", 400},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", c.path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("GET %s = %d, want %d", c.path, rec.Code, c.want)
		}
		if c.path == "/" && !strings.Contains(rec.Body.String(), "What should this tool do?") {
			t.Errorf("desk did not render: %s", rec.Body.String())
		}
		if c.path == "/people" && !strings.Contains(rec.Body.String(), "Invite only") {
			t.Errorf("people page did not render: %s", rec.Body.String())
		}
		if c.want != 200 && !strings.Contains(rec.Body.String(), "Back to Projects") {
			t.Errorf("GET %s error page is not styled", c.path)
		}
	}
}

func TestDeadPathsRedirectHome(t *testing.T) {
	handler := newTestServer(t)
	for _, path := range []string{"/onboarding", "/installed-apps"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 303 {
			t.Errorf("GET %s = %d, want 303", path, rec.Code)
			continue
		}
		loc := rec.Header().Get("Location")
		if path == "/onboarding" && loc != "/" {
			t.Errorf("onboarding → %s, want /", loc)
		}
		if path == "/installed-apps" && !strings.HasPrefix(loc, "/store") {
			t.Errorf("installed-apps → %s, want /store…", loc)
		}
	}
	req := httptest.NewRequest("GET", "/projects/1/feedback", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == 200 && strings.Contains(rec.Body.String(), "Send feedback") {
		t.Fatal("feedback route still live")
	}
}

func TestStoreInstalledFilterRenders(t *testing.T) {
	handler := newTestServer(t)
	req := httptest.NewRequest("GET", "/store?filter=installed", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("store filter = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="installed"`) || !strings.Contains(body, "selected") {
		t.Fatalf("installed filter not selected:\n%s", body)
	}
}

func TestImproveWaitMissingIsNotFound(t *testing.T) {
	handler := newTestServer(t)
	req := httptest.NewRequest("GET", "/fix/999", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("missing fix wait = %d, want 404", rec.Code)
	}
}

func TestMakeUnsignedModelStopsAtTheFrontDoor(t *testing.T) {
	auth := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(auth, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OOZIE_AUTH_PATH", auth)
	handler := newTestServer(t)

	form := strings.NewReader("text=A+page+that+says+hello")
	req := httptest.NewRequest("POST", "/make", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 303 {
		t.Fatalf("make = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "err=This+model+is+not+signed+in.") {
		t.Fatalf("location = %s", loc)
	}

	req = httptest.NewRequest("GET", loc, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "This model is not signed in.") {
		t.Fatalf("desk did not show the sentence: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "OPENROUTER_API_KEY") {
		t.Fatalf("desk did not show setup help: %s", rec.Body.String())
	}
	req = httptest.NewRequest("GET", "/projects", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "Page Says Hello") {
		t.Fatalf("a project was created:\n%s", rec.Body.String())
	}
}

func TestCreateProjectFlow(t *testing.T) {
	handler := newTestServer(t)
	form := strings.NewReader("name=Smoke+Test&project_path_display=~/Projects/smoke-test&trusted=on")
	req := httptest.NewRequest("POST", "/projects", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 303 {
		t.Fatalf("create project = %d, want 303", rec.Code)
	}

	req = httptest.NewRequest("GET", rec.Header().Get("Location"), nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Smoke Test") {
		t.Fatalf("project page = %d, contains name: %v", rec.Code, strings.Contains(rec.Body.String(), "Smoke Test"))
	}
}

// TestPublishBlankFormKeepsLifetime is a regression test: a one-click
// publish with an untouched (blank-text) form must still persist the
// non-text draft fields — the disposable-app lifetime in particular.
func TestPublishBlankFormKeepsLifetime(t *testing.T) {
	application := newTestApp(t)
	handler := application.Routes()

	form := url.Values{"name": {"Blanky"}, "project_path_display": {filepath.Join(t.TempDir(), "blanky")}}
	req := httptest.NewRequest("POST", "/projects", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 303 {
		t.Fatalf("create project: %d", rec.Code)
	}

	pub := url.Values{"app_name": {""}, "headline": {""}, "description": {""}, "changelog": {""}, "publish_target": {""}, "visibility": {""}, "screenshot_manifest": {""}, "expires_days": {"7"}}
	req = httptest.NewRequest("POST", "/projects/1/publish", strings.NewReader(pub.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("publish: %d", rec.Code)
	}
	application.service.WaitForJobs()

	d, err := application.service.GetDraft(req.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if d.ExpiresDays != 7 {
		t.Errorf("expires_days = %d, want 7 (blank-form publish dropped the lifetime)", d.ExpiresDays)
	}
	if d.AppName != "Blanky" || d.Headline == "" || d.Description == "" {
		t.Errorf("defaults not applied: %+v", d)
	}
}

func newTestApp(t *testing.T) *App {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "smoke.db"))
	if err != nil {
		t.Fatal(err)
	}
	migrationsFS, _ := fs.Sub(oozie.Assets, "migrations")
	if err := db.RunMigrations(database, migrationsFS); err != nil {
		t.Fatal(err)
	}
	templatesFS, _ := fs.Sub(oozie.Assets, "templates")
	staticFS, _ := fs.Sub(oozie.Assets, "static")
	renderer, err := render.New(templatesFS, "test")
	if err != nil {
		t.Fatal(err)
	}
	a := New(Config{}, database, renderer, staticFS)
	t.Cleanup(func() { a.service.WaitForJobs(); a.Shutdown(); database.Close() })
	return a
}

func TestCancelBackupExportAndKeySaveAreHandled(t *testing.T) {
	auth := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("OOZIE_AUTH_PATH", auth)
	application := newTestApp(t)
	handler := application.Routes()
	ctx := t.Context()

	dir := t.TempDir()
	p, err := application.service.CreateProject(ctx, "Cancel Me", dir, true)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/make/"+strconv.FormatInt(p.ID, 10)+"/cancel", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("cancel = 500: %s", rec.Body.String())
	}
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "/make/") {
		t.Fatalf("cancel = %d loc=%s", rec.Code, rec.Header().Get("Location"))
	}

	req = httptest.NewRequest(http.MethodPost, "/settings/backup", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("backup = 500: %s", rec.Body.String())
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Disposition"), "oozie-desk-backup.db") || rec.Body.Len() == 0 {
		t.Fatalf("backup = %d disp=%s bytes=%d body=%s", rec.Code, rec.Header().Get("Content-Disposition"), rec.Body.Len(), rec.Body.String())
	}

	form := strings.NewReader("provider=openrouter&key=sk-test-desk")
	req = httptest.NewRequest(http.MethodPost, "/settings/key", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("key save = 500: %s", rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.Contains(loc, "flash=") {
		t.Fatalf("key save = %d loc=%s", rec.Code, loc)
	}
	saved, err := os.ReadFile(auth)
	if err != nil || !strings.Contains(string(saved), "sk-test-desk") {
		t.Fatalf("key file: %v %s", err, saved)
	}
	form = strings.NewReader("provider=nope&key=x")
	req = httptest.NewRequest(http.MethodPost, "/settings/key", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusInternalServerError || rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("bad key = %d loc=%s", rec.Code, rec.Header().Get("Location"))
	}

	req = httptest.NewRequest(http.MethodGet, "/store/apps/999/data", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("missing export = 500: %s", rec.Body.String())
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing export = %d, want 422", rec.Code)
	}

	if _, err := application.database.Exec(`INSERT INTO store_apps (project_id, name, headline, description, visibility, bundle_slug) VALUES (?, 'Cancel Me', 'headline', 'description', 'unlisted', 'cancel-me')`, p.ID); err != nil {
		t.Fatal(err)
	}
	var appID int64
	if err := application.database.QueryRow(`SELECT id FROM store_apps WHERE project_id=?`, p.ID).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "row.txt"), []byte("saved"), 0o644); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/store/apps/"+strconv.FormatInt(appID, 10)+"/data", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("export = 500: %s", rec.Body.String())
	}
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/zip" || !strings.HasPrefix(rec.Body.String(), "PK") {
		t.Fatalf("export = %d type=%s body=%q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String()[:min(80, rec.Body.Len())])
	}
}
