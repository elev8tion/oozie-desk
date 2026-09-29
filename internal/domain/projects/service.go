package projects

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"oozie/internal/agent/pi"
	"oozie/internal/build"
)

// seedsFS is materialized into every project workdir: DESIGN.md (the
// visual standard for local web apps). Files are only written if missing,
// so per-project edits stick.
//
//go:embed all:seeds
var seedsFS embed.FS

type Service struct {
	repo    *Repo
	agent   *pi.Manager
	catalog pi.Catalog
	builder build.AppBuilder
	jobs    sync.WaitGroup
	baseURL string   // oozie's own address, used in improve links and beacon URLs
	procs   sync.Map // store app id -> *exec.Cmd for servers this process started

	// wishByRequest maps in-flight agent requests to the wish that spawned
	// them (in-memory: a restart mid-build fails the wish honestly at the
	// next startup sweep). makeByRequest is the same map for the front door.
	wishByRequest sync.Map
	makeByRequest sync.Map
}

func NewService(repo *Repo) *Service {
	return &Service{repo: repo, builder: build.GoBuilder{}, baseURL: "http://127.0.0.1:8090"}
}

// SetBaseURL records the address published apps use to reach oozie
// (improve links, optional launch pings).
func (s *Service) SetBaseURL(u string) {
	if u != "" {
		s.baseURL = strings.TrimSuffix(u, "/")
	}
}

// SetBuilder swaps the app builder (tests use a fake).
func (s *Service) SetBuilder(b build.AppBuilder) { s.builder = b }

// WaitForJobs blocks until all in-flight publishing jobs settle.
func (s *Service) WaitForJobs() { s.jobs.Wait() }

// StartBackground launches oozie's clocks: the TTL reaper that
// self-destructs disposable apps. Loops exit when ctx is cancelled.
func (s *Service) StartBackground(ctx context.Context) {
	go s.reapLoop(ctx)
	go s.fairyLoop(ctx)
}

func (s *Service) reapLoop(ctx context.Context) {
	s.reapExpiredApps(ctx)
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reapExpiredApps(ctx)
		}
	}
}

// reapExpiredApps uninstalls and delists disposable apps whose TTL has
// passed. Projects are untouched: republishing resurrects the app.
func (s *Service) reapExpiredApps(ctx context.Context) {
	ids, err := s.repo.ExpiredAppIDs(ctx)
	if err != nil {
		log.Printf("reap expired apps: %v", err)
		return
	}
	for _, id := range ids {
		app, err := s.repo.GetStoreApp(ctx, id)
		if err != nil {
			continue
		}
		if err := s.RemoveStoreApp(ctx, id); err != nil {
			log.Printf("reap %q: %v", app.Name, err)
			continue
		}
		log.Printf("disposable app %q reached its expiry and was removed", app.Name)
	}
}

// RecoverOrphanedJobs fails jobs stranded by a previous process. Run once
// at startup, before the server accepts requests.
func (s *Service) RecoverOrphanedJobs(ctx context.Context) {
	if n, err := s.repo.SweepOrphanedJobs(ctx); err != nil {
		log.Printf("sweep orphaned jobs: %v", err)
	} else if n > 0 {
		log.Printf("marked %d orphaned publishing job(s) failed", n)
	}
	if n, err := s.repo.SweepStaleAgentRequests(ctx); err != nil {
		log.Printf("sweep stale agent requests: %v", err)
	} else if n > 0 {
		log.Printf("marked %d orphaned agent request(s) failed", n)
	}
	if err := s.repo.SweepStaleWishes(ctx); err != nil {
		log.Printf("sweep stale wishes: %v", err)
	}
	if err := s.repo.SweepStalePrompts(ctx); err != nil {
		log.Printf("sweep stale prompts: %v", err)
	}
}

// SetAgent wires the pi RPC manager after construction (the manager's
// event sink is this service, so the two reference each other).
func (s *Service) SetAgent(agent *pi.Manager, catalog pi.Catalog) {
	s.agent = agent
	s.catalog = catalog
}

