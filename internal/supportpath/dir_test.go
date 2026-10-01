package supportpath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDirPrefersNewFolderThenLegacy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OOZIE_DATA_ROOT", "")

	support := filepath.Join(home, "Library", "Application Support")
	if err := os.MkdirAll(support, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := Dir(); got != filepath.Join(support, Folder) {
		t.Fatalf("empty home: Dir()=%s want new folder", got)
	}

	legacy := filepath.Join(support, LegacyFolder)
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Dir(); got != legacy {
		t.Fatalf("legacy only: Dir()=%s want %s", got, legacy)
	}

	neu := filepath.Join(support, Folder)
	if err := os.MkdirAll(neu, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Dir(); got != neu {
		t.Fatalf("both exist: Dir()=%s want %s", got, neu)
	}
}

func TestDirHonorsDataRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OOZIE_DATA_ROOT", root)
	if got := Dir(); got != root {
		t.Fatalf("Dir()=%s want OOZIE_DATA_ROOT %s", got, root)
	}
}
