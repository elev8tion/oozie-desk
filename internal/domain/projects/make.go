package projects

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"

	"oozie/internal/agent/pi"
	"oozie/internal/build"
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
	// Optional pending agent permission (untrusted projects).
	Permission *PermissionRequest
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
	return s.MakeWithModel(ctx, text, "")
}

// MakeWithModel builds from the desk. model, when set, becomes the desk preferred
// coding model and is used for this build (reviving hop memory for that pick).
func (s *Service) MakeWithModel(ctx context.Context, text, model string) (int64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, ErrValidation{"Describe the tool."}
	}
	if model = strings.TrimSpace(model); model != "" {
		if err := s.SetCodingModel(ctx, model); err != nil {
			return 0, err
		}
	}
	if _, err := s.modelForNewBuildCtx(ctx, ""); err != nil {
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
	// Pin the new session to the desk preferred model so hops start from the pick.
	if session, err := s.repo.GetSession(ctx, project.ID); err == nil {
		prefer := s.preferredSessionModel(ctx, "")
		if prefer != "" {
			_ = s.repo.SetSessionModel(ctx, session.ID, prefer)
		}
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
	if perm, err := s.repo.PendingPermission(ctx, projectID); err == nil && perm != nil {
		view.Permission = perm
		view.Line = "Waiting for permission: " + perm.PermissionName
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
	liveRequestID := requestID
	if v, ok := s.improveCurrent.Load(requestID); ok {
		liveRequestID = v.(int64)
	}
	switch imp.Status {
	case "done":
		view.Phase = "open"
		view.URL = fmt.Sprintf("/run/%d", app.ID)
		return view, nil
	case "failed":
		view.Phase = "failed"
		view.Error = "The fix did not land. Try another sentence."
		if status, msg, rerr := s.repo.RequestStatus(ctx, liveRequestID); rerr == nil && (status == "failed" || status == "cancelled") {
			if plain := plainPageError(msg); plain != "" {
				view.Error = plain
			}
		}
		return view, nil
	case "publishing":
		view.Line = "Starting the tool."
		return view, nil
	}
	if status, msg, err := s.repo.RequestStatus(ctx, liveRequestID); err == nil {
		if status == "failed" || status == "cancelled" {
			// A model hop may still be spinning up; only fail the wait screen
			// when improve itself is marked failed.
			if _, still := s.improveCurrent.Load(requestID); still && liveRequestID != requestID {
				view.Line = "Trying another model."
				return view, nil
			}
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
// On a credit/quota failure it retries once with the next signed-in model.
func (s *Service) settleMake(projectID, requestID int64, status string) {
	v, ok := s.makeByRequest.Load(requestID)
	if !ok || v.(int64) != projectID {
		return
	}
	ctx := context.Background()
	if status == "failed" {
		if s.retryMakeAfterCredit(ctx, projectID, requestID) {
			return
		}
		s.makeByRequest.Delete(requestID)
		return
	}
	if status != "completed" {
		return
	}
	if s.retryIncompleteScaffold(ctx, projectID, requestID, func(newID int64) {
		s.makeByRequest.Delete(requestID)
		s.trackFrontDoor(newID, projectID)
	}) {
		return
	}
	s.makeByRequest.Delete(requestID)
	s.makeCreditRetry.Delete(projectID)
	s.incompleteScaffold.Delete(projectID)
	if project, err := s.repo.GetProject(ctx, projectID); err == nil {
		if wd, werr := resolveWorkdir(project); werr == nil && !build.Buildable(wd) {
			s.noteIncompleteStop(ctx, projectID)
			return
		}
	}
	if err := s.Publish(ctx, projectID); err != nil {
		// Publish records its own job failure when the build starts. If it
		// cannot even start, the waiting screen still needs one sentence.
		if jobID, jerr := s.repo.CreateJob(ctx, projectID); jerr == nil {
			_ = s.repo.FinishJob(ctx, jobID, "failed", err.Error(), nil)
		}
	}
}

// noteIncompleteStop records a visible failure when the agent finished
// without Go source. This is not a model refusal, so the desk does not hop.
func (s *Service) noteIncompleteStop(ctx context.Context, projectID int64) {
	msg := "The model stopped before the tool had a Go source file. The desk did not switch models."
	if jobID, err := s.repo.CreateJob(ctx, projectID); err == nil {
		_ = s.repo.FinishJob(ctx, jobID, "failed", msg, nil)
	}
	log.Printf("project %d: incomplete scaffold — stopped, no model hop", projectID)
}

// retryIncompleteScaffold re-prompts once, on the same model, when the agent
// "completed" without a compileable Go app. A missing file is not a provider
// refusal, so this never hops and never marks the model dead.
func (s *Service) retryIncompleteScaffold(ctx context.Context, projectID, requestID int64, onRetry func(newID int64)) bool {
	project, err := s.repo.GetProject(ctx, projectID)
	if err != nil {
		return false
	}
	wd, err := resolveWorkdir(project)
	if err != nil {
		return false
	}
	if build.Buildable(wd) {
		return false
	}
	_, nudged := s.incompleteScaffold.Load(projectID)
	original, _ := s.repo.FirstBuildPrompt(ctx, projectID)
	if strings.TrimSpace(original) == "" {
		original, _ = s.repo.FirstUserMessage(ctx, requestID)
	}
	if !nudged {
		s.incompleteScaffold.Store(projectID, true)
		newID, err := s.sendAgentMessage(ctx, projectID, "build", incompleteScaffoldNudge(original))
		if err != nil || newID == 0 {
			s.incompleteScaffold.Delete(projectID)
			return false
		}
		onRetry(newID)
		log.Printf("project %d: incomplete scaffold after request %d — same-model nudge as request %d", projectID, requestID, newID)
		return true
	}
	return false
}

// maxMakeCreditRetries is how many times settleMake may hop to the next model
// after credit/quota refusals (openrouter → zai → xai, etc.).
const maxMakeCreditRetries = 4

// retryMakeAfterCredit starts another front-door build on the next model when
// the previous attempt died on credits/quota. Returns true if a retry started.
func (s *Service) retryMakeAfterCredit(ctx context.Context, projectID, requestID int64) bool {
	n := 0
	if v, ok := s.makeCreditRetry.Load(projectID); ok {
		n, _ = v.(int)
	}
	if n >= maxMakeCreditRetries {
		return false
	}
	_, errMsg, err := s.repo.RequestStatus(ctx, requestID)
	if err != nil {
		return false
	}
	if !pi.ModelRejected(errMsg) {
		return false
	}
	session, serr := s.repo.GetSession(ctx, projectID)
	if serr != nil {
		return false
	}
	next, err := s.modelForRetry(ctx, session.Model)
	if err != nil || !realHop(session.Model, next) {
		log.Printf("make project %d: refusal on %s — no other model, not hopping", projectID, session.Model)
		return false
	}
	if err := s.pinSessionModel(ctx, projectID, next); err != nil {
		return false
	}
	msg, err := s.repo.FirstUserMessage(ctx, requestID)
	if err != nil || strings.TrimSpace(msg) == "" {
		return false
	}
	s.makeByRequest.Delete(requestID)
	newID, err := s.sendAgentMessage(ctx, projectID, "build", msg)
	if err != nil || newID == 0 {
		// Count the attempt so we don't thrash; waiting screen shows the failure.
		s.makeCreditRetry.Store(projectID, n+1)
		return false
	}
	s.makeCreditRetry.Store(projectID, n+1)
	s.trackFrontDoor(newID, projectID)
	log.Printf("make project %d: refusal on %s request %d — switched to %s as request %d (attempt %d)", projectID, session.Model, requestID, next, newID, n+1)
	return true
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