func (s *Service) Dashboard(ctx context.Context) (Dashboard, error) {
	ps, err := s.repo.ListProjects(ctx, "", "active")
	if err != nil {
		return Dashboard{}, err
	}
	apps, _ := s.repo.ListStoreApps(ctx, "", "featured")
	jobs, _ := s.repo.ListJobs(ctx, "")
	return Dashboard{Projects: ps, StoreApps: apps, Jobs: jobs}, nil
}
func (s *Service) ListProjects(ctx context.Context, q, filter string) ([]Project, error) {
	return s.repo.ListProjects(ctx, strings.TrimSpace(q), filter)
}
func (s *Service) GetProject(ctx context.Context, id int64) (Project, error) {
	return s.repo.GetProject(ctx, id)
}
func (s *Service) CreateProject(ctx context.Context, name, path string, trusted bool) (Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Project{}, ErrValidation{"Project name is required."}
	}
	if path == "" {
		path = "~/Projects/" + strings.ToLower(strings.ReplaceAll(name, " ", "-"))
	}
	return s.repo.CreateProject(ctx, name, path, trusted)
}
func (s *Service) ArchiveProject(ctx context.Context, id int64) error {
	return s.repo.ArchiveProject(ctx, id)
}

// SetTrusted flips a project's trust flag. Trust is applied when the pi
// process launches (--approve vs the approval extension), so any running
// process is stopped; the next message relaunches it with the new mode
// against the same pi session.
func (s *Service) SetTrusted(ctx context.Context, id int64, trusted bool) error {
	if _, err := s.repo.GetProject(ctx, id); err != nil {
		return err
	}
	if err := s.repo.SetTrusted(ctx, id, trusted); err != nil {
		return err
	}
	if s.agent != nil {
		s.agent.StopProject(id)
	}
	return nil
}

// DeleteProject permanently removes a project: its pi process is stopped,
// its store listing (and installed copy) removed, and every DB trace
// cascades away. With deleteFiles, the working directory is deleted too —
// guarded so only real project dirs under the home folder are touched.
func (s *Service) DeleteProject(ctx context.Context, id int64, deleteFiles bool) error {
	project, err := s.repo.GetProject(ctx, id)
	if err != nil {
		return err
	}
	if s.agent != nil {
		s.agent.StopProject(id)
	}
	if appID, err := s.repo.StoreAppIDForProject(ctx, id); err == nil && appID != 0 {
		if err := s.RemoveStoreApp(ctx, appID); err != nil {
			return ErrValidation{"Couldn't remove the published app first: " + err.Error()}
		}
	}
	if err := s.repo.DeleteProject(ctx, id); err != nil {
		return err
	}
	if deleteFiles {
		dir, err := resolveWorkdir(project)
		if err == nil {
			if err := safeDeleteProjectDir(dir); err != nil {
				return ErrValidation{"Project deleted, but its files were kept: " + err.Error()}
			}
		}
	}
	return nil
}

// safeDeleteProjectDir refuses to delete anything that isn't a plain
// directory strictly inside the user's home folder — the guard between
// "delete my experiment" and "delete my home directory".
func safeDeleteProjectDir(dir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	clean := filepath.Clean(dir)
	if clean == home || !strings.HasPrefix(clean, home+string(filepath.Separator)) {
		return fmt.Errorf("%s is outside your home folder — delete it manually", clean)
	}
	// Refuse first-level dirs like ~/Documents; projects live at least two
	// levels deep (~/Projects/<name>).
	if filepath.Dir(clean) == home {
		return fmt.Errorf("%s is a top-level folder — delete it manually", clean)
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return nil // already gone: fine
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", clean)
	}
	return os.RemoveAll(clean)
}
func (s *Service) AgentPage(ctx context.Context, projectID int64) (AgentPage, error) {
	p, err := s.repo.GetProject(ctx, projectID)
	if err != nil {
		return AgentPage{}, err
	}
	session, err := s.repo.GetSession(ctx, projectID)
	if err != nil {
		return AgentPage{}, err
	}
	msgs, _ := s.repo.ListMessages(ctx, session.ID)
	reqs, _ := s.repo.ListRequests(ctx, session.ID)
	q, _ := s.repo.PendingQuestion(ctx, projectID)
	perm, _ := s.repo.PendingPermission(ctx, projectID)
	model := session.Model
	if model == "" {
		model = s.catalog.DefaultModel
	}
	streaming := len(reqs) > 0 && (reqs[0].Status == "streaming" || reqs[0].Status == "waiting")
	var stats *pi.SessionStats
	if s.agent != nil {
		stats = s.agent.Stats(projectID)
	}
	return AgentPage{Project: p, Session: session, Requests: reqs, Messages: msgs, Question: q, Permission: perm, Mode: "build", Models: s.models(), Model: model, Streaming: streaming, Stats: stats}, nil
}

