package projects

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A Recipe is an app shared as intent instead of a binary: the prompts
// that grew it, its metadata, its design standard, and its icon. Another
// oozie rebuilds it locally — adapted to that machine and that user.
type Recipe struct {
	Kind        string    `json:"kind"` // recipeKind
	Name        string    `json:"name"`
	Headline    string    `json:"headline,omitempty"`
	Description string    `json:"description,omitempty"`
	Prompts     []string  `json:"prompts"`
	Design      string    `json:"design,omitempty"`   // DESIGN.md contents
	IconPNG     string    `json:"icon_png,omitempty"` // base64
	ExportedAt  time.Time `json:"exported_at"`
}

const recipeKind = "oozie-recipe/v1"

// ExportRecipe packages a published app as a shareable recipe.
func (s *Service) ExportRecipe(ctx context.Context, appID int64) (Recipe, error) {
	app, err := s.repo.GetStoreApp(ctx, appID)
	if err != nil {
		return Recipe{}, err
	}
	if app.ProjectID == nil {
		return Recipe{}, ErrValidation{"This app has no linked project — nothing to export."}
	}
	prompts, err := s.repo.UserPrompts(ctx, *app.ProjectID)
	if err != nil {
		return Recipe{}, err
	}
	if len(prompts) == 0 {
		return Recipe{}, ErrValidation{"This project has no agent history — a recipe would be empty."}
	}
	rec := Recipe{Kind: recipeKind, Name: app.Name, Headline: app.Headline, Description: app.Description, Prompts: prompts, ExportedAt: time.Now().UTC()}
	if project, err := s.repo.GetProject(ctx, *app.ProjectID); err == nil {
		if workdir, err := projectWorkdir(project); err == nil {
			if body, err := os.ReadFile(filepath.Join(workdir, "DESIGN.md")); err == nil {
				rec.Design = string(body)
			}
			if icon, err := os.ReadFile(filepath.Join(workdir, "icon.png")); err == nil && len(icon) < 4<<20 {
				rec.IconPNG = base64.StdEncoding.EncodeToString(icon)
			}
		}
	}
	return rec, nil
}

// ImportRecipe creates a project from a recipe and asks the agent to
// rebuild the app it describes.
func (s *Service) ImportRecipe(ctx context.Context, raw string) (Project, error) {
	var rec Recipe
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &rec); err != nil {
		msg := "That doesn't parse as a recipe: " + err.Error()
		// The classic corruption: an editor auto-replaced straight quotes
		// with curly ones while the user tweaked a field by hand.
		if strings.ContainsAny(raw, "“”") {
			msg += ` — the text contains curly “smart quotes”; your editor likely auto-replaced a straight " while editing. Fix those and retry.`
		}
		return Project{}, ErrValidation{msg}
	}
	if rec.Kind != recipeKind {
		return Project{}, ErrValidation{"Unsupported recipe kind — expected " + recipeKind + "."}
	}
	if strings.TrimSpace(rec.Name) == "" || len(rec.Prompts) == 0 {
		return Project{}, ErrValidation{"A recipe needs at least a name and one prompt."}
	}
	project, err := s.CreateProject(ctx, rec.Name, "", false)
	if err != nil {
		return Project{}, err
	}
	workdir, err := projectWorkdir(project)
	if err != nil {
		return project, ErrValidation{"Project directory unavailable: " + err.Error()}
	}
	if rec.Design != "" {
		_ = os.WriteFile(filepath.Join(workdir, "DESIGN.md"), []byte(rec.Design), 0o644)
	}
	if rec.IconPNG != "" {
		if icon, err := base64.StdEncoding.DecodeString(rec.IconPNG); err == nil {
			_ = os.WriteFile(filepath.Join(workdir, "icon.png"), icon, 0o644)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Rebuild this app from its recipe. It was grown elsewhere through the prompts below; recreate it here as a working local web app.\n\nApp: %s", rec.Name)
	if rec.Headline != "" {
		fmt.Fprintf(&b, " — %s", rec.Headline)
	}
	if rec.Description != "" {
		fmt.Fprintf(&b, "\n\nDescription: %s", rec.Description)
	}
	b.WriteString("\n\nThe prompts that shaped it, in order:\n")
	for i, p := range rec.Prompts {
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, p)
	}
	b.WriteString("\nSynthesize these into one coherent app (later prompts refine earlier ones — don't replay them literally if they conflict). Verify with 'go build -o /tmp/app .' and keep the server listening on $ADDR.")
	if _, err := s.sendAgentMessage(ctx, project.ID, "build", b.String()); err != nil {
		return project, err
	}
	return project, nil
}
