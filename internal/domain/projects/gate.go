package projects

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// UsePageProbe installs the job-fit check. Nil disables it.
func (s *Service) UsePageProbe(probe func(workdir, request string) (ok bool, reason, snippet string)) {
	s.pageProbe = probe
}

// acceptPage reports whether publish may continue. repairing means one
// repair turn was started and the caller must not mark the job failed.
func (s *Service) acceptPage(ctx context.Context, projectID int64, request string, onRetry func(newID int64)) (proceed, repairing bool) {
	if s.pageProbe == nil {
		return true, false
	}
	project, err := s.repo.GetProject(ctx, projectID)
	if err != nil {
		return true, false
	}
	wd, err := resolveWorkdir(project)
	if err != nil {
		return true, false
	}
	ok, reason, snippet := s.pageProbe(wd, request)
	_ = s.repo.SaveOutcome(ctx, projectID, ok, reason)
	if ok {
		s.outcomeRetried.Delete(projectID)
		return true, false
	}
	if onRetry != nil && s.retryOutcome(ctx, projectID, request, reason, snippet, onRetry) {
		return false, true
	}
	s.noteOutcomeStop(ctx, projectID, reason, snippet)
	return false, false
}

func (s *Service) retryOutcome(ctx context.Context, projectID int64, request, reason, snippet string, onRetry func(newID int64)) bool {
	if _, already := s.outcomeRetried.Load(projectID); already {
		return false
	}
	s.outcomeRetried.Store(projectID, true)
	msg := outcomeRepairMessage(request, reason, snippet)
	newID, err := s.sendAgentMessage(ctx, projectID, "build", msg)
	if err != nil || newID == 0 {
		s.outcomeRetried.Delete(projectID)
		return false
	}
	onRetry(newID)
	log.Printf("project %d: job-fit miss — one repair as request %d", projectID, newID)
	return true
}

func outcomeRepairMessage(request, reason, snippet string) string {
	msg := "The page compiled, but it does not do the job. Fix the HTML that GET / returns. Do not stop at a hello page.\n\nMiss: " + reason
	if strings.TrimSpace(snippet) != "" {
		msg += "\n\nWhat GET / showed: " + snippet
	}
	if strings.TrimSpace(request) != "" {
		msg += "\n\nOriginal request:\n" + strings.TrimSpace(request)
	}
	msg += "\n\n" + qualityBar + "\n\n" + scopeRestraint
	return msg
}

func (s *Service) noteOutcomeStop(ctx context.Context, projectID int64, reason, snippet string) {
	msg := reason
	if snippet != "" {
		msg += " Page said: " + snippet
	}
	if jobID, err := s.repo.CreateJob(ctx, projectID); err == nil {
		_ = s.repo.FinishJob(ctx, jobID, "failed", msg, nil)
	}
	log.Printf("project %d: job-fit failed: %s", projectID, reason)
}

func (s *Service) jobText(ctx context.Context, projectID, requestID int64) string {
	if text, err := s.repo.FirstBuildPrompt(ctx, projectID); err == nil && strings.TrimSpace(text) != "" {
		return text
	}
	if text, err := s.repo.FirstUserMessage(ctx, requestID); err == nil {
		return text
	}
	return ""
}

func (s *Service) restoreFrontDoor(ctx context.Context) {
	rows, err := s.repo.FrontDoorRows(ctx)
	if err != nil {
		log.Printf("restore front door: %v", err)
		return
	}
	for _, row := range rows {
		if row.Kind != "make" {
			continue
		}
		if row.Status == "failed" || row.Status == "cancelled" {
			_ = s.repo.DeleteFrontDoor(ctx, row.RequestID)
			continue
		}
		// A completed agent run that never published is settled again.
		// Streaming rows were already failed by SweepStaleAgentRequests.
		if row.Status == "completed" {
			if appID, err := s.repo.StoreAppIDForProject(ctx, row.ProjectID); err == nil && appID != 0 {
				_ = s.repo.DeleteFrontDoor(ctx, row.RequestID)
				continue
			}
			s.makeByRequest.Store(row.RequestID, row.ProjectID)
			s.settleMake(row.ProjectID, row.RequestID, "completed")
		}
	}
}

// CancelBuild stops the agent for a project and fails the wait screen.
func (s *Service) CancelBuild(ctx context.Context, projectID int64) error {
	if s.agent != nil {
		_ = s.agent.Abort(projectID)
	}
	status, _, err := s.repo.LatestRequest(ctx, projectID)
	if err == nil && (status == "streaming" || status == "waiting") {
		if id, _, rerr := s.repo.LatestRequestID(ctx, projectID); rerr == nil {
			_ = s.repo.CancelRequest(ctx, id)
			_ = s.repo.DeleteFrontDoor(ctx, id)
		}
	}
	_ = s.repo.DeleteFrontDoorProject(ctx, projectID)
	if jobID, err := s.repo.CreateJob(ctx, projectID); err == nil {
		_ = s.repo.FinishJob(ctx, jobID, "failed", "Cancelled.", nil)
	}
	return nil
}

// ExportToolData zips the tool's data/ directory. An empty tool still gets a zip.
func (s *Service) ExportToolData(ctx context.Context, appID int64) (string, error) {
	app, err := s.repo.GetStoreApp(ctx, appID)
	if err != nil {
		return "", err
	}
	if app.ProjectID == nil {
		return "", ErrValidation{"This tool has no project data to export."}
	}
	project, err := s.repo.GetProject(ctx, *app.ProjectID)
	if err != nil {
		return "", err
	}
	wd, err := resolveWorkdir(project)
	if err != nil {
		return "", err
	}
	dataDir := filepath.Join(wd, "data")
	_ = os.MkdirAll(dataDir, 0o755)
	out := filepath.Join(os.TempDir(), fmt.Sprintf("oozie-data-%d.zip", appID))
	f, err := os.Create(out)
	if err != nil {
		return "", err
	}
	zw := zip.NewWriter(f)
	_ = filepath.Walk(dataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dataDir, path)
		if err != nil {
			return nil
		}
		w, err := zw.Create(rel)
		if err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(w, src)
		return err
	})
	if err := zw.Close(); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return out, nil
}

// BackupDesk writes a copy of the desk database.
func (s *Service) BackupDesk(ctx context.Context, path string) error {
	if strings.TrimSpace(path) == "" {
		return ErrValidation{"Backup path is required."}
	}
	return s.repo.BackupTo(ctx, path)
}
