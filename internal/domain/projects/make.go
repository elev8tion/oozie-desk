package projects

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"oozie/internal/agent/pi"
)

// MakeView is what the front door shows while a sentence becomes a tool.
// Phase is "building", "open", or "failed".
type MakeView struct {
	ProjectID int64
	Name      string
	Phase     string
	URL       string
	Error     string
	Text      string
	RetryURL  string
	Line      string
}

// ImproveView is the Fix wait screen: rebuild in place, then reopen /run.
// Phase is "building", "open", or "failed".
type ImproveView struct {
	RequestID int64
	AppID     int64
	Name      string
	Slug      string
	Phase     string
	URL       string
	Error     string
	Note      string
	RetryURL  string
	Line      string
}

// Make is the front door: one sentence becomes a trusted project, an agent
// build, and — when that build finishes — a running localhost tool.
func (s *Service) Make(ctx context.Context, text string) (int64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, ErrValidation{"Describe the tool."}
	}
	if _, err := s.modelForNewBuild(""); err != nil {
		return 0, err
	}
	name := wishProjectName(text)
	if strings.HasPrefix(name, "Wish ") {
		name = "Tool " + name[len("Wish "):]
	}
	project, err := s.CreateProject(ctx, name, "", true)
	if err != nil {
		return 0, err
	}
	draft := PublishDraft{
		ProjectID:   project.ID,
		AppName:     name,
		Headline:    wishHeadline(text),
		Description: text,
		AutoInstall: true,
	}
	if err := s.repo.SaveDraft(ctx, draft); err != nil {
		_ = s.DeleteProject(ctx, project.ID, true)
		return 0, err
	}
	requestID, err := s.sendAgentMessage(ctx, project.ID, "build", pageBuildMessage(text))
	if err != nil {
		_ = s.DeleteProject(ctx, project.ID, true)
		return 0, err
	}
	s.trackFrontDoor(requestID, project.ID)
	return project.ID, nil
}

// trackFrontDoor registers a build so settleMake auto-publishes and the
// make-wait screen can open /run when the tool is listening.
func (s *Service) trackFrontDoor(requestID, projectID int64) {
	if requestID == 0 || projectID == 0 {
		return
	}
	s.makeByRequest.Store(requestID, projectID)
}

// SetupHint is a desk sentence when no signed-in model is ready. It does not
// probe the network — only credentials + catalog — so the desk stays fast.
func (s *Service) SetupHint() string {
	signed := pi.SignedProviders(authPath())
	if s.signedIn != nil {
		signed = s.signedIn()
	}
	if len(pi.CandidateModels(s.catalog, "", signed)) == 0 {
		return "No model API key is set. Add OPENROUTER_API_KEY (or another provider key) in the environment, or keep a key in ~/.pi/agent/auth.json, then come back and build."
	}
	return ""
}

// MakeStatus reports whether the tool is still being built, ready to open,
// or stopped with one sentence.
func (s *Service) MakeStatus(ctx context.Context, projectID int64) (MakeView, error) {
	project, err := s.repo.GetProject(ctx, projectID)
	if err != nil {
		return MakeView{}, err
	}
	view := MakeView{ProjectID: project.ID, Name: project.Name, Phase: "building"}
	if draft, err := s.repo.GetDraft(ctx, projectID); err == nil {
		view.Text = draft.Description
	}
	view.RetryURL = "/?text=" + url.QueryEscape(view.Text)

	if appID, err := s.repo.StoreAppIDForProject(ctx, projectID); err == nil && appID != 0 {
		if app, err := s.repo.GetStoreApp(ctx, appID); err == nil && appIsOurs(app) {
			view.Phase = "open"
			// Open inside the desk chrome so Back to desk is always available.
			view.URL = fmt.Sprintf("/run/%d", app.ID)
			return view, nil
		}
	}

	job, jobErr := s.repo.LatestJob(ctx, projectID)
	if jobErr != nil && !errors.Is(jobErr, sql.ErrNoRows) {
		return MakeView{}, jobErr
	}
	if jobErr == nil && job.Status == "failed" {
		view.Phase = "failed"
		view.Error = plainPageError(job.ErrorMessage)
		return view, nil
	}

	status, msg, err := s.repo.LatestRequest(ctx, projectID)
	if err != nil {
		return MakeView{}, err
	}
	if status == "failed" || status == "cancelled" {
		view.Phase = "failed"
		view.Error = plainPageError(msg)
		if view.Error == "" {
			view.Error = "The agent stopped before the tool was ready."
		}
		return view, nil
	}
	view.Line = "Starting."
	if jobErr == nil && (job.Status == "queued" || job.Status == "running") {
		view.Line = "Starting the tool."
		return view, nil
	}
	if role, toolStatus, content, err := s.repo.LatestActivity(ctx, projectID); err == nil {
		view.Line = buildProgress(role, toolStatus, content)
	}
	return view, nil
}

