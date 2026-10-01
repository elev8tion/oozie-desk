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

	"oozie-desk/internal/agent/pi"
	"oozie-desk/internal/build"
)

// seedsFS is materialized into every project workdir: DESIGN.md (the
// visual standard for local web apps). Files are only written if missing,
// so per-project edits stick.
//
//go:embed all:seeds
var seedsFS embed.FS

// CodingAgent is the desk build worker (in-repo native loop or legacy pi RPC).
type CodingAgent interface {
	Prompt(opts pi.StartOptions, requestID int64, message string) error
	StopProject(projectID int64)
	Abort(projectID int64) error
	SetModel(projectID int64, model string) error
	RespondValue(projectID int64, rpcID, value string) error
	RespondConfirm(projectID int64, rpcID string, confirmed bool) error
	RespondCancel(projectID int64, rpcID string) error
	Streaming(projectID int64) bool
	Stats(projectID int64) *pi.SessionStats
	Shutdown()
}

type Service struct {
	repo    *Repo
	agent   CodingAgent
	catalog pi.Catalog
	builder build.AppBuilder
	jobs    sync.WaitGroup
	baseURL string   // Oozie Desk's own address, used in improve links and beacon URLs
	procs   sync.Map // store app id -> *exec.Cmd for servers this process started

	// Per-app mutexes guard Install/Uninstall/Open so two starts of same app
	// cannot both pass halt before one stores its process.
	appMu    sync.Mutex
	appLocks map[int64]*sync.Mutex

	// Per-project mutexes serialize publish jobs (build + optional InstallApp).
	pubMu    sync.Mutex
	pubLocks map[int64]*sync.Mutex

	// createMu serializes default-path allocation for CreateProject.
	createMu sync.Mutex

	// slugMu serializes bundle-slug allocation across projects.
	slugMu sync.Mutex

	// onInstall is a test hook called at the very start of InstallApp.
	onInstall func()

	// adopted remembers reclaimed live runtimes so StopRuntimes can still kill them.
	adopted sync.Map // id -> struct{pid int; artifact string}

	// wishByRequest maps in-flight agent requests to the wish that spawned
	// them (in-memory: a restart mid-build fails the wish honestly at the
	// next startup sweep). makeByRequest is the same map for the front door.
	wishByRequest sync.Map
	makeByRequest sync.Map
	// makeCreditRetry counts automatic front-door re-prompts after credit/quota
	// refusals per project (capped in retryMakeAfterCredit).
	makeCreditRetry sync.Map
	// improveOrigin maps a retry agent request → the Fix wait request id.
	// improveCurrent maps the Fix wait request id → the live agent request.
	improveOrigin  sync.Map
	improveCurrent sync.Map
	improveRetryN  sync.Map // origin request id → hop count
	wishRetryN     sync.Map // wish id → hop count
	// incompleteScaffold marks projects that already got one "finish main.go" nudge.
	incompleteScaffold sync.Map
	// outcomeRetried marks projects that already got one job-fit repair.
	outcomeRetried sync.Map
	// outcomeMiss is the job-fit sentence shown while that repair is still running.
	outcomeMiss sync.Map
	// pageProbe checks GET / before publish. Nil skips the gate (tests).
	pageProbe func(workdir, request string) (ok bool, reason, snippet string)

	// signedIn is a test hook. Nil reads the pi auth file.
	signedIn func() map[string]bool
	// modelProbe tries a model. Nil means the first signed-in model is used.
	// A rejected probe is skipped. Created test data uses this instead of pi.
	modelProbe func(string) error
	deadModels map[string]bool
}

func NewService(repo *Repo) *Service {
	return &Service{repo: repo, builder: build.GoBuilder{}, baseURL: "http://127.0.0.1:8090"}
}

func (s *Service) lockApp(id int64) *sync.Mutex {
	s.appMu.Lock()
	if s.appLocks == nil {
		s.appLocks = make(map[int64]*sync.Mutex)
	}
	m, ok := s.appLocks[id]
	if !ok {
		m = &sync.Mutex{}
		s.appLocks[id] = m
	}
	s.appMu.Unlock()
	return m
}

