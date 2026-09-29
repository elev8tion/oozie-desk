package app

import (
	"context"
	"database/sql"
	"io/fs"

	"oozie/internal/agent/pi"
	"oozie/internal/domain/projects"
	"oozie/internal/web/render"
)

type App struct {
	config     Config
	database   *sql.DB
	renderer   *render.Renderer
	static     fs.FS
	agent      *pi.Manager
	service    *projects.Service
	stopClocks context.CancelFunc
}

func New(config Config, database *sql.DB, renderer *render.Renderer, static fs.FS) *App {
	repo := projects.NewRepo(database)
	service := projects.NewService(repo)
	catalog := pi.LoadCatalog()
	agent := pi.NewManager(catalog, service)
	service.SetAgent(agent, catalog)
	service.SetBaseURL("http://" + config.Addr)
	service.RecoverOrphanedJobs(context.Background())
	clockCtx, stopClocks := context.WithCancel(context.Background())
	service.StartBackground(clockCtx)
	return &App{config: config, database: database, renderer: renderer, static: static, agent: agent, service: service, stopClocks: stopClocks}
}

// Shutdown stops background clocks, published app servers, and pi agents.
func (a *App) Shutdown() {
	a.stopClocks()
	a.service.StopRuntimes()
	a.agent.Shutdown()
}
