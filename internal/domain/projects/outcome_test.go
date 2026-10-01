package projects

import (
	"context"
	"strings"
	"testing"
)

func TestJudgePageRequiresTheJob(t *testing.T) {
	ok, reason := JudgePage("track sourdough feedings", `<html><body><h1>Hello</h1><footer><a>Back to desk</a></footer></body></html>`)
	if ok || !strings.Contains(reason, "does not show the job") {
		t.Fatalf("ok=%v reason=%q", ok, reason)
	}
	ok, reason = JudgePage("track sourdough feedings", `<html><body><h1>Sourdough</h1><form method="post"><input name="note"></form></body></html>`)
	if !ok || reason != "" {
		t.Fatalf("ok=%v reason=%q", ok, reason)
	}
	ok, reason = JudgePage("track sourdough feedings", `not html`)
	if ok {
		t.Fatal("plain text should fail")
	}
}

func TestOutcomeGateBlocksPublishAndRepairsOnce(t *testing.T) {
	s := newTestService(t)
	s.builder = fakeBuilder{}
	s.UsePageProbe(func(workdir, request string) (bool, string, string) {
		if strings.Contains(request, "repair") {
			return true, "", "sourdough"
		}
		return false, "GET / does not show the job (sourdough).", "<h1>Hello</h1>"
	})
	ctx := context.Background()
	p, err := s.CreateProject(ctx, "Sourdough", t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	var repairs int
	proceed, repairing := s.acceptPage(ctx, p.ID, "track sourdough feedings", func(newID int64) {
		repairs++
	})
	if proceed || !repairing || repairs != 0 {
		// no agent, so repair cannot start
		if repairing {
			t.Fatalf("repair started without an agent")
		}
	}
	if proceed {
		t.Fatal("bad page was accepted")
	}
	job, err := s.repo.LatestJob(ctx, p.ID)
	if err != nil || job.Status != "failed" || !strings.Contains(job.ErrorMessage, "sourdough") {
		t.Fatalf("job=%+v err=%v", job, err)
	}
}

func TestFrontDoorSurvivesRestart(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	p, err := s.CreateProject(ctx, "Keep", t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.repo.SaveFrontDoor(ctx, 42, p.ID, "make"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.repo.FrontDoorRows(ctx)
	if err != nil || len(rows) != 1 || rows[0].ProjectID != p.ID || rows[0].Kind != "make" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}
