package app

import (
	"os"
	"path/filepath"

	"oozie-desk/internal/supportpath"
)

type Config struct {
	Addr         string
	DatabasePath string
}

func LoadConfig() Config {
	return Config{
		Addr:         env("ADDR", "127.0.0.1:8090"),
		DatabasePath: env("DATABASE_PATH", defaultDatabasePath()),
	}
}

// defaultDatabasePath keeps the database in the standard macOS location so
// the app works no matter where the binary lives.
func defaultDatabasePath() string {
	dir := supportpath.Dir()
	if dir == "" {
		return "data/app.db"
	}
	return filepath.Join(dir, "app.db")
}

func env(key string, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}
