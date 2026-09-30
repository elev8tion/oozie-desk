package projects

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"oozie/internal/web/render"
)

type Handlers struct {
	service  *Service
	renderer *render.Renderer
}

func NewHandlers(service *Service, renderer *render.Renderer) *Handlers {
	return &Handlers{service: service, renderer: renderer}
}

// page renders a full page in the base layout with the user's saved theme
// and style applied.
func (h *Handlers) page(w http.ResponseWriter, r *http.Request, title, content string, data map[string]any) {
	s, _ := h.service.GetSettings(r.Context())
	h.renderer.HTML(w, 200, "layouts/base", render.ViewData{Title: title, Content: content, Theme: s.Appearance, Style: s.StyleProfile, Data: data})
}

// errorPage renders a styled error page in the layout.
func (h *Handlers) errorPage(w http.ResponseWriter, r *http.Request, status int, message string) {
	s, _ := h.service.GetSettings(r.Context())
	h.renderer.HTML(w, status, "layouts/base", render.ViewData{Title: "Error · oozie", Content: "pages/error-content", Theme: s.Appearance, Style: s.StyleProfile, Data: map[string]any{"Code": status, "Message": message}})
}

// NotFound is the desk page for a path that matches no route.
func (h *Handlers) NotFound(w http.ResponseWriter, r *http.Request) {
	h.errorPage(w, r, http.StatusNotFound, "That page is not on this desk.")
}

func (h *Handlers) Home(w http.ResponseWriter, r *http.Request) {
	h.page(w, r, "oozie", "pages/make/index-content", map[string]any{"Text": r.URL.Query().Get("text")})
}

func (h *Handlers) Make(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	text := r.FormValue("text")
	id, err := h.service.Make(r.Context(), text)
	if err != nil {
		http.Redirect(w, r, "/?text="+url.QueryEscape(text)+"&err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/make/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (h *Handlers) MakeWait(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	st, err := h.service.MakeStatus(r.Context(), id)
	if err != nil {
		h.errorPage(w, r, 404, "That page is not being built.")
		return
	}
	if st.Phase == "open" {
		http.Redirect(w, r, st.URL, http.StatusSeeOther)
		return
	}
	h.page(w, r, st.Name+" · oozie", "pages/make/wait-content", map[string]any{"Status": st})
}

// RunApp starts the tool if needed and shows it inside the desk chrome so
// Back to desk is always one click away, even when the tool itself forgot a link.
func (h *Handlers) RunApp(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	url, err := h.service.OpenApp(r.Context(), id)
	if err != nil {
		h.errorPage(w, r, 500, err.Error())
		return
	}
	app, err := h.service.GetStoreApp(r.Context(), id)
	if err != nil {
		h.errorPage(w, r, 404, "That tool is not on this desk.")
		return
	}
	h.page(w, r, app.Name+" · oozie", "pages/run/show-content", map[string]any{"App": app, "URL": url})
}

func (h *Handlers) MakeStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	st, err := h.service.MakeStatus(r.Context(), id)
	if err != nil {
		http.Error(w, "That page is not being built.", 404)
		return
	}
	if st.Phase == "open" {
		w.Header().Set("HX-Redirect", st.URL)
	}
	h.renderer.HTML(w, 200, "partials/make/status", render.ViewData{Data: map[string]any{"Status": st}})
}

func (h *Handlers) Onboarding(w http.ResponseWriter, r *http.Request) {
	h.page(w, r, "Welcome to oozie", "pages/projects/onboarding-content", nil)
}
func (h *Handlers) Projects(w http.ResponseWriter, r *http.Request) {
	ps, err := h.service.ListProjects(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("filter"))
	if err != nil {
		h.errorPage(w, r, 500, "Couldn't load projects.")
		return
	}
	h.page(w, r, "Projects · oozie", "pages/projects/index-content", map[string]any{"Projects": ps, "Q": r.URL.Query().Get("q"), "Filter": r.URL.Query().Get("filter"), "Insights": h.service.Insights(r.Context())})
}
func (h *Handlers) ProjectsList(w http.ResponseWriter, r *http.Request) {
	ps, err := h.service.ListProjects(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("filter"))
	if err != nil {
		http.Error(w, "projects", 500)
		return
	}
	h.renderer.HTML(w, 200, "partials/projects/list", render.ViewData{Data: map[string]any{"Projects": ps}})
}
func (h *Handlers) NewProject(w http.ResponseWriter, r *http.Request) {
	h.page(w, r, "New Project · oozie", "pages/projects/new-content", nil)
}
func (h *Handlers) CreateProject(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.errorPage(w, r, 400, "That form couldn't be read.")
		return
	}
	p, err := h.service.CreateProject(r.Context(), r.FormValue("name"), r.FormValue("project_path_display"), r.FormValue("trusted") == "on")
	if err != nil {
		h.renderer.HTML(w, 422, "partials/projects/flash", render.ViewData{Flash: err.Error()})
		return
	}
	if isHTMX(r) {
		ps, _ := h.service.ListProjects(r.Context(), "", "")
		h.renderer.HTML(w, 200, "partials/projects/list", render.ViewData{Flash: "Project created.", Data: map[string]any{"Projects": ps}})
		return
	}
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(p.ID, 10), 303)
}
func (h *Handlers) ShowProject(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	p, err := h.service.GetProject(r.Context(), id)
	if err != nil {
		h.errorPage(w, r, 404, "Project not found.")
		return
	}
	h.page(w, r, p.Name+" · oozie", "pages/projects/show-content", map[string]any{"Project": p})
}

