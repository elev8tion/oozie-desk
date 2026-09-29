package app

import (
	"log"
	"net/http"
	"runtime/debug"
	"time"

	"oozie/internal/domain/projects"
)

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()

	h := projects.NewHandlers(a.service, a.renderer)

	static := http.StripPrefix("/static/", http.FileServerFS(a.static))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /{$}", h.Home)
	mux.HandleFunc("POST /make", h.Make)
	mux.HandleFunc("GET /make/{id}", h.MakeWait)
	mux.HandleFunc("GET /fragments/make/{id}", h.MakeStatus)
	mux.HandleFunc("GET /onboarding", h.Onboarding)

	mux.HandleFunc("GET /projects", h.Projects)
	mux.HandleFunc("GET /projects/new", h.NewProject)
	mux.HandleFunc("POST /projects", h.CreateProject)
	mux.HandleFunc("GET /projects/{id}", h.ShowProject)
	mux.HandleFunc("POST /projects/{id}/archive", h.ArchiveProject)
	mux.HandleFunc("POST /projects/{id}/trust", h.SetTrusted)
	mux.HandleFunc("POST /projects/{id}/delete", h.DeleteProject)
	mux.HandleFunc("GET /fragments/projects/list", h.ProjectsList)

	mux.HandleFunc("GET /projects/{id}/agent", h.Agent)
	mux.HandleFunc("GET /projects/{id}/agent/timeline", h.AgentTimeline)
	mux.HandleFunc("POST /projects/{id}/agent/model", h.SelectAgentModel)
	mux.HandleFunc("POST /projects/{id}/agent/requests", h.AgentRequest)
	mux.HandleFunc("POST /projects/{id}/agent/requests/{requestID}/cancel", h.CancelAgentRequest)
	mux.HandleFunc("POST /projects/{id}/agent/questions/{toolUseID}/answer", h.AnswerQuestion)
	mux.HandleFunc("POST /projects/{id}/agent/questions/{toolUseID}/dismiss", h.DismissQuestion)
	mux.HandleFunc("POST /projects/{id}/agent/permissions/{requestID}", h.Permission)
	mux.HandleFunc("POST /projects/{id}/feedback", h.Feedback)

	// Optional launch pings from published apps (localhost only).
	mux.HandleFunc("GET /api/beacon/{slug}", h.Beacon)
	mux.HandleFunc("POST /api/beacon/{slug}", h.Beacon)

	// The fix-me page published apps link from their footer.
	mux.HandleFunc("GET /improve/{slug}", h.ImprovePage)
	mux.HandleFunc("POST /improve/{slug}", h.ImproveSubmit)

	mux.HandleFunc("GET /store", h.Store)
	mux.HandleFunc("GET /store/apps/{id}", h.StoreApp)
	mux.HandleFunc("POST /store/apps/{id}/install", h.InstallApp)
	mux.HandleFunc("POST /store/apps/{id}/open", h.OpenApp)
	mux.HandleFunc("POST /store/apps/{id}/uninstall", h.UninstallApp)
	mux.HandleFunc("POST /store/apps/{id}/remove", h.RemoveStoreApp)
	mux.HandleFunc("POST /store/apps/{id}/remix", h.RemixApp)
	mux.HandleFunc("GET /store/apps/{id}/recipe", h.ExportRecipe)
	mux.HandleFunc("GET /recipes", h.ImportRecipePage)
	mux.HandleFunc("GET /recipes/import", h.ImportRecipePage)
	mux.HandleFunc("POST /recipes/import", h.ImportRecipe)
	mux.HandleFunc("GET /installed-apps", h.InstalledApps)
	mux.HandleFunc("GET /fragments/store/results", h.StoreResults)

	mux.HandleFunc("GET /publishing/jobs", h.PublishingJobs)
	mux.HandleFunc("GET /fragments/publishing/jobs", h.PublishingJobsList)
	mux.HandleFunc("GET /projects/{id}/publish", h.PublishPage)
	mux.HandleFunc("POST /projects/{id}/publish/draft", h.SaveDraft)
	mux.HandleFunc("POST /projects/{id}/publish", h.Publish)

	mux.HandleFunc("GET /wishes", h.Wishes)
	mux.HandleFunc("POST /wishes", h.AddWish)
	mux.HandleFunc("POST /wishes/{id}/delete", h.DeleteWish)
	mux.HandleFunc("POST /wishes/{id}/build", h.BuildWish)

	mux.HandleFunc("GET /settings", h.Settings)
	mux.HandleFunc("POST /settings", h.SaveSettings)
	mux.HandleFunc("POST /settings/taste", h.SaveTaste)

	return withRecovery(withLogging(mux))
}

// withRecovery converts handler panics into a logged 500 instead of a
// dropped connection.
func withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic on %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/static/" || len(r.URL.Path) > 8 && r.URL.Path[:8] == "/static/" {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		log.Printf("%d %s %s (%s)", rec.status, r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