func (s *Service) models() []ModelOption {
	out := make([]ModelOption, 0, len(s.catalog.Models))
	for _, m := range s.catalog.Models {
		out = append(out, ModelOption{Provider: m.Provider, ID: m.ID, Full: m.Full})
	}
	return out
}

func (s *Service) SendAgentMessage(ctx context.Context, projectID int64, mode, message string) error {
	_, err := s.sendAgentMessage(ctx, projectID, mode, message)
	return err
}

// sendAgentMessage files an agent request and returns its ID so callers
// (the improve loop, the fairy) can watch for it to settle.
func (s *Service) sendAgentMessage(ctx context.Context, projectID int64, mode, message string) (int64, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return 0, ErrValidation{"Message is required."}
	}
	if s.agent == nil {
		return 0, ErrValidation{"The pi agent is not configured."}
	}
	project, err := s.repo.GetProject(ctx, projectID)
	if err != nil {
		return 0, err
	}
	session, err := s.repo.GetSession(ctx, projectID)
	if err != nil {
		return 0, err
	}
	if mode != "plan" {
		mode = "build"
	}
	model := session.Model
	if model == "" {
		model = s.catalog.DefaultModel
	}
	if session.PiSessionID == "" {
		session.PiSessionID = newPiSessionID(projectID)
		if err := s.repo.SetPiSessionID(ctx, session.ID, session.PiSessionID); err != nil {
			return 0, err
		}
	}
	workdir, err := projectWorkdir(project)
	if err != nil {
		return 0, ErrValidation{"Project directory unavailable: " + err.Error()}
	}
	s.materializeTaste(workdir)
	requestID, err := s.repo.CreateAgentRequest(ctx, session.ID, mode, message)
	if err != nil {
		return 0, err
	}
	opts := pi.StartOptions{
		ProjectID:    projectID,
		Workdir:      workdir,
		Model:        model,
		PiSessionID:  session.PiSessionID,
		SystemPrompt: oozieSystemPrompt(project, workdir, s.improveURL(ctx, project)),
		Trusted:      project.Trusted,
	}
	if err := s.agent.Prompt(opts, requestID, wrapModeMessage(mode, message)); err != nil {
		_ = s.repo.InsertMessage(ctx, requestID, "system", "error", "Failed to start pi: "+err.Error())
		_ = s.repo.CompleteRequest(ctx, requestID, "failed")
		return 0, ErrValidation{"Could not reach the pi agent: " + err.Error()}
	}
	return requestID, nil
}

// improveURL is the wormhole every published app links back to. Published
// apps use their real slug; unpublished ones get the predicted slug from
// the project name (the improve endpoint is slug-addressed either way).
func (s *Service) improveURL(ctx context.Context, p Project) string {
	slug, err := s.repo.StoreAppSlugForProject(ctx, p.ID)
	if err != nil || slug == "" {
		slug = build.Slug(p.Name)
	}
	return s.baseURL + "/improve/" + slug
}

func (s *Service) SelectModel(ctx context.Context, projectID int64, model string) error {
	found := false
	for _, m := range s.catalog.Models {
		if m.Full == model {
			found = true
			break
		}
	}
	if !found {
		return ErrValidation{"Unknown model: " + model}
	}
	session, err := s.repo.GetSession(ctx, projectID)
	if err != nil {
		return err
	}
	if err := s.repo.SetSessionModel(ctx, session.ID, model); err != nil {
		return err
	}
	if s.agent != nil {
		if err := s.agent.SetModel(projectID, model); err != nil {
			return ErrValidation{"Saved, but the running agent rejected the model: " + err.Error()}
		}
	}
	return nil
}