// SetTrusted toggles a project between trusted (agent runs unattended)
// and confirm mode (every mutating tool call needs approval).
func (h *Handlers) SetTrusted(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	_ = r.ParseForm()
	if err := h.service.SetTrusted(r.Context(), id, r.FormValue("trusted") == "on"); err != nil {
		h.errorPage(w, r, 422, err.Error())
		return
	}
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(id, 10), 303)
}

func (h *Handlers) ArchiveProject(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	_ = h.service.ArchiveProject(r.Context(), id)
	if isHTMX(r) {
		ps, _ := h.service.ListProjects(r.Context(), "", "")
		h.renderer.HTML(w, 200, "partials/projects/list", render.ViewData{Flash: "Project archived.", Data: map[string]any{"Projects": ps}})
		return
	}
	http.Redirect(w, r, "/projects", 303)
}

// DeleteProject permanently removes a project (and optionally its files).
func (h *Handlers) DeleteProject(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	_ = r.ParseForm()
	err := h.service.DeleteProject(r.Context(), id, r.FormValue("delete_files") == "on")
	if err != nil {
		if isHTMX(r) {
			ps, _ := h.service.ListProjects(r.Context(), "", "")
			h.renderer.HTML(w, 200, "partials/projects/list", render.ViewData{Err: err.Error(), Data: map[string]any{"Projects": ps}})
			return
		}
		h.errorPage(w, r, 422, err.Error())
		return
	}
	if isHTMX(r) {
		ps, _ := h.service.ListProjects(r.Context(), "", "")
		h.renderer.HTML(w, 200, "partials/projects/list", render.ViewData{Flash: "Project deleted permanently.", Data: map[string]any{"Projects": ps}})
		return
	}
	http.Redirect(w, r, "/projects", 303)
}