func (s *Service) lockProject(id int64) *sync.Mutex {
	s.pubMu.Lock()
	if s.pubLocks == nil {
		s.pubLocks = make(map[int64]*sync.Mutex)
	}
	m, ok := s.pubLocks[id]
	if !ok {
		m = &sync.Mutex{}
		s.pubLocks[id] = m
	}
	s.pubMu.Unlock()
	return m
}

// SetBaseURL records the address published apps use to reach Oozie Desk
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

// StartBackground launches Oozie Desk's clocks: the TTL reaper that
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
	s.restoreFrontDoor(ctx)
}

// SetAgent wires the coding agent after construction (the manager's
// event sink is this service, so the two reference each other).
func (s *Service) SetAgent(agent CodingAgent, catalog pi.Catalog) {
	s.agent = agent
	s.catalog = catalog
}

// UseCredentialGate sets how the desk discovers signed-in providers and
// whether a model may start. Tests override with signedIn / modelProbe hooks.
func (s *Service) UseCredentialGate(signed func() map[string]bool, probe func(string) error) {
	s.signedIn = signed
	s.modelProbe = probe
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
	s.createMu.Lock()
	defer s.createMu.Unlock()
	slug := folderSlug(name)
	if path == "" {
		ps, _ := s.repo.ListProjects(ctx, "", "")
		var err error
		path, err = nextAutomaticPath(slug, ps)
		if err != nil {
			return Project{}, err
		}
		return s.repo.CreateProject(ctx, name, path, trusted)
	}
	// explicit path: check here (repo insert does not enforce duplicate path)
	ps, _ := s.repo.ListProjects(ctx, "", "")
	for _, pr := range ps {
		if pr.ProjectPathDisplay == path {
			return Project{}, ErrValidation{"Project path already in use."}
		}
	}
	return s.repo.CreateProject(ctx, name, path, trusted)
}

// folderSlug is a filesystem-safe directory name. Colons, commas, and
// other punctuation never land in the path.
func folderSlug(name string) string {
	s := strings.Trim(build.Slug(name), "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	if s == "" || s == "app" {
		return "tool"
	}
	return s
}

// nextAutomaticPath skips folders already used by a project or already
// present on disk, so a second desk does not rebuild into the first tool.
func nextAutomaticPath(slug string, existing []Project) (string, error) {
	for i := 1; i <= 100; i++ {
		path := "~/Projects/" + slug
		if i > 1 {
			path = fmt.Sprintf("~/Projects/%s-%d", slug, i)
		}
		if pathUsed(path, existing) || projectDirExists(path) {
			continue
		}
		return path, nil
	}
	return "", ErrValidation{"Could not find a free folder for this tool."}
}

func pathUsed(path string, existing []Project) bool {
	for _, pr := range existing {
		if pr.ProjectPathDisplay == path {
			return true
		}
	}
	return false
}