func (s *Service) CancelRequest(ctx context.Context, projectID, id int64) error {
	if s.agent != nil {
		_ = s.agent.Abort(projectID)
	}
	return s.repo.CancelRequest(ctx, id)
}
func (s *Service) AnswerQuestion(ctx context.Context, id int64, answer string) error {
	q, err := s.repo.GetQuestion(ctx, id)
	if err != nil {
		return err
	}
	if q.RPCID != "" && s.agent != nil {
		if err := s.agent.RespondValue(q.ProjectID, q.RPCID, answer); err != nil {
			// The asking process is gone (crash/quit). Clear the stale
			// prompt instead of wedging the panel forever.
			_ = s.repo.ResolveQuestion(ctx, id, "expired")
			return ErrValidation{"That question came from an agent run that has ended — cleared it. Send your message again."}
		}
	}
	return s.repo.ResolveQuestion(ctx, id, "answered")
}
func (s *Service) DismissQuestion(ctx context.Context, id int64) error {
	q, err := s.repo.GetQuestion(ctx, id)
	if err != nil {
		return err
	}
	if q.RPCID != "" && s.agent != nil {
		_ = s.agent.RespondCancel(q.ProjectID, q.RPCID)
	}
	return s.repo.ResolveQuestion(ctx, id, "dismissed")
}
func (s *Service) ResolvePermission(ctx context.Context, id int64, approved bool) error {
	p, err := s.repo.GetPermission(ctx, id)
	if err != nil {
		return err
	}
	if p.RPCID != "" && s.agent != nil {
		if err := s.agent.RespondConfirm(p.ProjectID, p.RPCID, approved); err != nil {
			// The asking process is gone (crash/quit). Clear the stale
			// prompt instead of wedging the panel forever.
			_ = s.repo.ResolvePermission(ctx, id, "expired")
			return ErrValidation{"That permission request came from an agent run that has ended — cleared it. Send your message again."}
		}
	}
	if approved {
		return s.repo.ResolvePermission(ctx, id, "approved")
	}
	return s.repo.ResolvePermission(ctx, id, "denied")
}

// --- pi.Sink implementation: events from the agent process land here. ---

func (s *Service) AssistantMessage(projectID, requestID int64, content string) {
	if err := s.repo.FinalizeAssistant(context.Background(), requestID, content); err != nil {
		log.Printf("persist assistant message (project %d): %v", projectID, err)
	}
}
func (s *Service) AssistantPartial(projectID, requestID int64, content string) {
	if err := s.repo.UpsertPartialAssistant(context.Background(), requestID, content); err != nil {
		log.Printf("persist streaming text (project %d): %v", projectID, err)
	}
}
func (s *Service) ToolStarted(projectID, requestID int64, callID, content string) {
	if err := s.repo.InsertToolStart(context.Background(), requestID, callID, content); err != nil {
		log.Printf("persist tool start (project %d): %v", projectID, err)
	}
}
func (s *Service) ToolFinished(projectID, requestID int64, callID, content, body string) {
	if err := s.repo.FinalizeTool(context.Background(), requestID, callID, content, body); err != nil {
		log.Printf("persist tool result (project %d): %v", projectID, err)
	}
}
func (s *Service) RequestSettled(projectID, requestID int64, status string) {
	if err := s.repo.CompleteRequest(context.Background(), requestID, status); err != nil {
		log.Printf("settle request %d (project %d): %v", requestID, projectID, err)
	}
	s.settleImprovement(projectID, requestID, status)
	s.settleWish(projectID, requestID, status)
	s.settleMake(projectID, requestID, status)
}

// settleImprovement closes the fix-me loop: when an agent request that
// came from inside a published app finishes, republish the project and,
// if the app is installed, refresh the installed copy — no human touches
// the factory.
func (s *Service) settleImprovement(projectID, requestID int64, status string) {
	ctx := context.Background()
	imp, err := s.repo.ImproveByRequest(ctx, requestID)
	if err != nil || imp == nil {
		return
	}
	if status != "completed" {
		_ = s.repo.SetImproveStatus(ctx, imp.ID, "failed")
		return
	}
	_ = s.repo.SetImproveStatus(ctx, imp.ID, "publishing")
	appID := imp.StoreAppID
	err = s.publish(ctx, projectID, func(_ int64, buildErr error) {
		if buildErr != nil {
			log.Printf("improve %d: republish failed: %v", imp.ID, buildErr)
			_ = s.repo.SetImproveStatus(ctx, imp.ID, "failed")
			return
		}
		app, err := s.repo.GetStoreApp(ctx, appID)
		if err == nil && app.Installed {
			if err := s.InstallApp(ctx, appID); err != nil {
				log.Printf("improve %d: reinstall failed: %v", imp.ID, err)
				_ = s.repo.SetImproveStatus(ctx, imp.ID, "failed")
				return
			}
		}
		_ = s.repo.SetImproveStatus(ctx, imp.ID, "done")
	})
	if err != nil {
		log.Printf("improve %d: publish: %v", imp.ID, err)
		_ = s.repo.SetImproveStatus(ctx, imp.ID, "failed")
	}
}