func (h *Handlers) Agent(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	page, err := h.service.AgentPage(r.Context(), id)
	if err != nil {
		h.errorPage(w, r, 404, "Project not found.")
		return
	}
	h.page(w, r, "Agent · "+page.Project.Name, "pages/agents/show-content", map[string]any{"Agent": page})
}
func (h *Handlers) AgentRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	_ = r.ParseForm()
	err := h.service.SendAgentMessage(r.Context(), id, r.FormValue("mode"), r.FormValue("message"))
	page, _ := h.service.AgentPage(r.Context(), id)
	if err != nil {
		page.Error = err.Error()
	}
	h.renderer.HTML(w, 200, "partials/agents/live", render.ViewData{Data: map[string]any{"Agent": page}})
}
func (h *Handlers) AgentTimeline(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	page, err := h.service.AgentPage(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h.renderer.HTML(w, 200, "partials/agents/live", render.ViewData{Data: map[string]any{"Agent": page}})
}
func (h *Handlers) SelectAgentModel(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	_ = r.ParseForm()
	err := h.service.SelectModel(r.Context(), id, r.FormValue("model"))
	page, _ := h.service.AgentPage(r.Context(), id)
	flash := "Model set to " + page.Model + "."
	if err != nil {
		flash = err.Error()
	}
	h.renderer.HTML(w, 200, "partials/agents/form", render.ViewData{Flash: flash, Data: map[string]any{"Agent": page}})
}
func (h *Handlers) CancelAgentRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	rid, ok := h.pathID(w, r, "requestID")
	if !ok {
		return
	}
	_ = h.service.CancelRequest(r.Context(), id, rid)
	page, _ := h.service.AgentPage(r.Context(), id)
	h.renderer.HTML(w, 200, "partials/agents/live", render.ViewData{Flash: "Request cancelled.", Data: map[string]any{"Agent": page}})
}
func (h *Handlers) AnswerQuestion(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	qid, ok := h.pathID(w, r, "toolUseID")
	if !ok {
		return
	}
	_ = r.ParseForm()
	err := h.service.AnswerQuestion(r.Context(), qid, r.FormValue("answer"))
	page, _ := h.service.AgentPage(r.Context(), id)
	flash := "Answer sent to pi."
	if err != nil {
		flash = err.Error()
	}
	h.renderer.HTML(w, 200, "partials/agents/pending", render.ViewData{Flash: flash, Data: map[string]any{"Agent": page}})
}
func (h *Handlers) DismissQuestion(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	qid, ok := h.pathID(w, r, "toolUseID")
	if !ok {
		return
	}
	_ = h.service.DismissQuestion(r.Context(), qid)
	page, _ := h.service.AgentPage(r.Context(), id)
	h.renderer.HTML(w, 200, "partials/agents/pending", render.ViewData{Flash: "Question dismissed.", Data: map[string]any{"Agent": page}})
}
func (h *Handlers) Permission(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	rid, ok := h.pathID(w, r, "requestID")
	if !ok {
		return
	}
	_ = r.ParseForm()
	err := h.service.ResolvePermission(r.Context(), rid, r.FormValue("decision") != "deny")
	page, _ := h.service.AgentPage(r.Context(), id)
	flash := "Permission decision sent to pi."
	if err != nil {
		flash = err.Error()
	}
	h.renderer.HTML(w, 200, "partials/agents/pending", render.ViewData{Flash: flash, Data: map[string]any{"Agent": page}})
}
func (h *Handlers) Feedback(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	_ = r.ParseForm()
	err := h.service.SaveFeedback(r.Context(), id, r.FormValue("feedback_type"), r.FormValue("reason"), r.FormValue("additional_feedback"))
	if err != nil {
		h.renderer.HTML(w, 422, "partials/projects/flash", render.ViewData{Flash: err.Error()})
		return
	}
	h.renderer.HTML(w, 200, "partials/projects/flash", render.ViewData{Flash: "Feedback sent."})
}

// ImprovePage is the fix-me page every published app links from its footer.
func (h *Handlers) ImprovePage(w http.ResponseWriter, r *http.Request) {
	app, err := h.service.AppBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		h.errorPage(w, r, 404, "No tool on this desk matches this link. Build it first.")
		return
	}
	h.page(w, r, "Improve "+app.Name, "pages/improve/show-content", map[string]any{"App": app})
}

