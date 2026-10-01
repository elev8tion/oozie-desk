package projects

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJudgePageRequiresTheJob(t *testing.T) {
	ok, reason := JudgePage("track sourdough feedings", `<html><body><h1>Hello</h1><footer><a>Back to desk</a></footer></body></html>`)
	if ok || !strings.Contains(reason, "does not show the job") {
		t.Fatalf("ok=%v reason=%q", ok, reason)
	}
	ok, reason = JudgePage("track sourdough feedings", `<html><body><h1>Sourdough</h1><form method="post"><input name="note"></form></body></html>`)
	if ok {
		t.Fatal("one real noun must not pass when two or more remain")
	}
	ok, reason = JudgePage("track sourdough feedings", `<html><body><h1>Sourdough</h1><p>Feedings</p><form method="post"><input name="note"></form></body></html>`)
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

func TestBoilerplateNounsDoNotPassOrFail(t *testing.T) {
	req := "Rebuild this tool from its recipe. The prompts below are the spec — implement that job. Project remix quality scope desk contract. Track sourdough feedings."
	ok, reason := JudgePage(req, `<html><body><h1>Recipe</h1><p>Rebuild the project from prompts and the spec.</p></body></html>`)
	if ok {
		t.Fatal("boilerplate nouns must not pass the page")
	}
	if !strings.Contains(reason, "sourdough") {
		t.Fatalf("reason should name the real job, got %q", reason)
	}
	ok, reason = JudgePage(req, `<html><body><h1>Sourdough</h1><p>Track feedings</p></body></html>`)
	if !ok || reason != "" {
		t.Fatalf("real nouns failed without boilerplate words: ok=%v reason=%q", ok, reason)
	}
}

func TestProbeBinaryPostsSavedRow(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "probe-tool")
	script := `#!/usr/bin/env python3
import os
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs
saved = []
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        body = "<html><body><h1>Sourdough</h1><p>Feedings</p><form method=\"post\"><input name=\"note\"></form>"
        if saved:
            body += saved[0]
        body += "</body></html>"
        raw = body.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/html")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)
    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        vals = parse_qs(self.rfile.read(n).decode())
        saved.append(vals.get("note", [""])[0])
        self.send_response(303)
        self.send_header("Location", "/")
        self.end_headers()
    def log_message(self, *args):
        pass
HTTPServer(("127.0.0.1", int(os.environ["PORT"])), H).serve_forever()
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ok, reason, snippet := ProbeBinary(ctx, bin, dir, "track sourdough feedings")
	if !ok {
		t.Fatalf("probe = %v reason=%q snippet=%q", ok, reason, snippet)
	}
}