// ImproveStatus reports Fix progress for a filed improve request.
func (s *Service) ImproveStatus(ctx context.Context, requestID int64) (ImproveView, error) {
	imp, err := s.repo.ImproveByRequest(ctx, requestID)
	if err != nil {
		return ImproveView{}, err
	}
	if imp == nil {
		return ImproveView{}, sql.ErrNoRows
	}
	app, err := s.repo.GetStoreApp(ctx, imp.StoreAppID)
	if err != nil {
		return ImproveView{}, err
	}
	view := ImproveView{
		RequestID: requestID,
		AppID:     app.ID,
		Name:      app.Name,
		Slug:      app.BundleSlug,
		Note:      imp.Note,
		Phase:     "building",
		RetryURL:  "/improve/" + app.BundleSlug,
		Line:      "Starting the fix.",
	}
	switch imp.Status {
	case "done":
		view.Phase = "open"
		view.URL = fmt.Sprintf("/run/%d", app.ID)
		return view, nil
	case "failed":
		view.Phase = "failed"
		view.Error = "The fix did not land. Try another sentence."
		if status, msg, rerr := s.repo.RequestStatus(ctx, requestID); rerr == nil && (status == "failed" || status == "cancelled") {
			if plain := plainPageError(msg); plain != "" {
				view.Error = plain
			}
		}
		return view, nil
	case "publishing":
		view.Line = "Starting the tool."
		return view, nil
	}
	if status, msg, err := s.repo.RequestStatus(ctx, requestID); err == nil {
		if status == "failed" || status == "cancelled" {
			view.Phase = "failed"
			view.Error = plainPageError(msg)
			if view.Error == "" {
				view.Error = "The agent stopped before the fix was ready."
			}
			return view, nil
		}
	}
	if app.ProjectID != nil {
		if role, toolStatus, content, err := s.repo.LatestActivity(ctx, *app.ProjectID); err == nil {
			view.Line = buildProgress(role, toolStatus, content)
		}
	}
	return view, nil
}

// settleMake publishes a front-door build once the agent finishes. The job
// stays running until the tool is listening, so the waiting screen can open it.
func (s *Service) settleMake(projectID, requestID int64, status string) {
	v, ok := s.makeByRequest.LoadAndDelete(requestID)
	if !ok || v.(int64) != projectID || status != "completed" {
		return
	}
	if err := s.Publish(context.Background(), projectID); err != nil {
		// Publish records its own job failure when the build starts. If it
		// cannot even start, the waiting screen still needs one sentence.
		ctx := context.Background()
		if jobID, jerr := s.repo.CreateJob(ctx, projectID); jerr == nil {
			_ = s.repo.FinishJob(ctx, jobID, "failed", err.Error(), nil)
		}
	}
}

func pageBuildMessage(text string) string {
	return fmt.Sprintf(`Build one small local tool as a single web page for this request. Do not ask questions — pick sensible defaults.

Contract:
- Go module at the project root (go.mod and main.go). Prefer the standard library.
- Listen on the ADDR environment variable. If ADDR is empty, listen on 127.0.0.1:$PORT. Never hardcode a port.
- GET / returns HTML with status 200.
- If the tool stores anything the user enters, keep it under data/ (or $OOZIE_DATA_DIR). Never bake personal records into source. Each desk keeps its own data when the recipe is shared.
- Every page has a footer with a "Back to desk" link (target="_top") to the desk URL from the system prompt (or $OOZIE_DESK_URL). The user must always be able to return to the desk from the tool.
- Also put a footer link labeled "Fix" to the improve URL from the system prompt, when that URL is non-empty.
- No icon, no screenshot, no visual-review pass. The page itself is the preview.
- Verify with: go build -o /tmp/app .

Request:

%s`, text)
}

func buildProgress(role, status, content string) string {
	text := strings.ToLower(content)
	switch {
	case strings.Contains(text, "go build"), strings.Contains(text, "go test"):
		return "Checking the build."
	case strings.HasPrefix(text, "write"), strings.HasPrefix(text, "edit"), strings.Contains(text, "main.go"):
		return "Writing the tool."
	case role == "assistant" && strings.TrimSpace(content) != "":
		return "Writing the tool."
	case role == "tool" || strings.Contains(text, "bash") || status == "running":
		return "Running a check."
	default:
		return "Starting."
	}
}

func plainPageError(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	if strings.Contains(msg, "No API key") || strings.Contains(msg, "not signed in") {
		return "This model is not signed in."
	}
	if strings.Contains(strings.ToLower(msg), "credit") || strings.Contains(msg, "max_tokens") {
		return "This model needs more credits. Try another model or top up the provider."
	}
	if strings.Contains(strings.ToLower(msg), "not found") || strings.Contains(msg, "404") {
		return "No model answered."
	}
	if strings.Contains(msg, "no go.mod") || strings.Contains(msg, "without producing") {
		return "The agent finished without a tool to open."
	}
	if strings.Contains(msg, "not reachable") || strings.Contains(msg, "Couldn't start") {
		return "The tool built, but it did not start."
	}
	if i := strings.IndexByte(msg, '\n'); i > 0 {
		msg = msg[:i]
	}
	if r := []rune(msg); len(r) > 180 {
		msg = string(r[:180]) + "…"
	}
	return msg
}
