package projects

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"oozie-desk/internal/supportpath"
)

// TASTE.md is the user's evolving design voice — the genome every new app
// inherits. It lives beside the database, is editable in Settings, is
// copied fresh into every project workdir, and accumulates signals (remix
// mutations, improvement requests) the user can promote into rules.

const defaultTaste = `# TASTE.md — your design voice

Rules here override DESIGN.md's generic choices. The agent reads this
before building anything. Write in plain language:

- (add your rules — "always dark-mode first", "dense tables over cards",
  "no onboarding screens, ever")

## Signals

Raw evidence of your taste, appended automatically by Oozie Desk. Promote the
patterns you see into rules above, delete the noise.
`

// tasteDirOverride redirects the taste file (tests point it at a temp dir
// so they never touch the real one).
var tasteDirOverride string

func tastePath() (string, error) {
	if tasteDirOverride != "" {
		return filepath.Join(tasteDirOverride, "TASTE.md"), nil
	}
	dir := supportpath.Dir()
	if dir == "" {
		return "", os.ErrNotExist
	}
	return filepath.Join(dir, "TASTE.md"), nil
}

// LoadTaste returns the taste file, creating the default on first touch.
func (s *Service) LoadTaste() string {
	path, err := tastePath()
	if err != nil {
		return defaultTaste
	}
	body, err := os.ReadFile(path)
	if err != nil {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		_ = os.WriteFile(path, []byte(defaultTaste), 0o644)
		return defaultTaste
	}
	return string(body)
}

func (s *Service) SaveTaste(body string) error {
	if strings.TrimSpace(body) == "" {
		return ErrValidation{"Taste can evolve, but not into nothing — keep at least a heading."}
	}
	path, err := tastePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

// appendTasteSignal records evidence of taste (an improvement request, a
// remix mutation) under the Signals section.
func (s *Service) appendTasteSignal(kind, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if len(text) > 200 {
		text = text[:197] + "…"
	}
	body := s.LoadTaste()
	entry := fmt.Sprintf("- %s [%s] %s\n", time.Now().Format("2006-01-02"), kind, strings.ReplaceAll(text, "\n", " "))
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	_ = s.SaveTaste(body + entry)
}

// materializeTaste writes the current taste into a project workdir so the
// agent always sees the latest version (unlike seeds, this refreshes).
func (s *Service) materializeTaste(workdir string) {
	_ = os.WriteFile(filepath.Join(workdir, "TASTE.md"), []byte(s.LoadTaste()), 0o644)
}