func projectDirExists(display string) bool {
	path, err := resolveWorkdir(Project{ProjectPathDisplay: display})
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// authPath is the pi credential file. OOZIE_AUTH_PATH lets a throwaway
// desk prove the unsigned-model gate without touching the real credentials.
func providerOf(model string) string {
	if i := strings.IndexByte(model, '/'); i > 0 {
		return model[:i]
	}
	return ""
}

func authPath() string {
	if p := os.Getenv("OOZIE_AUTH_PATH"); p != "" {
		return p
	}
	return pi.DefaultAuthPath()
}

// preferredSessionModel is the model the operator wants first: project session,
// else desk-wide coding model, else catalog default.
func (s *Service) preferredSessionModel(ctx context.Context, sessionModel string) string {
	if strings.TrimSpace(sessionModel) != "" {
		return strings.TrimSpace(sessionModel)
	}
	if st, err := s.repo.GetSettings(ctx); err == nil {
		if m := strings.TrimSpace(st.CodingModel); m != "" {
			return m
		}
	}
	return s.catalog.DefaultModel
}

// ModelChoices powers desk/settings pickers: current preferred model, catalog,
// and which providers have keys.
func (s *Service) ModelChoices(ctx context.Context) (current string, models []ModelOption, signed map[string]bool) {
	signed = pi.SignedProviders(authPath())
	if s.signedIn != nil {
		signed = s.signedIn()
	}
	current = s.preferredSessionModel(ctx, "")
	s.applyCatalogRefresh()
	models = s.models()
	return current, models, signed
}

// applyCatalogRefresh reloads chat models from the pi store. A live OpenRouter
// list replaces that provider when the key is set and the request succeeds.
// A network failure leaves the store list, including xAI chat models, in place.
func (s *Service) applyCatalogRefresh() {
	fresh := pi.RefreshChatModels(pi.OpenRouterKey())
	if len(fresh) == 0 {
		return
	}
	s.catalog.Models = pi.MergeRefreshedModels(s.catalog.Models, fresh)
	if s.catalog.DefaultModel == "" && len(s.catalog.Models) > 0 {
		s.catalog.DefaultModel = s.catalog.Models[0].Full
	}
}

// reviveModel clears in-process hop memory so a manual pick is tried again.
func (s *Service) reviveModel(model string) {
	if s.deadModels == nil || model == "" {
		return
	}
	delete(s.deadModels, model)
	if p := providerOf(model); p != "" {
		delete(s.deadModels, "provider:"+p)
	}
}

// ClearDeadModels drops automatic hop blacklists so every signed model is fair game again.
func (s *Service) ClearDeadModels() {
	s.deadModels = nil
}

// SetCodingModel saves the desk-wide preferred model and revives it for hops.
func (s *Service) SetCodingModel(ctx context.Context, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		st, err := s.repo.GetSettings(ctx)
		if err != nil {
			return err
		}
		st.CodingModel = ""
		return s.repo.SaveSettings(ctx, st)
	}
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
	st, err := s.repo.GetSettings(ctx)
	if err != nil {
		return err
	}
	st.CodingModel = model
	if err := s.repo.SaveSettings(ctx, st); err != nil {
		return err
	}
	s.reviveModel(model)
	return nil
}

func (s *Service) modelForNewBuild(sessionModel string) (string, error) {
	return s.modelForNewBuildCtx(context.Background(), sessionModel)
}

func (s *Service) modelForNewBuildCtx(ctx context.Context, sessionModel string) (string, error) {
	signed := pi.SignedProviders(authPath())
	if s.signedIn != nil {
		signed = s.signedIn()
	}
	prefer := s.preferredSessionModel(ctx, sessionModel)
	candidates := pi.CandidateModels(s.catalog, prefer, signed)
	if len(candidates) == 0 {
		return "", ErrValidation{"This model is not signed in."}
	}
	var rejected bool
	var skipProvider string
	for _, model := range candidates {
		if skipProvider != "" && providerOf(model) == skipProvider {
			continue
		}
		if s.deadModels[model] || s.deadModels["provider:"+providerOf(model)] {
			rejected = true
			continue
		}
		// Nil probe = accept the first signed-in candidate (native agent).
		// Tests and legacy pi wiring may still set modelProbe.
		if s.modelProbe == nil {
			return model, nil
		}
		err := s.modelProbe(model)
		if err == nil {
			return model, nil
		}
		if !pi.ModelRejected(err.Error()) && !strings.Contains(err.Error(), "did not answer") && !strings.Contains(err.Error(), "No API key") {
			return "", ErrValidation{err.Error()}
		}
		s.noteModelRefusal(model, err)
		rejected = true
		// One refusal does not record provider:. Skip siblings only in this
		// selection so a 404 is not retried as every model on that provider.
		if !strings.Contains(strings.ToLower(err.Error()), "no api key") {
			skipProvider = providerOf(model)
		}
	}
	if rejected {
		return "", ErrValidation{"No model answered."}
	}
	return "", ErrValidation{"This model is not signed in."}
}