func (h *Handlers) ImproveSubmit(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	app, err := h.service.AppBySlug(r.Context(), slug)
	if err != nil {
		h.errorPage(w, r, 404, "No tool on this desk matches this link.")
		return
	}
	_ = r.ParseForm()
	if err := h.service.FileImprovement(r.Context(), slug, r.FormValue("text")); err != nil {
		h.page(w, r, "Improve "+app.Name, "pages/improve/show-content", map[string]any{"App": app, "Error": err.Error(), "Text": r.FormValue("text")})
		return
	}
	h.page(w, r, "Improve "+app.Name, "pages/improve/show-content", map[string]any{"App": app, "Sent": true})
}

// Beacon records an optional launch ping from a published app. Always 204.
func (h *Handlers) Beacon(w http.ResponseWriter, r *http.Request) {
	h.service.RecordLaunch(r.Context(), r.PathValue("slug"))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) Store(w http.ResponseWriter, r *http.Request) {
	apps, err := h.service.ListStoreApps(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("filter"))
	if err != nil {
		h.errorPage(w, r, 500, "Couldn't load the tools on this desk.")
		return
	}
	h.page(w, r, "Tools · oozie", "pages/store/index-content", map[string]any{"Apps": apps})
}
func (h *Handlers) StoreResults(w http.ResponseWriter, r *http.Request) {
	apps, _ := h.service.ListStoreApps(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("filter"))
	h.renderer.HTML(w, 200, "partials/store/list", render.ViewData{Data: map[string]any{"Apps": apps}})
}
func (h *Handlers) StoreApp(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	app, err := h.service.GetStoreApp(r.Context(), id)
	if err != nil {
		h.errorPage(w, r, 404, "That tool is not on this desk.")
		return
	}
	h.page(w, r, app.Name+" · oozie", "pages/store/show-content", map[string]any{"App": app})
}
func (h *Handlers) InstallApp(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	err := h.service.InstallApp(r.Context(), id)
	app, _ := h.service.GetStoreApp(r.Context(), id)
	flash, errMsg := "Running at "+app.PublicURL+".", ""
	if err != nil {
		flash, errMsg = "", err.Error()
	}
	h.renderer.HTML(w, 200, "partials/store/row", render.ViewData{Flash: flash, Err: errMsg, Data: map[string]any{"App": app}})
}
func (h *Handlers) UninstallApp(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	err := h.service.UninstallApp(r.Context(), id)
	flash, errMsg := "Stopped. It stays on this desk.", ""
	if err != nil {
		flash, errMsg = "", err.Error()
	}
	app, _ := h.service.GetStoreApp(r.Context(), id)
	h.renderer.HTML(w, 200, "partials/store/row", render.ViewData{Flash: flash, Err: errMsg, Data: map[string]any{"App": app}})
}
func (h *Handlers) RemoveStoreApp(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	err := h.service.RemoveStoreApp(r.Context(), id)
	if err != nil {
		app, _ := h.service.GetStoreApp(r.Context(), id)
		h.renderer.HTML(w, 200, "partials/store/row", render.ViewData{Err: err.Error(), Data: map[string]any{"App": app}})
		return
	}
	h.renderer.HTML(w, 200, "partials/store/flash", render.ViewData{Flash: "Removed from this desk. Build the project again to bring it back."})
}