// RemixApp forks a published app into a new project — source copied,
// mutation prompt handed to the agent. The store becomes a gene pool.
func (s *Service) RemixApp(ctx context.Context, appID int64, mutation string) (Project, error) {
	mutation = strings.TrimSpace(mutation)
	if mutation == "" {
		return Project{}, ErrValidation{"Describe the mutation — what should the remix do differently?"}
	}
	app, err := s.repo.GetStoreApp(ctx, appID)
	if err != nil {
		return Project{}, err
	}
	if app.ProjectID == nil {
		return Project{}, ErrValidation{"This app has no linked project to remix."}
	}
	source, err := s.repo.GetProject(ctx, *app.ProjectID)
	if err != nil {
		return Project{}, err
	}
	srcDir, err := projectWorkdir(source)
	if err != nil {
		return Project{}, ErrValidation{"Source project directory unavailable: " + err.Error()}
	}
	remix, err := s.CreateProject(ctx, remixName(app.Name), "", source.Trusted)
	if err != nil {
		return Project{}, err
	}
	dstDir, err := projectWorkdir(remix)
	if err != nil {
		return remix, ErrValidation{"Remix directory unavailable: " + err.Error()}
	}
	if err := copyProjectTree(srcDir, dstDir); err != nil {
		return remix, ErrValidation{"Copied project incompletely: " + err.Error()}
	}
	s.appendTasteSignal("remix "+app.Name, mutation)
	msg := fmt.Sprintf("This project is a remix of %q — its full source was copied here as the starting point.\n\nMutation requested by the user:\n\n%s\n\nApply the mutation: rename the app appropriately (module path, page title, and any user-visible names), implement the change, keep what still serves the new purpose, delete what doesn't, and verify with 'go build -o /tmp/remix .'. The app must keep listening on $ADDR.", app.Name, mutation)
	if _, err := s.sendAgentMessage(ctx, remix.ID, "build", msg); err != nil {
		return remix, err
	}
	return remix, nil
}

func remixName(base string) string {
	return strings.TrimSpace(base) + " Remix"
}

// copyProjectTree copies a project's source, skipping build products,
// bundles, VCS internals, and review screenshots.
func copyProjectTree(src, dst string) error {
	skip := map[string]bool{".build": true, "dist": true, ".git": true}
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil || rel == "." {
			return err
		}
		top := strings.Split(rel, string(filepath.Separator))[0]
		if skip[top] || filepath.Base(p) == "review.png" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		dest := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, body, info.Mode().Perm())
	})
}

// AppBySlug resolves a published app from its improve/beacon slug.
func (s *Service) AppBySlug(ctx context.Context, slug string) (StoreApp, error) {
	return s.repo.GetStoreAppBySlug(ctx, strings.TrimSpace(slug))
}

// FileImprovement is the fix-me wormhole: text typed inside a published
// app (or on its store page) becomes a BUILD request on the app's project,
// and the loop auto-republishes + reinstalls when the agent finishes.
func (s *Service) FileImprovement(ctx context.Context, slug, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return ErrValidation{"Describe what should be better."}
	}
	app, err := s.repo.GetStoreAppBySlug(ctx, slug)
	if err != nil {
		return ErrValidation{"No published app matches this link."}
	}
	if app.ProjectID == nil {
		return ErrValidation{"This app has no linked project, so the agent can't work on it."}
	}
	msg := fmt.Sprintf("[improvement request filed from the running app %q]\n\nThe user asked for this improvement:\n\n%s\n\nImplement it in this project, keep everything else working, and verify with 'go build -o /tmp/app .'. The server must still listen on $ADDR and serve GET /. oozie republishes and restarts the app automatically when you finish.", app.Name, text)
	requestID, err := s.sendAgentMessage(ctx, *app.ProjectID, "build", msg)
	if err != nil {
		return err
	}
	s.appendTasteSignal("improve "+app.Name, text)
	return s.repo.InsertImproveRequest(ctx, requestID, app.ID, text)
}
func (s *Service) Question(projectID, requestID int64, rpcID, prompt, optionsJSON string) {
	if err := s.repo.InsertQuestion(context.Background(), projectID, requestID, rpcID, prompt, optionsJSON); err != nil {
		log.Printf("persist question (project %d): %v", projectID, err)
	}
}
func (s *Service) Permission(projectID, requestID int64, rpcID, name, reason string) {
	if err := s.repo.InsertPermission(context.Background(), projectID, requestID, rpcID, name, reason); err != nil {
		log.Printf("persist permission (project %d): %v", projectID, err)
	}
}
func (s *Service) AgentError(projectID, requestID int64, message string) {
	if err := s.repo.InsertMessage(context.Background(), requestID, "system", "error", message); err != nil {
		log.Printf("persist agent error (project %d): %v", projectID, err)
	}
}

