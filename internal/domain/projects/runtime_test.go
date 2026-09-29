package projects

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
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
