package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"oozie"
	"oozie/internal/app"
	"oozie/internal/db"
	"oozie/internal/web/render"
)

func main() {
	cfg := app.LoadConfig()

	// Only auto-migrate the old repo-local database into the default
	// location; an explicit DATABASE_PATH means the caller chose a home.
	if os.Getenv("DATABASE_PATH") == "" {
		migrateLegacyDatabase(cfg.DatabasePath)
	}

	database, err := db.Open(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer database.Close()

	migrationsFS, err := fs.Sub(oozie.Assets, "migrations")
	if err != nil {
		log.Fatalf("migrations fs: %v", err)
	}
	if err := db.RunMigrations(database, migrationsFS); err != nil {
		log.Fatalf("run migrations: %v", err)
	}

	templatesFS, err := fs.Sub(oozie.Assets, "templates")
	if err != nil {
		log.Fatalf("templates fs: %v", err)
	}
	staticFS, err := fs.Sub(oozie.Assets, "static")
	if err != nil {
		log.Fatalf("static fs: %v", err)
	}
	renderer, err := render.New(templatesFS, render.CSSStamp(staticFS, "css/app.css"))
	if err != nil {
		log.Fatalf("parse templates: %v", err)
	}

	application := app.New(cfg, database, renderer, staticFS)

	server := &http.Server{Addr: cfg.Addr, Handler: application.Routes()}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Under the .app shell, stdin is a pipe held by the parent process.
	// EOF means the parent died (even via force-quit), so shut down rather
	// than run orphaned.
	if os.Getenv("OOZIE_PARENT_WATCH") == "1" {
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		}()
	}

	go func() {
		log.Printf("oozie listening on http://%s (database: %s)", cfg.Addr, cfg.DatabasePath)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	// Launched from the .app bundle, open the UI in the default browser.
	if os.Getenv("OOZIE_OPEN_BROWSER") == "1" {
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = exec.Command("open", "http://"+cfg.Addr).Run()
		}()
	}

	<-ctx.Done()
	log.Printf("shutting down…")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	application.Shutdown()
}

// migrateLegacyDatabase copies a repo-local data/app.db (the old default
// location) to the new Application Support path, once, so existing
// projects and sessions carry over.
func migrateLegacyDatabase(target string) {
	legacy := filepath.Join("data", "app.db")
	if _, err := os.Stat(target); err == nil {
		return
	}
	src, err := os.Open(legacy)
	if err != nil {
		return
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return
	}
	dst, err := os.Create(target)
	if err != nil {
		return
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err == nil {
		log.Printf("migrated existing database from %s to %s", legacy, target)
	}
}