func projectWorkdir(p Project) (string, error) {
	path, err := resolveWorkdir(p)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", err
	}
	seedProject(path)
	return path, nil
}

// resolveWorkdir expands the project's path without creating or seeding
// anything (deletion must never mkdir what it's about to remove).
func resolveWorkdir(p Project) (string, error) {
	path := strings.TrimSpace(p.ProjectPathDisplay)
	if path == "" {
		path = "~/Projects/" + strings.ToLower(strings.ReplaceAll(p.Name, " ", "-"))
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	return path, nil
}

// seedProject writes DESIGN.md and Tools/ into the workdir, skipping any
// file that already exists so per-project edits stick.
func seedProject(workdir string) {
	_ = fs.WalkDir(seedsFS, "seeds", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel("seeds", p)
		dest := filepath.Join(workdir, rel)
		if _, err := os.Stat(dest); err == nil {
			return nil
		}
		body, err := seedsFS.ReadFile(p)
		if err != nil {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return nil
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(dest, ".sh") {
			mode = 0o755
		}
		_ = os.WriteFile(dest, body, mode)
		return nil
	})
}

func newPiSessionID(projectID int64) string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("oozie-p%d-%s", projectID, hex.EncodeToString(buf))
}

func oozieSystemPrompt(p Project, workdir, improveURL string) string {
	return fmt.Sprintf(`You are running inside oozie, a local workspace whose purpose is building small personal web apps, as the agent for the project %q (working directory: %s).

How to behave in oozie:
- Requests arrive in one of two modes, stated at the top of each message.
- PLAN mode: produce a concise, numbered implementation plan. Do not create, modify, or delete any files. End by asking whether to proceed.
- BUILD mode: implement the request directly in the working directory, verifying your work as you go (build/tests where applicable).
- Your responses are rendered in a compact web timeline; keep them focused and skip decorative preamble.
- The user approves questions and permission dialogs through the oozie side panel; when you ask via a dialog, wait for that response.

Producing web apps (oozie's publish pipeline):
- When the user asks for an app, scaffold a Go module at the project root: go.mod plus a main package in main.go (or cmd/app). Prefer the standard library.
- The server MUST listen on the ADDR environment variable (host:port, for example 127.0.0.1:8091). If ADDR is empty, listen on 127.0.0.1:$PORT. Do not hardcode a port.
- GET / must return HTML with status 200. Data lives in a file under the working directory if the app needs storage (SQLite is fine; modernc.org/sqlite needs no C compiler).
- Put a footer link "Improve this app" to %q when that URL is non-empty. That page files a request back to you. Do not build any other feedback system.
- If OOZIE_BEACON_URL is set, a page view may GET it (failure is fine). It records that the app was opened.
- Verify with 'go build -o /tmp/app .' before declaring the work done. oozie then builds the same way and opens the page. No Swift, no Xcode, no .app bundle, no icon, no screenshot pass.

Design:
- Build one page, not a suite. The page itself is the preview.
- The project root contains TASTE.md — the user's personal design voice. Read it before any UI work; its rules override DESIGN.md wherever they conflict.
- The project root contains DESIGN.md — read it before any UI work and follow it.
`, p.Name, workdir, improveURL)
}