// ExportRecipe downloads an app as a shareable recipe file — prompts, not
// binaries.
func (h *Handlers) ExportRecipe(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	rec, err := h.service.ExportRecipe(r.Context(), id)
	if err != nil {
		h.errorPage(w, r, 422, err.Error())
		return
	}
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		h.errorPage(w, r, 500, "Couldn't encode the recipe.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(rec.Name, `"`, "")+`.oozie-recipe.json"`)
	_, _ = w.Write(body)
}

// ImportRecipePage is the Recipes hub: paste a store link (or advanced JSON),
// and export any published app as a recipe file.
func (h *Handlers) ImportRecipePage(w http.ResponseWriter, r *http.Request) {
	apps, _ := h.service.ListStoreApps(r.Context(), "", "")
	h.page(w, r, "Recipes · oozie", "pages/recipes/import-content", map[string]any{
		"Apps":  apps,
		"Error": r.URL.Query().Get("err"),
		"Link":  r.URL.Query().Get("link"),
		"Flash": r.URL.Query().Get("flash"),
	})
}

// ProposeRecipeFromLink reads a Chrome / App Store / Play listing into a draft.
func (h *Handlers) ProposeRecipeFromLink(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	link := r.FormValue("link")
	draft, err := h.service.ProposeRecipeFromLink(r.Context(), link)
	if err != nil {
		http.Redirect(w, r, "/recipes?link="+url.QueryEscape(link)+"&err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/recipes/drafts/"+strconv.FormatInt(draft.ID, 10), http.StatusSeeOther)
}

// RecipeDraftPage shows the natural-language plan with Accept / Reject / Edit.
func (h *Handlers) RecipeDraftPage(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	draft, err := h.service.GetRecipeDraft(r.Context(), id)
	if err != nil {
		h.errorPage(w, r, 404, err.Error())
		return
	}
	if draft.Status != "pending" {
		http.Redirect(w, r, "/recipes?flash="+url.QueryEscape("That draft was already "+draft.Status+"."), http.StatusSeeOther)
		return
	}
	h.page(w, r, draft.Name+" · recipe", "pages/recipes/draft-content", map[string]any{
		"Draft":      draft,
		"SourceKind": sourceKindLabel(draft.SourceKind),
		"Error":      r.URL.Query().Get("err"),
	})
}

func (h *Handlers) AcceptRecipeDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	project, err := h.service.AcceptRecipeDraft(r.Context(), id)
	if project.ID != 0 {
		// Project exists even when pi failed to start — land on the agent page.
		http.Redirect(w, r, "/projects/"+strconv.FormatInt(project.ID, 10)+"/agent", http.StatusSeeOther)
		return
	}
	if err != nil {
		http.Redirect(w, r, "/recipes/drafts/"+strconv.FormatInt(id, 10)+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/recipes", http.StatusSeeOther)
}

func (h *Handlers) RejectRecipeDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	if err := h.service.RejectRecipeDraft(r.Context(), id); err != nil {
		http.Redirect(w, r, "/recipes?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/recipes?flash="+url.QueryEscape("Draft discarded. Paste another store link when you're ready."), http.StatusSeeOther)
}

func (h *Handlers) EditRecipeDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	_ = r.ParseForm()
	project, err := h.service.EditRecipeDraft(r.Context(), id, r.FormValue("plan"))
	if project.ID != 0 {
		http.Redirect(w, r, "/projects/"+strconv.FormatInt(project.ID, 10)+"/agent", http.StatusSeeOther)
		return
	}
	if err != nil {
		http.Redirect(w, r, "/recipes/drafts/"+strconv.FormatInt(id, 10)+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/recipes", http.StatusSeeOther)
}

func (h *Handlers) ImportRecipe(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	raw := r.FormValue("recipe")
	if raw == "" {
		if file, _, err := r.FormFile("recipe_file"); err == nil {
			defer file.Close()
			body, _ := io.ReadAll(io.LimitReader(file, 8<<20))
			raw = string(body)
		}
	}
	project, err := h.service.ImportRecipe(r.Context(), raw)
	if err != nil {
		apps, _ := h.service.ListStoreApps(r.Context(), "", "")
		h.page(w, r, "Recipes · oozie", "pages/recipes/import-content", map[string]any{"Error": err.Error(), "Recipe": raw, "Apps": apps})
		return
	}
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(project.ID, 10)+"/agent", http.StatusSeeOther)
}

// RemixApp forks a store app into a new project with a mutation prompt
// and drops the user onto the new project's agent page.
func (h *Handlers) RemixApp(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	_ = r.ParseForm()
	remix, err := h.service.RemixApp(r.Context(), id, r.FormValue("mutation"))
	if err != nil {
		app, _ := h.service.GetStoreApp(r.Context(), id)
		h.page(w, r, app.Name+" · oozie", "pages/store/show-content", map[string]any{"App": app, "Error": err.Error()})
		return
	}
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(remix.ID, 10)+"/agent", http.StatusSeeOther)
}

func (h *Handlers) OpenApp(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	runURL := "/run/" + strconv.FormatInt(id, 10)
	url, err := h.service.OpenApp(r.Context(), id)
	flash, errMsg := "Running at "+url+".", ""
	if err != nil {
		flash, errMsg = "", err.Error()
	} else if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", runURL)
	}
	app, _ := h.service.GetStoreApp(r.Context(), id)
	h.renderer.HTML(w, 200, "partials/store/row", render.ViewData{Flash: flash, Err: errMsg, Data: map[string]any{"App": app}})
}
func (h *Handlers) InstalledApps(w http.ResponseWriter, r *http.Request) {
	apps, _ := h.service.InstalledApps(r.Context())
	h.page(w, r, "Running · oozie", "pages/store/installed-content", map[string]any{"Apps": apps})
}

func (h *Handlers) PublishingJobs(w http.ResponseWriter, r *http.Request) {
	jobs, _ := h.service.ListJobs(r.Context(), r.URL.Query().Get("status"))
	h.page(w, r, "Jobs · oozie", "pages/publishing/index-content", map[string]any{"Jobs": jobs, "Active": jobsActive(jobs)})
}
func (h *Handlers) PublishingJobsList(w http.ResponseWriter, r *http.Request) {
	jobs, _ := h.service.ListJobs(r.Context(), r.URL.Query().Get("status"))
	h.renderer.HTML(w, 200, "partials/publishing/list", render.ViewData{Data: map[string]any{"Jobs": jobs, "Active": jobsActive(jobs)}})
}
func jobsActive(jobs []PublishingJob) bool {
	for _, j := range jobs {
		if j.Status == "queued" || j.Status == "running" {
			return true
		}
	}
	return false
}
func (h *Handlers) PublishPage(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	p, _ := h.service.GetProject(r.Context(), id)
	d, _ := h.service.GetDraft(r.Context(), id)
	h.page(w, r, "Build · "+p.Name, "pages/publishing/show-content", map[string]any{"Project": p, "Draft": d})
}
func (h *Handlers) SaveDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	_ = r.ParseForm()
	d := draftFromForm(id, r)
	err := h.service.SaveDraft(r.Context(), d)
	if err != nil {
		h.renderer.HTML(w, 422, "partials/publishing/form", render.ViewData{Flash: err.Error(), Data: map[string]any{"Draft": d}})
		return
	}
	h.renderer.HTML(w, 200, "partials/publishing/form", render.ViewData{Flash: "Draft saved.", Data: map[string]any{"Draft": d}})
}
func (h *Handlers) Publish(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	_ = r.ParseForm()
	// The Publish button lives inside the draft form: save the current
	// form values first so one click does the whole thing. Blank fields
	// get defaults in SaveDraft, and skipping the save would drop the
	// non-text fields (like the disposable-app lifetime), so save
	// whenever the form was actually submitted with the request.
	if r.Form.Has("app_name") || r.Form.Has("expires_days") {
		d := draftFromForm(id, r)
		if err := h.service.SaveDraft(r.Context(), d); err != nil {
			h.renderJobs(w, r, "", err.Error())
			return
		}
	}
	err := h.service.Publish(r.Context(), id)
	if err != nil {
		h.renderJobs(w, r, "", err.Error())
		return
	}
	h.renderJobs(w, r, "Building on this desk…", "")
}

