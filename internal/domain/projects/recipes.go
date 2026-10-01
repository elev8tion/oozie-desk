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
//
// Isolation contract: a recipe never carries runtime usage data. Export
// reads only agent prompts plus DESIGN.md and optional icon.png. It does
// not open data/, *.db, logs, or any other file the tool wrote while the
// author used it. The recipient rebuilds an empty tool on their desk.
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

// toolDataDirName is the only place generated tools may keep durable
// usage data. Share/remix/export paths skip it so one desk never receives
// another desk's records.
const toolDataDirName = "data"

// ExportRecipe packages a published app as a shareable recipe.
// Runtime databases and other usage files under the project workdir are
// never read or attached.
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
	prompts = recipePrompts(prompts)
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
			// Deliberately ignore workdir/data and any *.db — those are the
			// author's private use of the tool, not part of the recipe.
		}
	}
	return rec, nil
}

// recipePrompts keeps the genome (what to build) and drops empty lines.
// It does not scrape project files; usage data never enters this list
// unless a human typed it into a prompt themselves.
func recipePrompts(in []string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// ProposeRecipeFromLink validates a Chrome / App Store / Play link, reads
// the public listing, and stores a draft plan for Accept / Reject / Edit.
// No project is created and the build agent is not started until Accept or Edit.
func (s *Service) ProposeRecipeFromLink(ctx context.Context, rawURL string) (RecipeDraft, error) {
	kind, canonical, err := classifyStoreURL(rawURL)
	if err != nil {
		return RecipeDraft{}, err
	}
	listing, err := fetchStoreListing(ctx, kind, canonical)
	if err != nil {
		return RecipeDraft{}, err
	}
	// Second-line guard: if the fetch path ever returned a non-store kind, refuse.
	switch listing.Kind {
	case storeKindChrome, storeKindAppStore, storeKindPlay:
	default:
		return RecipeDraft{}, ErrValidation{storeLinkRejectMsg}
	}
	plan := synthesizePlan(listing)
	rec := recipeFromListing(listing, plan)
	body, err := json.Marshal(rec)
	if err != nil {
		return RecipeDraft{}, ErrValidation{"Couldn't prepare that recipe draft."}
	}
	d := RecipeDraft{
		SourceURL:        listing.URL,
		SourceKind:       listing.Kind,
		Name:             listing.Name,
		Headline:         listing.Headline,
		StoreDescription: listing.Description,
		Plan:             plan,
		RecipeJSON:       string(body),
		Status:           "pending",
	}
	id, err := s.repo.CreateRecipeDraft(ctx, d)
	if err != nil {
		return RecipeDraft{}, err
	}
	return s.repo.GetRecipeDraft(ctx, id)
}

// GetRecipeDraft returns a draft by id.
func (s *Service) GetRecipeDraft(ctx context.Context, id int64) (RecipeDraft, error) {
	return s.repo.GetRecipeDraft(ctx, id)
}

// AcceptRecipeDraft starts the build from the draft's recipe and marks it accepted.
func (s *Service) AcceptRecipeDraft(ctx context.Context, id int64) (Project, error) {
	d, err := s.repo.GetRecipeDraft(ctx, id)
	if err != nil {
		return Project{}, err
	}
	if d.Status != "pending" {
		return Project{}, ErrValidation{"That recipe draft is no longer waiting for a decision."}
	}
	project, err := s.importRecipeJSON(ctx, d.RecipeJSON)
	if project.ID != 0 {
		_ = s.repo.SettleRecipeDraft(ctx, id, "accepted", &project.ID)
	}
	return project, err
}

// RejectRecipeDraft discards a pending draft without building.
func (s *Service) RejectRecipeDraft(ctx context.Context, id int64) error {
	d, err := s.repo.GetRecipeDraft(ctx, id)
	if err != nil {
		return err
	}
	if d.Status != "pending" {
		return ErrValidation{"That recipe draft is no longer waiting for a decision."}
	}
	return s.repo.SettleRecipeDraft(ctx, id, "rejected", nil)
}

// EditRecipeDraft replaces the natural-language plan, adapts the recipe
// prompts to match, then starts the build.
func (s *Service) EditRecipeDraft(ctx context.Context, id int64, plan string) (Project, error) {
	plan = strings.TrimSpace(plan)
	if plan == "" {
		return Project{}, ErrValidation{"Edit the plan first, or Reject to discard it."}
	}
	d, err := s.repo.GetRecipeDraft(ctx, id)
	if err != nil {
		return Project{}, err
	}
	if d.Status != "pending" {
		return Project{}, ErrValidation{"That recipe draft is no longer waiting for a decision."}
	}
	var rec Recipe
	if err := json.Unmarshal([]byte(d.RecipeJSON), &rec); err != nil {
		return Project{}, ErrValidation{"That draft's recipe is damaged — Reject it and try a new link."}
	}
	rec = recipeFromEditedPlan(rec, plan)
	body, err := json.Marshal(rec)
	if err != nil {
		return Project{}, ErrValidation{"Couldn't adapt that recipe."}
	}
	if err := s.repo.UpdateRecipeDraftPlan(ctx, id, plan, string(body)); err != nil {
		return Project{}, err
	}
	project, err := s.importRecipeJSON(ctx, string(body))
	if project.ID != 0 {
		_ = s.repo.SettleRecipeDraft(ctx, id, "accepted", &project.ID)
	}
	return project, err
}

// ImportRecipe creates a project from a recipe and asks the agent to
// rebuild the app it describes.
func (s *Service) ImportRecipe(ctx context.Context, raw string) (Project, error) {
	return s.importRecipeJSON(ctx, raw)
}

func (s *Service) importRecipeJSON(ctx context.Context, raw string) (Project, error) {
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
	b.WriteString("\nSynthesize these into one coherent app (later prompts refine earlier ones — don't replay them literally if they conflict).")
	b.WriteString("\n\nData isolation: this is a fresh desk. Start with empty local storage. Put any durable records under a data/ directory in the project root (create it on first write). Do not invent or hardcode the original author's personal records, sample rows that look like real usage, or anything that could have come from their database. Each desk owns its own data/.")
	b.WriteString("\nVerify with 'go build -o /tmp/app .' and keep the server listening on $ADDR.")
	headline := strings.TrimSpace(rec.Headline)
	if headline == "" {
		headline = rec.Name
	}
	desc := strings.TrimSpace(rec.Description)
	if desc == "" && len(rec.Prompts) > 0 {
		desc = rec.Prompts[0]
	}
	draft := PublishDraft{
		ProjectID:   project.ID,
		AppName:     rec.Name,
		Headline:    headline,
		Description: desc,
		AutoInstall: true,
	}
	if err := s.repo.SaveDraft(ctx, draft); err != nil {
		return project, err
	}
	requestID, err := s.sendAgentMessage(ctx, project.ID, "build", b.String())
	if err != nil {
		// Leave a failed request so /make does not spin on "Starting." forever.
		s.recordFailedStart(ctx, project.ID, err.Error())
		return project, err
	}
	s.trackFrontDoor(requestID, project.ID)
	return project, nil
}