func wrapModeMessage(mode, message string) string {
	if mode == "plan" {
		return "[oozie mode: PLAN — plan only, do not modify files]\n\n" + message
	}
	return "[oozie mode: BUILD — implement directly]\n\n" + message
}
func (s *Service) SaveFeedback(ctx context.Context, projectID int64, typ, reason, extra string) error {
	if strings.TrimSpace(typ) == "" {
		return ErrValidation{"Feedback type is required."}
	}
	return s.repo.SaveFeedback(ctx, projectID, typ, reason, extra)
}
func (s *Service) GetDraft(ctx context.Context, projectID int64) (PublishDraft, error) {
	d, err := s.repo.GetDraft(ctx, projectID)
	if err != nil {
		return s.draftWithDefaults(ctx, PublishDraft{ProjectID: projectID, AutoInstall: true}), nil
	}
	return d, err
}

// draftWithDefaults fills any blank draft fields so publishing never
// dead-ends on an incomplete form: app name falls back to the project
// name, headline to a stock line, description to the headline. The user
// can polish the listing afterwards.
func (s *Service) draftWithDefaults(ctx context.Context, d PublishDraft) PublishDraft {
	if strings.TrimSpace(d.AppName) == "" {
		p, _ := s.repo.GetProject(ctx, d.ProjectID)
		d.AppName = p.Name
	}
	if strings.TrimSpace(d.Headline) == "" {
		d.Headline = "A focused project workspace"
	}
	if strings.TrimSpace(d.Description) == "" {
		d.Description = d.Headline
	}
	if d.PublishTarget == "" {
		d.PublishTarget = "public"
	}
	if d.Visibility == "" {
		d.Visibility = "unlisted"
	}
	if d.ScreenshotManifest == "" {
		d.ScreenshotManifest = "[]"
	}
	return d
}

func (s *Service) SaveDraft(ctx context.Context, d PublishDraft) error {
	d = s.draftWithDefaults(ctx, d)
	if strings.TrimSpace(d.AppName) == "" {
		return ErrValidation{"App name is required."}
	}
	return s.repo.SaveDraft(ctx, d)
}

// Publish builds the project into a web-app binary asynchronously: a job is
// queued immediately, a background worker runs the build, and on success
// the app appears in (or updates) the store with its artifact path.
func (s *Service) Publish(ctx context.Context, projectID int64) error {
	return s.publish(ctx, projectID, nil)
}