// modelForRetry picks the next signed-in model after a real provider refusal.
// It never returns exclude. Same model is not a hop.
func (s *Service) modelForRetry(ctx context.Context, exclude string) (string, error) {
	exclude = strings.TrimSpace(exclude)
	if exclude != "" {
		if s.deadModels == nil {
			s.deadModels = map[string]bool{}
		}
		s.deadModels[exclude] = true
	}
	model, err := s.modelForNewBuildCtx(ctx, "")
	if err != nil {
		return "", err
	}
	if !realHop(exclude, model) {
		return "", ErrValidation{"No other signed-in model is available."}
	}
	return model, nil
}

// realHop is true only when the next model is a different signed-in model.
func realHop(failed, next string) bool {
	failed, next = strings.TrimSpace(failed), strings.TrimSpace(next)
	return next != "" && next != failed
}

func (s *Service) pinSessionModel(ctx context.Context, projectID int64, model string) error {
	session, err := s.repo.GetSession(ctx, projectID)
	if err != nil {
		return err
	}
	return s.repo.SetSessionModel(ctx, session.ID, model)
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
	return s.SendAgentMessageModel(ctx, projectID, mode, message, "")
}

// SendAgentMessageModel optionally locks the project (and desk) onto model first.
func (s *Service) SendAgentMessageModel(ctx context.Context, projectID int64, mode, message, model string) error {
	if model = strings.TrimSpace(model); model != "" {
		if err := s.SelectModel(ctx, projectID, model); err != nil {
			return err
		}
	}
	_, err := s.sendAgentMessage(ctx, projectID, mode, message)
	return err
}

// recordFailedStart leaves a failed agent request when the build never
// started (no model key, agent missing). Make-wait can then show the error
// instead of spinning on "Starting."
func (s *Service) recordFailedStart(ctx context.Context, projectID int64, msg string) {
	if projectID == 0 || strings.TrimSpace(msg) == "" {
		return
	}
	if status, _, err := s.repo.LatestRequest(ctx, projectID); err == nil && status != "" {
		return
	}
	session, err := s.repo.GetSession(ctx, projectID)
	if err != nil {
		return
	}
	rid, err := s.repo.CreateAgentRequest(ctx, session.ID, "build", "(start failed)")
	if err != nil {
		return
	}
	_ = s.repo.InsertMessage(ctx, rid, "system", "error", msg)
	_ = s.repo.CompleteRequest(ctx, rid, "failed")
}

