package app

import (
	"context"
	"database/sql"
	"io/fs"

	"oozie/internal/agent/native"
	"oozie/internal/agent/pi"
	"oozie/internal/domain/hub"
	"oozie/internal/domain/projects"
	"oozie/internal/web/render"
)

type App struct {
	config     Config
	database   *sql.DB
	renderer   *render.Renderer
	static     fs.FS
	agent      projects.CodingAgent
	service    *projects.Service
	hub        *hub.Service
	stopClocks context.CancelFunc
}

func New(config Config, database *sql.DB, renderer *render.Renderer, static fs.FS) *App {
	repo := projects.NewRepo(database)
	service := projects.NewService(repo)
	catalog := native.MergeCatalog(pi.LoadCatalog())
	keys := native.LoadKeys()
	agent := native.NewManager(catalog, service, keys)
	service.SetAgent(agent, catalog)
	// Prefer keys from env + auth file; do not spawn external pi to probe models.
	service.UseCredentialGate(func() map[string]bool {
		return native.SignedFromKeys(native.LoadKeys())
	}, func(model string) error {
		return agent.HasKeyFor(model)
	})
	service.SetBaseURL("http://" + config.Addr)
	service.RecoverOrphanedJobs(context.Background())
	service.ReclaimRuntimes(context.Background())
	desk := hub.NewService(hub.NewRepo(database), service)
	desk.Restore(context.Background())
	clockCtx, stopClocks := context.WithCancel(context.Background())
	service.StartBackground(clockCtx)
	return &App{config: config, database: database, renderer: renderer, static: static, agent: agent, service: service, hub: desk, stopClocks: stopClocks}
}

// Shutdown stops background clocks, published app servers, and the coding agent.
func (a *App) Shutdown() {
	a.stopClocks()
	a.hub.Stop()
	a.service.StopRuntimes()
	a.agent.Shutdown()
}
