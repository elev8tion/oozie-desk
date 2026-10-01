package supportpath

import (
	"os"
	"path/filepath"
)

const (
	Folder       = "Oozie-Desk"
	LegacyFolder = "oozie-web"
)

// Dir is the Application Support folder for this desk.
// OOZIE_DATA_ROOT wins. Otherwise prefer Oozie-Desk, then the older oozie-web
// folder so an existing database is not orphaned.
func Dir() string {
	if v := os.Getenv("OOZIE_DATA_ROOT"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	neu := filepath.Join(home, "Library", "Application Support", Folder)
	legacy := filepath.Join(home, "Library", "Application Support", LegacyFolder)
	if exists(neu) {
		return neu
	}
	if exists(legacy) {
		return legacy
	}
	return neu
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