// publish queues a build job; after (optional) runs when the job settles,
// with the store app ID on success or the build error on failure.
func (s *Service) publish(ctx context.Context, projectID int64, after func(storeAppID int64, buildErr error)) error {
	draft, err := s.repo.GetDraft(ctx, projectID)
	if errors.Is(err, sql.ErrNoRows) {
		// No draft yet — publish anyway with sensible defaults so one
		// click always works; the listing can be polished afterwards.
		draft = s.draftWithDefaults(ctx, PublishDraft{ProjectID: projectID, AutoInstall: true})
		if err := s.SaveDraft(ctx, draft); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	project, err := s.repo.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	workdir, err := projectWorkdir(project)
	if err != nil {
		return ErrValidation{"Project directory unavailable: " + err.Error()}
	}
	jobID, err := s.repo.CreateJob(ctx, projectID)
	if err != nil {
		return err
	}
	s.jobs.Add(1)
	go s.runPublishJob(jobID, projectID, draft, workdir, after)
	return nil
}

func (s *Service) runPublishJob(jobID, projectID int64, draft PublishDraft, workdir string, after func(int64, error)) {
	defer s.jobs.Done()
	ctx := context.Background()
	if err := s.repo.SetJobRunning(ctx, jobID); err != nil {
		log.Printf("publish job %d: %v", jobID, err)
	}
	slug := build.Slug(draft.AppName)
	appPath, err := s.builder.Build(workdir, draft.AppName)
	if err != nil {
		_ = s.repo.FinishJob(ctx, jobID, "failed", err.Error(), nil)
		if after != nil {
			after(0, err)
		}
		return
	}
	appID, err := s.repo.UpsertStoreApp(ctx, projectID, draft, appPath, slug)
	if err != nil {
		_ = s.repo.FinishJob(ctx, jobID, "failed", "app built but store update failed: "+err.Error(), nil)
		if after != nil {
			after(0, err)
		}
		return
	}
	// One-click philosophy: a successful publish ends with the app running
	// on localhost unless the draft opted out. The job stays running until
	// that start succeeds, so "succeeded" means the app is reachable.
	if draft.AutoInstall {
		if err := s.InstallApp(ctx, appID); err != nil {
			_ = s.repo.FinishJob(ctx, jobID, "failed", err.Error(), &appID)
			if after != nil {
				after(appID, err)
			}
			return
		}
	}
	_ = s.repo.FinishJob(ctx, jobID, "succeeded", "", &appID)
	if after != nil {
		after(appID, nil)
	}
}

func (s *Service) ListJobs(ctx context.Context, status string) ([]PublishingJob, error) {
	return s.repo.ListJobs(ctx, status)
}
func (s *Service) ListStoreApps(ctx context.Context, q, filter string) ([]StoreApp, error) {
	return s.repo.ListStoreApps(ctx, strings.TrimSpace(q), filter)
}
func (s *Service) GetStoreApp(ctx context.Context, id int64) (StoreApp, error) {
	return s.repo.GetStoreApp(ctx, id)
}

// InstallApp starts the built binary on a free localhost port and marks
// the app installed. Reinstall stops the previous process first.
func (s *Service) InstallApp(ctx context.Context, id int64) error {
	app, err := s.repo.GetStoreApp(ctx, id)
	if err != nil {
		return err
	}
	if app.ArtifactPath == "" {
		return ErrValidation{"This app has no built artifact. Publish its project first."}
	}
	if _, err := os.Stat(app.ArtifactPath); err != nil {
		return ErrValidation{"The built app is missing on disk (" + app.ArtifactPath + "). Publish the project again."}
	}
	url, pid, err := s.spawnPublished(app)
	if err != nil {
		return err
	}
	if err := s.repo.SetRuntime(ctx, id, url, pid); err != nil {
		s.halt(id, pid, app.ArtifactPath)
		return err
	}
	return s.repo.InstallApp(ctx, id)
}

// OpenApp makes sure the installed app is listening and returns its URL.
// A launch event is recorded so the store can show real usage.
func (s *Service) OpenApp(ctx context.Context, id int64) (string, error) {
	app, err := s.repo.GetStoreApp(ctx, id)
	if err != nil {
		return "", err
	}
	if !app.Installed {
		return "", ErrValidation{"Start the app first."}
	}
	if app.PublicURL != "" && tcpUp(app.PublicURL) {
		s.RecordLaunch(ctx, app.BundleSlug)
		return app.PublicURL, nil
	}
	if err := s.InstallApp(ctx, id); err != nil {
		return "", err
	}
	app, err = s.repo.GetStoreApp(ctx, id)
	if err != nil {
		return "", err
	}
	s.RecordLaunch(ctx, app.BundleSlug)
	return app.PublicURL, nil
}

// UninstallApp stops the local server but keeps the store listing so the
// app can be started again.
func (s *Service) UninstallApp(ctx context.Context, id int64) error {
	app, err := s.repo.GetStoreApp(ctx, id)
	if err != nil {
		return err
	}
	s.halt(id, app.RuntimePID, app.ArtifactPath)
	if err := s.repo.SetRuntime(ctx, id, "", 0); err != nil {
		return err
	}
	return s.repo.UninstallApp(ctx, id)
}

// RemoveStoreApp uninstalls the app and deletes its store listing. The
// project and its build artifacts are untouched — republishing brings the
// app back.
func (s *Service) RemoveStoreApp(ctx context.Context, id int64) error {
	if err := s.UninstallApp(ctx, id); err != nil {
		return err
	}
	return s.repo.DeleteStoreApp(ctx, id)
}

func (s *Service) InstalledApps(ctx context.Context) ([]StoreApp, error) {
	return s.repo.InstalledApps(ctx)
}

// RecordLaunch handles a beacon ping from an installed app's launcher
// shim. Unknown slugs are ignored (the app may have been removed).
func (s *Service) RecordLaunch(ctx context.Context, slug string) {
	if strings.TrimSpace(slug) == "" {
		return
	}
	if err := s.repo.RecordAppEvent(ctx, slug, "launch"); err != nil {
		log.Printf("record launch for %q: %v", slug, err)
	}
}
func (s *Service) GetSettings(ctx context.Context) (Settings, error) { return s.repo.GetSettings(ctx) }
func (s *Service) SaveSettings(ctx context.Context, settings Settings) error {
	if settings.Appearance == "" {
		settings.Appearance = "system"
	}
	if settings.StyleProfile == "" {
		settings.StyleProfile = "graphite"
	}
	return s.repo.SaveSettings(ctx, settings)
}
