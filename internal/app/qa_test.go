package app

import (
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"oozie"
	"oozie/internal/db"
	"oozie/internal/web/render"
)

// TestQAWebFactory is the live QA for this fork: pages, a real go build,
// a localhost start, and a stop. It does not touch /Users/kc/oozie.
func TestQAWebFactory(t *testing.T) {
	server := startQA(t)
	client := server.Client()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	t.Run("pages", func(t *testing.T) {
		for _, path := range []string{"/projects", "/projects/new", "/store", "/publishing/jobs", "/settings", "/wishes"} {
			body := qaGet(t, client, server.URL+path, 200)
			for _, banned := range []string{"/Applications", "Package.swift", "Install to /Applications", ".app bundle"} {
				if strings.Contains(body, banned) {
					t.Errorf("%s still mentions %q", path, banned)
				}
			}
		}
		res, err := client.Get(server.URL + "/store/apps/1/surgery")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != 404 {
			t.Fatalf("removed surgery route = %d\n%s", res.StatusCode, body)
		}
		if strings.Contains(string(body), "screenshot") {
			t.Fatalf("surgery page still rendered:\n%s", body)
		}
	})

	workdir := filepath.Join(t.TempDir(), "qa-sample")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeQAApp(t, workdir)

	t.Run("create", func(t *testing.T) {
		res := qaPost(t, client, server.URL+"/projects", url.Values{
			"name":                 {"QA Sample"},
			"project_path_display": {workdir},
			"trusted":              {"on"},
		})
		if res.StatusCode != 303 {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("create = %d\n%s", res.StatusCode, b)
		}
		loc := res.Header.Get("Location")
		if !strings.HasPrefix(loc, "/projects/") {
			t.Fatalf("create location = %q", loc)
		}
	})

	t.Run("publish-and-serve", func(t *testing.T) {
		res := qaPost(t, client, server.URL+"/projects/1/publish", url.Values{
			"app_name":     {"QA Sample"},
			"headline":     {"QA"},
			"description":  {"A throwaway page for the factory QA."},
			"auto_install": {"on"},
			"expires_days": {"0"},
		})
		if res.StatusCode != 200 {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("publish = %d\n%s", res.StatusCode, b)
		}
		res.Body.Close()

		deadline := time.Now().Add(2 * time.Minute)
		var jobs string
		for {
			jobs = qaGet(t, client, server.URL+"/fragments/publishing/jobs", 200)
			if strings.Contains(jobs, `class="badge succeeded"`) {
				break
			}
			if strings.Contains(jobs, `class="badge failed"`) || time.Now().After(deadline) {
				t.Fatalf("publish did not succeed:\n%s", jobs)
			}
			time.Sleep(300 * time.Millisecond)
		}

		store := qaGet(t, client, server.URL+"/store", 200)
		if !strings.Contains(store, "running") {
			t.Fatalf("store did not mark the app running:\n%s", store)
		}
		match := regexp.MustCompile(`http://127\.0\.0\.1:\d+`).FindString(store)
		if match == "" {
			t.Fatalf("store has no localhost URL:\n%s", store)
		}
		appBody := qaGet(t, client, match, 200)
		if !strings.Contains(appBody, "qa-ok") {
			t.Fatalf("published app body = %q", appBody)
		}
		improve := qaGet(t, client, server.URL+"/improve/qa-sample", 200)
		if !strings.Contains(improve, "QA Sample") {
			t.Fatal("improve page did not name the app")
		}

		stop := qaPost(t, client, server.URL+"/store/apps/1/uninstall", nil)
		if stop.StatusCode != 200 {
			b, _ := io.ReadAll(stop.Body)
			t.Fatalf("stop = %d\n%s", stop.StatusCode, b)
		}
		stop.Body.Close()
		if stillUp(match) {
			t.Fatalf("app still listening at %s after stop", match)
		}
	})
}

func startQA(t *testing.T) *httptest.Server {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "qa.db"))
	if err != nil {
		t.Fatal(err)
	}
	migrationsFS, err := fs.Sub(oozie.Assets, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RunMigrations(database, migrationsFS); err != nil {
		t.Fatal(err)
	}
	templatesFS, _ := fs.Sub(oozie.Assets, "templates")
	staticFS, _ := fs.Sub(oozie.Assets, "static")
	renderer, err := render.New(templatesFS, "qa")
	if err != nil {
		t.Fatal(err)
	}
	application := New(Config{Addr: "127.0.0.1:8090"}, database, renderer, staticFS)
	server := httptest.NewServer(application.Routes())
	t.Cleanup(func() {
		server.Close()
		application.Shutdown()
		database.Close()
	})
	return server
}

func writeQAApp(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module qasample\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := `package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = "127.0.0.1:" + os.Getenv("PORT")
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "qa-ok")
	})
	if err := http.ListenAndServe(addr, nil); err != nil {
		panic(err)
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func qaGet(t *testing.T, client *http.Client, raw string, want int) string {
	t.Helper()
	res, err := client.Get(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != want {
		t.Fatalf("GET %s = %d, want %d\n%s", raw, res.StatusCode, want, body)
	}
	return string(body)
}

func qaPost(t *testing.T, client *http.Client, raw string, form url.Values) *http.Response {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(http.MethodPost, raw, body)
	if err != nil {
		t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func stillUp(raw string) bool {
	host := strings.TrimPrefix(raw, "http://")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", host, 150*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		time.Sleep(50 * time.Millisecond)
	}
	return true
}