// sendAgentMessage files an agent request and returns its ID so callers
// (the improve loop, the fairy) can watch for it to settle.
func (s *Service) sendAgentMessage(ctx context.Context, projectID int64, mode, message string) (int64, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return 0, ErrValidation{"Message is required."}
	}
	if s.agent == nil {
		return 0, ErrValidation{"The coding agent is not configured."}
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
	model, err := s.modelForNewBuildCtx(ctx, session.Model)
	if err != nil {
		return 0, err
	}
	if model != session.Model {
		if err := s.repo.SetSessionModel(ctx, session.ID, model); err != nil {
			return 0, err
		}
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
	history, _ := s.repo.RecentTurns(ctx, projectID, 8)
	var turns []pi.Turn
	for _, row := range history {
		turns = append(turns, pi.Turn{Role: row.Role, Content: row.Content})
	}
	requestID, err := s.repo.CreateAgentRequest(ctx, session.ID, mode, message)
	if err != nil {
		return 0, err
	}
	opts := pi.StartOptions{
		ProjectID:    projectID,
		Workdir:      workdir,
		Model:        model,
		PiSessionID:  session.PiSessionID,
		SystemPrompt: oozieSystemPrompt(project, workdir, s.baseURL, s.improveURL(ctx, project), s.repo.IndustryPack(ctx), s.LoadTaste()),
		Trusted:      project.Trusted,
		History:      turns,
	}
	if err := s.agent.Prompt(opts, requestID, wrapModeMessage(mode, message)); err != nil {
		_ = s.repo.InsertMessage(ctx, requestID, "system", "error", "Failed to start agent: "+err.Error())
		_ = s.repo.CompleteRequest(ctx, requestID, "failed")
		return 0, ErrValidation{"Could not start the coding agent: " + err.Error()}
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
	model = strings.TrimSpace(model)
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
	// Manual pick is authoritative: revive hop memory and set desk-wide preferred.
	if err := s.SetCodingModel(ctx, model); err != nil {
		return err
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
	originID := requestID
	if v, ok := s.improveOrigin.Load(requestID); ok {
		originID = v.(int64)
	}
	imp, err := s.repo.ImproveByRequest(ctx, originID)
	if err != nil || imp == nil {
		return
	}
	if status != "completed" {
		if s.retryImproveAfterModel(ctx, projectID, originID, requestID, imp) {
			return
		}
		_ = s.repo.SetImproveStatus(ctx, imp.ID, "failed")
		s.improveOrigin.Delete(requestID)
		s.improveCurrent.Delete(originID)
		s.improveRetryN.Delete(originID)
		return
	}
	s.improveOrigin.Delete(requestID)
	s.improveCurrent.Delete(originID)
	s.improveRetryN.Delete(originID)
	if proceed, repairing := s.acceptPage(ctx, projectID, imp.Note, func(newID int64) {
		s.improveOrigin.Store(newID, originID)
		s.improveCurrent.Store(originID, newID)
	}); !proceed {
		if !repairing {
			_ = s.repo.SetImproveStatus(ctx, imp.ID, "failed")
		}
		return
	}
	_ = s.repo.SetImproveStatus(ctx, imp.ID, "publishing")
	appID := imp.StoreAppID
	// Load draft before publish: if AutoInstall (default), the publish job itself
	// already calls InstallApp; callback must not do it again.
	draft, _ := s.repo.GetDraft(ctx, projectID)
	autoInstall := draft.AutoInstall || draft.ProjectID == 0 // missing draft means default true
	err = s.publish(ctx, projectID, func(_ int64, buildErr error) {
		if buildErr != nil {
			log.Printf("improve %d: republish failed: %v", imp.ID, buildErr)
			_ = s.repo.SetImproveStatus(ctx, imp.ID, "failed")
			return
		}
		app, err := s.repo.GetStoreApp(ctx, appID)
		if err == nil && app.Installed && !autoInstall {
			// Only when AutoInstall=false: callback Install picks up new binary
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

const maxImproveModelRetries = 4

// retryImproveAfterModel hops Fix to the next signed-in model when the current
// one is refused (credits, overload). The Fix wait URL keeps the origin request id.
func (s *Service) retryImproveAfterModel(ctx context.Context, projectID, originID, failedID int64, imp *ImproveRequest) bool {
	n := 0
	if v, ok := s.improveRetryN.Load(originID); ok {
		n, _ = v.(int)
	}
	if n >= maxImproveModelRetries {
		return false
	}
	_, errMsg, err := s.repo.RequestStatus(ctx, failedID)
	if err != nil || !pi.ModelRejected(errMsg) {
		return false
	}
	session, serr := s.repo.GetSession(ctx, projectID)
	if serr != nil {
		return false
	}
	next, nerr := s.modelForRetry(ctx, session.Model)
	if nerr != nil || !realHop(session.Model, next) {
		log.Printf("improve origin %d: refusal on %s — no other model, not hopping", originID, session.Model)
		return false
	}
	if err := s.pinSessionModel(ctx, projectID, next); err != nil {
		return false
	}
	msg, err := s.repo.FirstUserMessage(ctx, failedID)
	if err != nil || strings.TrimSpace(msg) == "" {
		return false
	}
	newID, err := s.sendAgentMessage(ctx, projectID, "build", msg)
	if err != nil || newID == 0 {
		s.improveRetryN.Store(originID, n+1)
		return false
	}
	s.improveRetryN.Store(originID, n+1)
	s.improveOrigin.Store(newID, originID)
	s.improveCurrent.Store(originID, newID)
	s.improveOrigin.Delete(failedID)
	log.Printf("improve origin %d: refusal on %s request %d — switched to %s as request %d (attempt %d)", originID, session.Model, failedID, next, newID, n+1)
	return true
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
	draft := PublishDraft{
		ProjectID:   remix.ID,
		AppName:     remix.Name,
		Headline:    wishHeadline(mutation),
		Description: mutation,
		AutoInstall: true,
	}
	if err := s.repo.SaveDraft(ctx, draft); err != nil {
		return remix, err
	}
	msg := remixMessage(app.Name, mutation)
	requestID, err := s.sendAgentMessage(ctx, remix.ID, "build", msg)
	if err != nil {
		s.recordFailedStart(ctx, remix.ID, err.Error())
		return remix, err
	}
	s.trackFrontDoor(requestID, remix.ID)
	return remix, nil
}

func remixName(base string) string {
	return strings.TrimSpace(base) + " Remix"
}

// copyProjectTree copies a project's source, skipping build products,
// bundles, VCS internals, review screenshots, and runtime usage data.
// data/ and database files stay with the source desk so a remix never
// inherits another person's records.
func copyProjectTree(src, dst string) error {
	skipDir := map[string]bool{".build": true, "dist": true, ".git": true, toolDataDirName: true}
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil || rel == "." {
			return err
		}
		base := filepath.Base(p)
		top := strings.Split(rel, string(filepath.Separator))[0]
		if skipDir[top] || base == "review.png" || isToolDataFile(base) {
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

// isToolDataFile reports names that hold runtime usage data and must not
// travel with remix or share artifacts.
func isToolDataFile(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case ".env", ".env.local":
		return true
	}
	for _, suf := range []string{".db", ".db-wal", ".db-shm", ".sqlite", ".sqlite3", ".sqlite-wal", ".sqlite-shm"} {
		if strings.HasSuffix(lower, suf) {
			return true
		}
	}
	return false
}

// AppBySlug resolves a published app from its improve/beacon slug.
func (s *Service) AppBySlug(ctx context.Context, slug string) (StoreApp, error) {
	return s.repo.GetStoreAppBySlug(ctx, strings.TrimSpace(slug))
}

// FileImprovement is the fix-me wormhole: text typed inside a published
// tool becomes a BUILD request on its project; the loop auto-republishes
// and restarts when the agent finishes. Returns the agent request id for
// the Fix wait screen.
func (s *Service) FileImprovement(ctx context.Context, slug, text string) (int64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, ErrValidation{"Describe what should be better."}
	}
	app, err := s.repo.GetStoreAppBySlug(ctx, slug)
	if err != nil {
		return 0, ErrValidation{"No tool on this desk matches this link."}
	}
	if app.ProjectID == nil {
		return 0, ErrValidation{"This tool has no linked project, so the agent can't work on it."}
	}
	msg := improvementMessage(app.Name, text)
	requestID, err := s.sendAgentMessage(ctx, *app.ProjectID, "build", msg)
	if err != nil {
		return 0, err
	}
	s.appendTasteSignal("improve "+app.Name, text)
	if err := s.repo.InsertImproveRequest(ctx, requestID, app.ID, text); err != nil {
		return 0, err
	}
	return requestID, nil
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
	// A refusal marks only this model. provider: is reserved for a missing API key.
	if !pi.ModelRejected(message) {
		return
	}
	session, err := s.repo.GetSession(context.Background(), projectID)
	if err != nil || session.Model == "" {
		return
	}
	if s.deadModels == nil {
		s.deadModels = map[string]bool{}
	}
	s.noteModelRefusal(session.Model, errors.New(message))
}

// noteModelRefusal marks one model dead. The whole provider is marked dead
// only when the error says there is no API key.
func (s *Service) noteModelRefusal(model string, err error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	if s.deadModels == nil {
		s.deadModels = map[string]bool{}
	}
	s.deadModels[model] = true
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "no api key") {
		return
	}
	if p := providerOf(model); p != "" {
		s.deadModels["provider:"+p] = true
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
		path = "~/Projects/" + folderSlug(p.Name)
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

func oozieSystemPrompt(p Project, workdir, deskURL, improveURL, industryPack, taste string) string {
	prompt := fmt.Sprintf(`You are running inside Oozie Desk, a local desk whose purpose is building small personal tools, as the agent for the project %q (working directory: %s).

How to behave in Oozie Desk:
- Requests arrive in one of two modes, stated at the top of each message.
- PLAN mode: produce a concise, numbered implementation plan. Do not create, modify, or delete any files. End by asking whether to proceed.
- BUILD mode: implement the request directly. The user's job comes first. A compiling stub is a failure.
- Your responses are rendered in a compact web timeline; keep them focused and skip decorative preamble.
- The user approves questions and permission dialogs through the Oozie Desk side panel; when you ask via a dialog, wait for that response. On front-door builds, do not ask — pick defaults.

Producing web apps (Oozie Desk's publish pipeline):
- Scaffold a Go module at the project root: go.mod plus main.go. Prefer the standard library. Add a dependency only when the tool cannot work without it.
- The server MUST listen on the ADDR environment variable (host:port). If ADDR is empty, listen on 127.0.0.1:$PORT. Do not hardcode a port.
- GET / must return HTML with status 200 that does the user's job.
- Data isolation: durable user records only under data/ (or $OOZIE_DATA_DIR). Prefer SQLite there (modernc.org/sqlite). Never hardcode personal records, real usage rows, or API keys into source. A share sends the recipe only; data/ stays on this desk.
- Every page MUST include a footer link labeled "Back to desk" to %q (also $OOZIE_DESK_URL), with target="_top".
- Also put a footer link "Fix" to %q when that URL is non-empty. Do not build any other feedback system.
- If OOZIE_BEACON_URL is set, a page view may GET it (failure is fine).
- Verify with 'go build -o /tmp/app .' before declaring the work done. No Swift, no Xcode, no .app bundle.

%s

%s

%s
`, p.Name, workdir, deskURL, improveURL, qualityBar, scopeRestraint, uiSkeleton)
	if rules := tasteRules(taste); rules != "" {
		prompt += "\nUser taste — these override the generic design:\n" + rules + "\n"
	} else {
		prompt += "\nNo personal taste rules yet. Follow the quality bar above. DESIGN.md in the project root is the generic standard; read it if you need the long form.\n"
	}
	if strings.TrimSpace(industryPack) != "" {
		prompt += "\nIndustry pack " + strings.TrimSpace(industryPack) + ":\nFollow that pack's terms when they do not conflict with the contract above. The pack does not change who can connect or where the tool runs.\n"
	}
	return prompt
}

func wrapModeMessage(mode, message string) string {
	if mode == "plan" {
		return "[Oozie Desk mode: PLAN — plan only, do not modify files]\n\n" + message
	}
	return "[Oozie Desk mode: BUILD — implement directly]\n\n" + message
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
	if err := s.gatePublish(ctx, projectID); err != nil {
		return err
	}
	return s.publish(ctx, projectID, nil)
}

// gatePublish runs the same job check as the front door, before go build.
// A miss returns an error and does not mark the job succeeded.
func (s *Service) gatePublish(ctx context.Context, projectID int64) error {
	if s.pageProbe == nil {
		return nil
	}
	project, err := s.repo.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	wd, err := resolveWorkdir(project)
	if err != nil {
		return err
	}
	job := s.checkedJob(ctx, projectID, s.jobText(ctx, projectID, 0))
	ok, reason, snippet := s.pageProbe(wd, job)
	_ = s.repo.SaveOutcome(ctx, projectID, ok, reason)
	if ok {
		s.outcomeMiss.Delete(projectID)
		return nil
	}
	if strings.TrimSpace(reason) == "" {
		reason = "GET / does not show the job."
	}
	if strings.TrimSpace(snippet) != "" {
		reason += " Page said: " + strings.TrimSpace(snippet)
	}
	if jobID, jerr := s.repo.CreateJob(ctx, projectID); jerr == nil {
		_ = s.repo.FinishJob(ctx, jobID, "failed", reason, nil)
	}
	return ErrValidation{reason}
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

// storeWithUniqueSlug keeps a project's existing slug. A new app gets
// build.Slug(name), then name-2, name-3, ... Caller holds slugMu.
func (s *Service) storeWithUniqueSlug(ctx context.Context, projectID int64, draft PublishDraft, appPath string) (int64, error) {
	slug := ""
	if appID, err := s.repo.StoreAppIDForProject(ctx, projectID); err == nil && appID != 0 {
		if old, err := s.repo.GetStoreApp(ctx, appID); err == nil && old.BundleSlug != "" {
			slug = old.BundleSlug
		}
	}
	kept := slug != ""
	base := build.Slug(draft.AppName)
	for attempt := 0; attempt < 8; attempt++ {
		if slug == "" {
			slug = base
			for i := 2; ; i++ {
				_, err := s.repo.GetStoreAppBySlug(ctx, slug)
				if errors.Is(err, sql.ErrNoRows) {
					break
				}
				if err != nil {
					return 0, err
				}
				slug = fmt.Sprintf("%s-%d", base, i)
			}
		}
		id, err := s.repo.UpsertStoreApp(ctx, projectID, draft, appPath, slug)
		if err != nil && !kept && strings.Contains(err.Error(), "UNIQUE") {
			slug = ""
			continue
		}
		return id, err
	}
	return 0, fmt.Errorf("could not allocate a unique slug for %s", draft.AppName)
}

func (s *Service) runPublishJob(jobID, projectID int64, draft PublishDraft, workdir string, after func(int64, error)) {
	m := s.lockProject(projectID)
	m.Lock()
	defer m.Unlock()
	defer s.jobs.Done()
	ctx := context.Background()
	if err := s.repo.SetJobRunning(ctx, jobID); err != nil {
		log.Printf("publish job %d: %v", jobID, err)
	}
	appPath, err := s.builder.Build(workdir, draft.AppName)
	if err != nil {
		_ = s.repo.FinishJob(ctx, jobID, "failed", err.Error(), nil)
		if after != nil {
			after(0, err)
		}
		return
	}
	s.slugMu.Lock()
	appID, err := s.storeWithUniqueSlug(ctx, projectID, draft, appPath)
	s.slugMu.Unlock()
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
	if s.onInstall != nil {
		s.onInstall()
	}
	m := s.lockApp(id)
	m.Lock()
	defer m.Unlock()
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
	m := s.lockApp(id)
	m.Lock()
	app, err := s.repo.GetStoreApp(ctx, id)
	if err != nil {
		m.Unlock()
		return "", err
	}
	if !app.Installed {
		m.Unlock()
		return "", ErrValidation{"Start the app first."}
	}
	if appIsOurs(app) {
		s.RecordLaunch(ctx, app.BundleSlug)
		m.Unlock()
		return app.PublicURL, nil
	}
	m.Unlock()
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
	m := s.lockApp(id)
	m.Lock()
	defer m.Unlock()
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
	settings.CodingModel = strings.TrimSpace(settings.CodingModel)
	if settings.CodingModel != "" {
		found := false
		for _, m := range s.catalog.Models {
			if m.Full == settings.CodingModel {
				found = true
				break
			}
		}
		if !found {
			return ErrValidation{"Unknown model: " + settings.CodingModel}
		}
		s.reviveModel(settings.CodingModel)
	}
	return s.repo.SaveSettings(ctx, settings)
}