func draftFromForm(projectID int64, r *http.Request) PublishDraft {
	days, _ := strconv.Atoi(r.FormValue("expires_days"))
	if days < 0 {
		days = 0
	}
	return PublishDraft{ProjectID: projectID, AppName: r.FormValue("app_name"), Headline: r.FormValue("headline"), Description: r.FormValue("description"), Changelog: r.FormValue("changelog"), PublishTarget: r.FormValue("publish_target"), Visibility: r.FormValue("visibility"), ScreenshotManifest: r.FormValue("screenshot_manifest"), ExpiresDays: days, AutoInstall: r.FormValue("auto_install") == "on"}
}

func (h *Handlers) renderJobs(w http.ResponseWriter, r *http.Request, flash, errMsg string) {
	jobs, _ := h.service.ListJobs(r.Context(), "")
	h.renderer.HTML(w, 200, "partials/publishing/list", render.ViewData{Flash: flash, Err: errMsg, Data: map[string]any{"Jobs": jobs, "Active": jobsActive(jobs)}})
}

func (h *Handlers) Wishes(w http.ResponseWriter, r *http.Request) {
	h.wishesPage(w, r, "", "")
}
func (h *Handlers) wishesPage(w http.ResponseWriter, r *http.Request, flash, errMsg string) {
	wishes, _ := h.service.ListWishes(r.Context())
	s, _ := h.service.GetSettings(r.Context())
	h.renderer.HTML(w, 200, "layouts/base", render.ViewData{Title: "Wishes · oozie", Content: "pages/wishes/index-content", Flash: flash, Err: errMsg, Theme: s.Appearance, Style: s.StyleProfile, Data: map[string]any{"Wishes": wishes}})
}
func (h *Handlers) AddWish(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if err := h.service.AddWish(r.Context(), r.FormValue("text")); err != nil {
		h.wishesPage(w, r, "", err.Error())
		return
	}
	h.wishesPage(w, r, "Wish added — the fairy will find it tonight, or build it now.", "")
}
func (h *Handlers) DeleteWish(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	_ = h.service.DeleteWish(r.Context(), id)
	h.wishesPage(w, r, "Wish deleted.", "")
}
func (h *Handlers) BuildWish(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r, "id")
	if !ok {
		return
	}
	if err := h.service.BuildWish(r.Context(), id); err != nil {
		h.wishesPage(w, r, "", err.Error())
		return
	}
	h.wishesPage(w, r, "Granting the wish — the agent is building it now.", "")
}

