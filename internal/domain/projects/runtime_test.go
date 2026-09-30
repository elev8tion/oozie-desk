package projects

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLeasePortHandsOutDistinctPorts(t *testing.T) {
	const n = 12
	avoid := map[int]struct{}{8090: {}}
	ports := make([]int, n)
	releases := make([]func(), n)
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			port, release, err := leasePort(avoid)
			if err != nil {
				errCh <- err
				return
			}
			ports[i] = port
			releases[i] = release
		}(i)
	}
	wg.Wait()
	close(errCh)
	t.Cleanup(func() {
		for _, release := range releases {
			if release != nil {
				release()
			}
		}
	})
	for err := range errCh {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, port := range ports {
		if port == 0 || port == 8090 || seen[port] {
			t.Fatalf("port %d reused or forbidden among %v", port, ports)
		}
		seen[port] = true
	}
}

func TestConcurrentStartsGetDistinctPorts(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	s.SetBaseURL("http://127.0.0.1:8090")
	ids := make([]int64, 2)
	for i, name := range []string{"Port One", "Port Two"} {
		dir := t.TempDir()
		bin, err := (fakeBuilder{}).Build(dir, name)
		if err != nil {
			t.Fatal(err)
		}
		workdir := filepath.Join(dir, "proj")
		if err := os.MkdirAll(workdir, 0o755); err != nil {
			t.Fatal(err)
		}
		p, err := s.CreateProject(ctx, name, workdir, true)
		if err != nil {
			t.Fatal(err)
		}
		id, err := s.repo.UpsertStoreApp(ctx, p.ID, PublishDraft{AppName: name, Headline: "h", Description: "d"}, bin, "")
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(ids))
	wg.Add(len(ids))
	for _, id := range ids {
		go func(id int64) {
			defer wg.Done()
			errCh <- s.InstallApp(ctx, id)
		}(id)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	seen := map[string]bool{}
	for _, id := range ids {
		app, err := s.GetStoreApp(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if app.PublicURL == "" || !tcpUp(app.PublicURL) {
			t.Fatalf("app %d not listening at %q", id, app.PublicURL)
		}
		if seen[app.PublicURL] {
			t.Fatalf("two apps share %s", app.PublicURL)
		}
		seen[app.PublicURL] = true
		if portFromURL(app.PublicURL) == 8090 {
			t.Fatalf("app took the factory port: %s", app.PublicURL)
		}
	}
}

func livePIDs(substr string) []int {
	out, err := exec.Command("ps", "-ax", "-o", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, substr) {
			continue
		}
		pidStr, _, _ := strings.Cut(line, " ")
		pid, err := strconv.Atoi(pidStr)
		if err == nil && pid > 1 {
			pids = append(pids, pid)
		}
	}
	return pids
}

func TestConcurrentInstallSameApp(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	dir := t.TempDir()
	bin, err := (fakeBuilder{}).Build(dir, "SameApp")
	if err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(dir, "proj")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProject(ctx, "SameApp", workdir, true)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.repo.UpsertStoreApp(ctx, p.ID, PublishDraft{AppName: "SameApp", Headline: "h", Description: "d"}, bin, "")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = s.InstallApp(ctx, id)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
	}

	app, err := s.GetStoreApp(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	pids := livePIDs(bin)
	if len(pids) != 1 {
		t.Fatalf("live artifact processes = %v, want exactly one", pids)
	}
	if app.RuntimePID != pids[0] {
		t.Fatalf("db pid %d, live pid %d", app.RuntimePID, pids[0])
	}
}

func TestOpenAppRejectsStranger(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	dir := t.TempDir()
	bin, _ := (fakeBuilder{}).Build(dir, "StrangerTest")
	workdir := filepath.Join(dir, "p")
	os.MkdirAll(workdir, 0o755)
	p, _ := s.CreateProject(ctx, "StrangerTest", workdir, true)
	id, _ := s.repo.UpsertStoreApp(ctx, p.ID, PublishDraft{AppName: "StrangerTest", Headline: "h", Description: "d"}, bin, "")

	// start a stranger listener (python)
	lnPort := 34567
	cmd := exec.Command("python3", "-m", "http.server", strconv.Itoa(lnPort), "--bind", "127.0.0.1")
	cmd.Start()
	t.Cleanup(func() { cmd.Process.Kill() })
	time.Sleep(200 * time.Millisecond)

	// set stranger runtime (pid 1 or cmd not contain artifact)
	s.repo.SetRuntime(ctx, id, "http://127.0.0.1:"+strconv.Itoa(lnPort), 1)
	s.repo.InstallApp(ctx, id)

	url, err := s.OpenApp(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(url, strconv.Itoa(lnPort)) {
		t.Fatal("OpenApp returned stranger URL")
	}
	app, _ := s.GetStoreApp(ctx, id)
	if app.PublicURL == "" || strings.Contains(app.PublicURL, strconv.Itoa(lnPort)) {
		t.Fatal("should have started on new port")
	}
}

func TestReclaimDropsDeadApp(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	dir := t.TempDir()
	bin, err := (fakeBuilder{}).Build(dir, "dead")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProject(ctx, "dead", filepath.Join(dir, "proj"), true)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.repo.UpsertStoreApp(ctx, p.ID, PublishDraft{AppName: "dead", Headline: "h", Description: "d"}, bin, "dead")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	dead := cmd.Process.Pid
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if err := s.repo.SetRuntime(ctx, id, "http://127.0.0.1:1234", dead); err != nil {
		t.Fatal(err)
	}
	if err := s.repo.InstallApp(ctx, id); err != nil {
		t.Fatal(err)
	}
	s.ReclaimRuntimes(ctx)
	app, err := s.GetStoreApp(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if app.Installed || app.PublicURL != "" || app.RuntimePID != 0 {
		t.Fatalf("reclaim left %+v", app)
	}
}

func TestReclaimAdoptsLiveApp(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	dir := t.TempDir()
	bin, err := (fakeBuilder{}).Build(dir, "adopt")
	if err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(dir, "p2")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProject(ctx, "adopt", workdir, true)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.repo.UpsertStoreApp(ctx, p.ID, PublishDraft{AppName: "adopt", Headline: "h", Description: "d"}, bin, "adopt")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InstallApp(ctx, id); err != nil {
		t.Fatal(err)
	}
	app, err := s.GetStoreApp(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	pid := app.RuntimePID
	s.procs.Delete(id)
	s.ReclaimRuntimes(ctx)
	app2, err := s.GetStoreApp(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !app2.Installed {
		t.Fatal("reclaim uninstalled a live app")
	}
	s.StopRuntimes()
	deadline := time.Now().Add(2 * time.Second)
	for processCommand(pid) != "" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if processCommand(pid) != "" {
		t.Fatalf("adopted pid %d still alive after StopRuntimes", pid)
	}
}

func TestDefaultPathsDoNotCollide(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	p1, err := s.CreateProject(ctx, "CollideName", "", true)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.CreateProject(ctx, "CollideName", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if p1.ProjectPathDisplay == p2.ProjectPathDisplay {
		t.Fatal("default paths collided")
	}
	if p1.ProjectPathDisplay != "~/Projects/collidename" {
		t.Fatalf("first default wrong: %s", p1.ProjectPathDisplay)
	}
	_ = p1
}

func TestExplicitPathCollision(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	if _, err := s.CreateProject(ctx, "PathCollide", "~/Projects/explicit", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProject(ctx, "PathCollide2", "~/Projects/explicit", true); err == nil {
		t.Fatal("explicit path collision was accepted")
	}
}

func TestSlugStaysUnique(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	s.SetBuilder(fakeBuilder{})
	dir := t.TempDir()
	p1, err := s.CreateProject(ctx, "SlugTest", filepath.Join(dir, "p1"), true)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.CreateProject(ctx, "SlugTest", filepath.Join(dir, "p2"), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(ctx, p1.ID); err != nil {
		t.Fatal(err)
	}
	s.WaitForJobs()
	if err := s.Publish(ctx, p2.ID); err != nil {
		t.Fatal(err)
	}
	s.WaitForJobs()
	id1, err := s.repo.StoreAppIDForProject(ctx, p1.ID)
	if err != nil || id1 == 0 {
		t.Fatal(err)
	}
	id2, err := s.repo.StoreAppIDForProject(ctx, p2.ID)
	if err != nil || id2 == 0 {
		t.Fatal(err)
	}
	a1, err := s.GetStoreApp(ctx, id1)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := s.GetStoreApp(ctx, id2)
	if err != nil {
		t.Fatal(err)
	}
	if a1.BundleSlug == "" || a2.BundleSlug == "" || a1.BundleSlug == a2.BundleSlug {
		t.Fatalf("slugs = %q %q", a1.BundleSlug, a2.BundleSlug)
	}
	old := a1.BundleSlug
	if err := s.Publish(ctx, p1.ID); err != nil {
		t.Fatal(err)
	}
	s.WaitForJobs()
	a1b, err := s.GetStoreApp(ctx, id1)
	if err != nil {
		t.Fatal(err)
	}
	if a1b.BundleSlug != old {
		t.Fatalf("slug changed on republish: %q -> %q", old, a1b.BundleSlug)
	}
}

func TestImproveAutoInstallStartsOnce(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	s.SetBuilder(fakeBuilder{})
	var count int
	s.onInstall = func() { count++ }
	dir := t.TempDir()
	p, err := s.CreateProject(ctx, "ImproveOnce", dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.repo.SaveDraft(ctx, PublishDraft{ProjectID: p.ID, AppName: "ImproveOnce", Headline: "h", Description: "d", AutoInstall: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	s.WaitForJobs()
	if count != 1 {
		t.Fatalf("publish installs = %d, want 1", count)
	}
	appID, err := s.repo.StoreAppIDForProject(ctx, p.ID)
	if err != nil || appID == 0 {
		t.Fatal(err)
	}
	sess, err := s.repo.GetSession(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	reqID, err := s.repo.CreateAgentRequest(ctx, sess.ID, "build", "fix the page")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.repo.InsertImproveRequest(ctx, reqID, appID, "fix"); err != nil {
		t.Fatal(err)
	}
	s.RequestSettled(p.ID, reqID, "completed")
	s.WaitForJobs()
	// The republish starts the new binary once. The callback must not start it again.
	if count != 2 {
		t.Fatalf("installs after improve = %d, want 2 (one publish, one republish)", count)
	}
	imp, err := s.repo.ImproveByRequest(ctx, reqID)
	if err != nil || imp == nil {
		t.Fatal(err)
	}
	if imp.Status != "done" {
		t.Fatalf("improve status = %s, want done", imp.Status)
	}
}

