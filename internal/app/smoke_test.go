package app

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"oozie"
	"oozie/internal/db"
	"oozie/internal/web/render"
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
		{"/installed-apps", 200},
		{"/publishing/jobs", 200},
		{"/settings", 200},
		{"/onboarding", 200},
		{"/static/js/htmx.min.js", 200},
		{"/static/css/app.css", 200},
		{"/projects/999", 404},
		{"/store/apps/999", 404},
		{"/projects/banana", 400},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", c.path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("GET %s = %d, want %d", c.path, rec.Code, c.want)
		}
		if c.want != 200 && !strings.Contains(rec.Body.String(), "Back to Projects") {
			t.Errorf("GET %s error page is not styled", c.path)
		}
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