func (h *Handlers) Settings(w http.ResponseWriter, r *http.Request) {
	s, _ := h.service.GetSettings(r.Context())
	h.page(w, r, "Settings · oozie", "pages/settings/index-content", map[string]any{"Settings": s, "Taste": h.service.LoadTaste()})
}

// SaveTaste persists the user's design voice; it flows into every project
// the next time the agent works there.
func (h *Handlers) SaveTaste(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	flash, errMsg := "Taste saved — every project inherits it on the agent's next run.", ""
	if err := h.service.SaveTaste(r.FormValue("taste")); err != nil {
		flash, errMsg = "", err.Error()
	}
	h.renderer.HTML(w, 200, "partials/settings/taste", render.ViewData{Flash: flash, Err: errMsg, Data: map[string]any{"Taste": h.service.LoadTaste()}})
}
func (h *Handlers) SaveSettings(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	hour, _ := strconv.Atoi(r.FormValue("fairy_hour"))
	if hour < 0 || hour > 23 {
		hour = 2
	}
	s := Settings{Appearance: r.FormValue("appearance"), StyleProfile: r.FormValue("style_profile"), FairyEnabled: r.FormValue("fairy_enabled") == "on", FairyHour: hour}
	_ = h.service.SaveSettings(r.Context(), s)
	h.renderer.HTML(w, 200, "partials/settings/form", render.ViewData{Flash: "Settings saved.", Data: map[string]any{"Settings": s}})
}

func (h *Handlers) pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id < 1 {
		h.errorPage(w, r, 400, "That ID isn't valid.")
		return 0, false
	}
	return id, true
}
func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }
