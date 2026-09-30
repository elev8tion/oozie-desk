package projects

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// MakeView is what the front door shows while a sentence becomes a page.
// Phase is "building", "open", or "failed".
type MakeView struct {
	ProjectID int64
	Name      string
	Phase     string
	URL       string
	Error     string
	Text      string
	RetryURL  string
}

// Make is the front door: one sentence becomes a trusted project, an agent
// build, and — when that build finishes — a running localhost page.
func (s *Service) Make(ctx context.Context, text string) (int64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, ErrValidation{"Describe the page."}
	}
	name := wishProjectName(text)
	if strings.HasPrefix(name, "Wish ") {
		name = "Page " + name[len("Wish "):]
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
	s.makeByRequest.Store(requestID, project.ID)
	return project.ID, nil
}

// MakeStatus reports whether the page is still being built, ready to open,
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
			view.URL = app.PublicURL
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
			view.Error = "The agent stopped before the page was ready."
		}
		return view, nil
	}
	return view, nil
}

// settleMake publishes a front-door build once the agent finishes. The job
// stays running until the page is listening, so the waiting screen can open it.
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
- Put a footer link labeled "Fix" to the improve URL from the system prompt, when that URL is non-empty.
- No icon, no screenshot, no visual-review pass. The page itself is the preview.
- Verify with: go build -o /tmp/app .

Request:

%s`, text)
}

func plainPageError(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	if strings.Contains(msg, "no go.mod") || strings.Contains(msg, "without producing") {
		return "The agent finished without a page to open."
	}
	if strings.Contains(msg, "not reachable") || strings.Contains(msg, "Couldn't start") {
		return "The page built, but it did not start."
	}
	if i := strings.IndexByte(msg, '\n'); i > 0 {
		msg = msg[:i]
	}
	if r := []rune(msg); len(r) > 180 {
		msg = string(r[:180]) + "…"
	}
	return msg
}
